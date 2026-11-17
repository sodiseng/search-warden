package obs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/store"
)

type flakyStore struct {
	mu   sync.Mutex
	fail int
	got  int
}

func (f *flakyStore) InsertRequestLogs(_ context.Context, logs []store.RequestLog, _ []store.Attempt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return errors.New("the database is restarting")
	}
	f.got += len(logs)
	return nil
}

func (f *flakyStore) rows() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got
}

// Found in acceptance (2026-11-10): a request served while PostgreSQL was
// restarting never reached the request log.
func TestFailedBatchIsRetried(t *testing.T) {
	st := &flakyStore{fail: 2}
	w := NewWriter(st, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	for i := 0; i < 3; i++ {
		w.Record(store.RequestLog{TS: time.Now(), RequestID: "r"}, nil)
	}
	deadline := time.Now().Add(10 * time.Second)
	for st.rows() != 3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	if st.rows() != 3 || w.Dropped() != 0 {
		t.Fatalf("rows written = %d (want 3), dropped = %d", st.rows(), w.Dropped())
	}
}
