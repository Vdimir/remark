package search

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/gammazero/deque"
	log "github.com/go-pkgz/lgr"
	"github.com/pkg/errors"
)

type idxFlusher struct {
	notifier chan error
}

// bufferedEngine provides common functionality around searchEngine,
// like buffering, startup/shotdown handling etc
type bufferedEngine struct {
	queueLock     sync.RWMutex
	docQueue      deque.Deque
	queueNotifier chan bool
	shutdownWait  sync.WaitGroup
	index         searchEngine
	flushEvery    time.Duration
	flushCount    int
	indexPath     string
}

// IndexDocument adds or updates document to search index
func (s *bufferedEngine) IndexDocument(doc *DocumentComment) error {
	s.queueLock.Lock()
	s.docQueue.PushBack(doc)
	s.queueLock.Unlock()
	s.queueNotifier <- false
	return nil
}

func (s *bufferedEngine) indexBatch() {
	s.queueLock.Lock()

	docCount := s.docQueue.Len()
	if docCount == 0 {
		s.queueLock.Unlock()
		return
	}

	batch := s.index.NewBatch()
	for i := 0; i < docCount; i++ {
		switch val := s.docQueue.PopFront().(type) {
		case *DocumentComment:
			err := batch.Index(val.ID, val)
			if err != nil {
				log.Printf("[ERROR] error while adding doc %q to batch %v", val.ID, err)
				break
			}
		case *idxFlusher:
			defer func() { val.notifier <- nil }()
		default:
			s.queueLock.Unlock()
			panic(fmt.Sprintf("unknown type %T", val))
		}
	}

	s.queueLock.Unlock()

	err := s.index.Batch(batch)
	if err != nil {
		log.Printf("[ERROR] error while indexing batch, %v", err)
	}
}

func (s *bufferedEngine) indexDocumentWorker() {
	log.Printf("[INFO] start bleve indexer worker")
	s.shutdownWait.Add(1)
	defer s.shutdownWait.Done()

	tmr := time.NewTimer(s.flushEvery)
	cont := true
	for cont {
		var force bool
		select {
		case <-tmr.C:
			s.indexBatch()
			tmr.Reset(s.flushEvery)
		case force, cont = <-s.queueNotifier:
			s.queueLock.RLock()
			full := s.docQueue.Len() >= s.flushCount
			s.queueLock.RUnlock()
			if force || full {
				s.indexBatch()
			}
		}
	}
	log.Printf("[INFO] shutdown bleve indexer worker")

	s.writeAheadLog()
}

func (s *bufferedEngine) getAheadLogPath() string {
	return path.Join(s.indexPath, aheadLogFname)
}

func (s *bufferedEngine) writeAheadLog() {
	var err error

	aheadLogPath := s.getAheadLogPath()
	if _, errOpen := os.Stat(aheadLogPath); !os.IsNotExist(errOpen) {
		log.Printf("[ERROR] file %q already exists and would be rewritten", aheadLogPath)
	}

	f, err := os.Create(filepath.Clean(aheadLogPath))
	if err != nil {
		log.Printf("[ERROR] error %v opening log file %q", err, aheadLogPath)
		return
	}
	defer func() {
		errClose := f.Close()
		if errClose != nil {
			log.Printf("[ERROR] error %v closing log file %q", errClose, aheadLogPath)
		}
	}()

	s.queueLock.Lock()
	defer s.queueLock.Unlock()
	for s.docQueue.Len() > 0 {
		switch val := s.docQueue.PopFront().(type) {
		case *DocumentComment:
			if err != nil {
				continue
			}
			var data []byte
			data, err = json.Marshal(val)
			if err != nil {
				continue
			}
			data = append(data, 0x0)
			_, err = f.Write(data)
		case *idxFlusher:
			defer func() { val.notifier <- errors.Errorf("indexer closing") }()
		default:
			panic(fmt.Sprintf("unknown type %T", val))
		}
	}
	if err != nil {
		log.Printf("[ERROR] error %v writing log file", err)
	}
}

// Init engine. It loads unindexed comments from ahead log saved from buffer on shutdown
// Return true if engine initalizated before, false means cold start
func (s *bufferedEngine) Init(ctx context.Context) (bool, error) {
	// TODO(@vdimir) add tests for this part

	aheadLogPath := s.getAheadLogPath()
	f, err := os.Open(filepath.Clean(aheadLogPath))

	if os.IsNotExist(err) {
		log.Printf("[INFO] log file %q does not exists", aheadLogPath)
		return false, nil
	}
	if err != nil {
		return false, err
	}

	defer func() {
		err = f.Close()
		if err != nil {
			log.Printf("[ERROR] error %v closing log file %q", err, aheadLogPath)
		}
	}()

	reader := bufio.NewReader(f)
	err = s.readAheadLog(ctx, reader)
	if err == nil {
		defer func() {
			err = os.Remove(aheadLogPath)
			if err != nil {
				log.Printf("[ERROR] error %v deleting log file %q", err, aheadLogPath)
			}
		}()
	}

	return true, err
}

func (s *bufferedEngine) readAheadLog(ctx context.Context, reader *bufio.Reader) error {
	for {
		select {
		case <-ctx.Done():
			return errors.Errorf("reading ahead log interrupted")
		default:
		}

		data, err := reader.ReadBytes(0x0)
		if err != nil {
			for err == io.EOF {
				return nil
			}
			return err
		}
		data = data[:len(data)-1]
		var doc *DocumentComment
		if err = json.Unmarshal(data, doc); err == nil {
			err = s.IndexDocument(doc)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}
}

// Flush documents buffer
func (s *bufferedEngine) Flush() error {
	flusher := &idxFlusher{make(chan error)}

	s.queueLock.Lock()
	s.docQueue.PushBack(flusher)
	s.queueLock.Unlock()

	s.queueNotifier <- true

	return <-flusher.notifier
}

// Search documents
func (s *bufferedEngine) Search(req *Request) (*ResultPage, error) {
	log.Printf("[INFO] searching %v", req)
	return s.index.Search(req)
}

// Delete comment from index
func (s *bufferedEngine) Delete(commentID string) error {
	if err := s.index.Delete(commentID); err != nil {
		return errors.Wrapf(err, "cannot detele comment %q from search index", commentID)
	}
	return nil
}

// Close search service
func (s *bufferedEngine) Close() error {
	close(s.queueNotifier)
	err := s.index.Close()

	s.shutdownWait.Wait()
	return err
}

func validateSortField(sortBy string, possible ...string) bool {
	if sortBy == "" {
		return false
	}
	if sortBy[0] == '-' || sortBy[0] == '+' {
		sortBy = sortBy[1:]
	}
	for _, e := range possible {
		if sortBy == e {
			return true
		}
	}
	return false
}
