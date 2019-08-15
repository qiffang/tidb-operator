# Hack, instruct terraform that the kubeconfig_filename is only available until the k8s created
data "template_file" "kubeconfig_filename" {
  template = var.kubeconfig_file
  vars = {
    kubernetes_depedency = alicloud_cs_managed_kubernetes.k8s.client_cert
  }
}

provider "helm" {
  alias = "initial"
  insecure = true
  install_tiller = false
  kubernetes {
    config_path = data.template_file.kubeconfig_filename.rendered
  }
}


resource "null_resource" "setup-env" {
  depends_on = [data.template_file.kubeconfig_filename]

  provisioner "local-exec" {
    working_dir = path.cwd
    # Note for the patch command: ACK has a toleration issue with the pre-deployed flexvolume daemonset, we have to patch
    # it manually and the resource namespace & name are hard-coded by convention
    command     = <<EOS
kubectl apply -f https://raw.githubusercontent.com/pingcap/tidb-operator/${var.operator_version}/manifests/crd.yaml
kubectl apply -f ${path.module}/manifest/alicloud-disk-storageclass.yaml
echo '${data.template_file.local-volume-provisioner.rendered}' | kubectl apply -f -
kubectl patch -n kube-system daemonset flexvolume --type='json' -p='[{"op":"replace", "path": "/spec/template/spec/tolerations", "value":[{"operator": "Exists"}]}]'

# Kubernetes cluster monitor
wget https://raw.githubusercontent.com/pingcap/monitoring/master/k8s-cluster-monitor/manifests/archive/prometheus-operator.tar.gz
wget https://raw.githubusercontent.com/pingcap/monitoring/master/k8s-cluster-monitor/manifests/archive/prometheus.tar.gz
mkdir monitor
tar -zxvf prometheus-operator.tar.gz -C monitor/
tar -zxvf prometheus.tar.gz -C monitor/
kubectl apply -f monitor/manifests/prometheus-operator
kubectl apply -f monitor/manifests/prometheus

helm init
until helm ls; do
  echo "Wait tiller ready"
done
EOS
    environment = {
      KUBECONFIG = data.template_file.kubeconfig_filename.rendered
    }
  }
}

data "helm_repository" "pingcap" {
  provider = "helm.initial"
  depends_on = ["null_resource.setup-env"]
  name = "pingcap"
  url = "http://charts.pingcap.org/"
}

resource "helm_release" "tidb-operator" {
  provider = "helm.initial"
  depends_on = ["null_resource.setup-env"]

  repository = data.helm_repository.pingcap.name
  chart = "tidb-operator"
  version = var.operator_version
  namespace = "tidb-admin"
  name = "tidb-operator"
  values = [var.operator_helm_values]

  set {
    name = "scheduler.kubeSchedulerImageName"
    value = "gcr.akscn.io/google_containers/hyperkube"
  }
}