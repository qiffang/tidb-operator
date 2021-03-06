// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.package spec

package blockwriter

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/pingcap/tidb-operator/tests/pkg/util"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	queryChanSize int = 10000
)

// BlockWriterCase is for concurrent writing blocks.
type BlockWriterCase struct {
	cfg Config
	bws []*blockWriter

	isRunning uint32
	isInit    uint32
	stopChan  chan struct{}

	sync.RWMutex
}

// Config defines the config of BlockWriterCase
type Config struct {
	TableNum    int
	Concurrency int
	BatchSize   int
	RawSize     int
}

type blockWriter struct {
	rawSize   int
	values    []string
	batchSize int
}

// NewBlockWriterCase returns the BlockWriterCase.
func NewBlockWriterCase(cfg Config) *BlockWriterCase {
	c := &BlockWriterCase{
		cfg:      cfg,
		stopChan: make(chan struct{}, 1),
	}

	if c.cfg.TableNum < 1 {
		c.cfg.TableNum = 1
	}
	c.initBlocks()

	return c
}

func (c *BlockWriterCase) initBlocks() {
	c.bws = make([]*blockWriter, c.cfg.Concurrency)
	for i := 0; i < c.cfg.Concurrency; i++ {
		c.bws[i] = c.newBlockWriter()
	}
}

func (c *BlockWriterCase) newBlockWriter() *blockWriter {
	return &blockWriter{
		rawSize:   c.cfg.RawSize,
		values:    make([]string, c.cfg.BatchSize),
		batchSize: c.cfg.BatchSize,
	}
}

func (c *BlockWriterCase) generateQuery(ctx context.Context, queryChan chan []string, wg *sync.WaitGroup) {
	defer func() {
		glog.Infof("[%s] [action: generate Query] stopped", c)
		wg.Done()
	}()

	for {
		tableN := rand.Intn(c.cfg.TableNum)
		var index string
		if tableN > 0 {
			index = fmt.Sprintf("%d", tableN)
		}

		var querys []string
		for i := 0; i < 100; i++ {
			values := make([]string, c.cfg.BatchSize)
			for i := 0; i < c.cfg.BatchSize; i++ {
				blockData := util.RandString(c.cfg.RawSize)
				values[i] = fmt.Sprintf("('%s')", blockData)
			}

			querys = append(querys, fmt.Sprintf(
				"INSERT INTO block_writer%s(raw_bytes) VALUES %s",
				index, strings.Join(values, ",")))
		}

		select {
		case <-ctx.Done():
			return
		default:
			if len(queryChan) < queryChanSize {
				queryChan <- querys
			} else {
				glog.Infof("[%s] [action: generate Query] query channel is full, sleep 10 seconds", c)
				util.Sleep(ctx, 10*time.Second)
			}
		}
	}
}

func (bw *blockWriter) batchExecute(db *sql.DB, query string) error {
	_, err := db.Exec(query)
	if err != nil {
		glog.Errorf("[block_writer] exec sql [%s] failed, err: %v", query, err)
		return err
	}

	return nil
}

func (bw *blockWriter) run(ctx context.Context, db *sql.DB, queryChan chan []string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		querys, ok := <-queryChan
		if !ok {
			// No more query
			return
		}

		for _, query := range querys {
			select {
			case <-ctx.Done():
				return
			default:
				if err := bw.batchExecute(db, query); err != nil {
					glog.Fatal(err)
				}
			}
		}
	}
}

// Initialize inits case
func (c *BlockWriterCase) initialize(db *sql.DB) error {
	glog.Infof("[%s] start to init...", c)
	defer func() {
		atomic.StoreUint32(&c.isInit, 1)
		glog.Infof("[%s] init end...", c)
	}()

	for i := 0; i < c.cfg.TableNum; i++ {
		var s string
		if i > 0 {
			s = fmt.Sprintf("%d", i)
		}

		tmt := fmt.Sprintf("CREATE TABLE IF NOT EXISTS block_writer%s %s", s, `
	(
      id BIGINT NOT NULL AUTO_INCREMENT,
      raw_bytes BLOB NOT NULL,
      PRIMARY KEY (id)
)`)

		err := wait.PollImmediate(5*time.Second, 30*time.Second, func() (bool, error) {
			_, err := db.Exec(tmt)
			if err != nil {
				glog.Warningf("[%s] exec sql [%s] failed, err: %v, retry...", c, tmt, err)
				return false, nil
			}

			return true, nil
		})

		if err != nil {
			glog.Errorf("[%s] exec sql [%s] failed, err: %v", c, tmt, err)
			return err
		}
	}

	return nil
}

// Start starts to run cases
func (c *BlockWriterCase) Start(db *sql.DB) error {
	if !atomic.CompareAndSwapUint32(&c.isRunning, 0, 1) {
		err := fmt.Errorf("[%s] is running, you can't start it again", c)
		glog.Error(err)
		return err
	}

	defer func() {
		c.RLock()
		glog.Infof("[%s] stopped", c)
		atomic.SwapUint32(&c.isRunning, 0)
	}()

	if c.isInit == 0 {
		if err := c.initialize(db); err != nil {
			return err
		}
	}

	glog.Infof("[%s] start to execute case...", c)

	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())

	queryChan := make(chan []string, queryChanSize)

	for i := 0; i < c.cfg.Concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.bws[i].run(ctx, db, queryChan)
		}(i)
	}

	wg.Add(1)
	go c.generateQuery(ctx, queryChan, &wg)

loop:
	for {
		select {
		case <-c.stopChan:
			glog.Infof("[%s] stoping...", c)
			cancel()
			break loop
		default:
			util.Sleep(context.Background(), 2*time.Second)
		}
	}

	wg.Wait()
	close(queryChan)

	return nil
}

// Stop stops cases
func (c *BlockWriterCase) Stop() {
	c.stopChan <- struct{}{}
}

// String implements fmt.Stringer interface.
func (c *BlockWriterCase) String() string {
	return "block_writer"
}
