// Package obs records what happened to requests (ARCHITECTURE.md §13): logs
// are queued and written in batches by one goroutine, so the request path
// never waits on the database.
package obs

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/store"
)

const (
	queueSize   = 5000
	batchSize   = 200
	flushEvery  = 500 * time.Millisecond
	enqueueWait = 50 * time.Millisecond
)

// Store is the subset of *store.Store used here.
type Store interface {
	InsertRequestLogs(ctx context.Context, logs []store.RequestLog, attempts []store.Attempt) error
}

type record struct {
	log      store.RequestLog
	attempts []store.Attempt
}

// Writer is safe for concurrent use.
type Writer struct {
	st      Store
	log     *slog.Logger
	ch      chan record
	dropped atomic.Int64
}

// NewWriter creates a Writer; start it with Run.
func NewWriter(st Store, log *slog.Logger) *Writer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Writer{st: st, log: log, ch: make(chan record, queueSize)}
}

// Record enqueues one request log. If the queue stays full, the entry is
// dropped and counted: losing a log line must never slow down a request.
func (w *Writer) Record(l store.RequestLog, attempts []store.Attempt) {
	r := record{l, attempts}
	select {
	case w.ch <- r:
		return
	default:
	}
	t := time.NewTimer(enqueueWait)
	defer t.Stop()
	select {
	case w.ch <- r:
	case <-t.C:
		w.dropped.Add(1)
	}
}

// Dropped is how many log entries were lost to back-pressure.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

// Run writes batches until ctx is done, then drains the queue.
func (w *Writer) Run(ctx context.Context) {
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	var logs []store.RequestLog
	var attempts []store.Attempt
	failing := false
	add := func(r record) {
		logs = append(logs, r.log)
		attempts = append(attempts, r.attempts...)
	}
	flush := func() {
		if len(logs) == 0 && len(attempts) == 0 {
			return
		}
		fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := w.st.InsertRequestLogs(fctx, logs, attempts); err != nil {
			// Keep the batch for the next flush (the database may be restarting);
			// beyond one queue's worth the oldest rows are given up and counted.
			if over := len(logs) - queueSize; over > 0 {
				logs = logs[over:]
				attempts = nil
				w.dropped.Add(int64(over))
			}
			if !failing {
				w.log.Error("request log write failed; will retry", "rows", len(logs), "err", err)
			}
			failing = true
			return
		}
		if failing {
			w.log.Info("request log writes recovered", "rows", len(logs))
			failing = false
		}
		logs, attempts = nil, nil
	}
	for {
		select {
		case r := <-w.ch:
			add(r)
			if len(logs) >= batchSize {
				flush()
			}
		case <-tick.C:
			flush()
		case <-ctx.Done():
			for {
				select {
				case r := <-w.ch:
					add(r)
				default:
					flush()
					return
				}
			}
		}
	}
}
