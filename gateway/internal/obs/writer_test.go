package obs

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/config"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

func TestWriterFlushesOnShutdown(t *testing.T) {
	st, err := store.Open(context.Background(), config.DB{Driver: config.DriverSQLite, SQLiteDir: filepath.Join(t.TempDir(), "db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := NewWriter(st, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	now := time.Now()
	for i := 0; i < batchSize+7; i++ { // crosses one batch boundary, the rest flush at shutdown
		w.Record(store.RequestLog{TS: now, RequestID: "r"}, nil)
	}
	w.Record(store.RequestLog{TS: now, RequestID: "with-attempts", Attempts: 2},
		[]store.Attempt{{RequestID: "with-attempts", N: 1, TS: now}, {RequestID: "with-attempts", N: 2, TS: now}})
	cancel()
	<-done

	rows, err := st.QueryRequestLogs(context.Background(), store.LogQuery{Limit: 1000})
	if err != nil || len(rows) != batchSize+8 {
		t.Fatalf("rows = %d, %v", len(rows), err)
	}
	if at, _ := st.ListAttempts(context.Background(), "with-attempts"); len(at) != 2 {
		t.Fatalf("attempts = %v", at)
	}
}

type blockedStore struct{ release chan struct{} }

func (b *blockedStore) InsertRequestLogs(context.Context, []store.RequestLog, []store.Attempt) error {
	<-b.release
	return nil
}

func TestRecordDropsInsteadOfBlocking(t *testing.T) {
	w := NewWriter(&blockedStore{release: make(chan struct{})}, nil) // Run is not started: nothing drains the queue
	for i := 0; i < queueSize; i++ {
		w.Record(store.RequestLog{}, nil)
	}
	start := time.Now()
	w.Record(store.RequestLog{}, nil)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Record blocked for %v", d)
	}
	if w.Dropped() != 1 {
		t.Fatalf("dropped = %d", w.Dropped())
	}
}
