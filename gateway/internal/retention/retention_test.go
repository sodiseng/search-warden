package retention

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/config"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), config.DB{Driver: config.DriverSQLite, SQLiteDir: filepath.Join(t.TempDir(), "db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func snapshot(retentionDays, maxMB int) func() *confstore.Snapshot {
	s := &confstore.Snapshot{Config: confstore.Config{Logs: confstore.LogsConfig{RetentionDays: retentionDays, MaxSizeMB: maxMB}}}
	return func() *confstore.Snapshot { return s }
}

// fill writes n logs, one per minute ending at end, each with ~1 KB of preview.
func fill(t *testing.T, st *store.Store, end time.Time, n int) {
	t.Helper()
	pad := strings.Repeat("x", 1000)
	var batch []store.RequestLog
	for i := 0; i < n; i++ {
		batch = append(batch, store.RequestLog{TS: end.Add(-time.Duration(n-1-i) * time.Minute), RequestID: "r", Provider: "tavily",
			Operation: "search", Status: 200, QueryPreview: pad})
		if len(batch) == 500 || i == n-1 {
			if err := st.InsertRequestLogs(context.Background(), batch, nil); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
}

func TestPurgeByAgeAndRemovedKeys(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if err := st.InsertRequestLogs(ctx, []store.RequestLog{
		{TS: now.Add(-31 * 24 * time.Hour), RequestID: "old", Status: 200},
		{TS: now.Add(-29 * 24 * time.Hour), RequestID: "kept", Status: 200},
	}, []store.Attempt{{RequestID: "old", N: 1, TS: now.Add(-31 * 24 * time.Hour)}, {RequestID: "old", N: 2, TS: now.Add(-31 * 24 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertKeyStates(ctx, []store.KeyState{{KeyID: "gone", Provider: "exa", Status: "active"}, {KeyID: "recent", Provider: "exa", Status: "active"}}); err != nil {
		t.Fatal(err)
	}
	_ = st.MarkKeysRemoved(ctx, []string{"gone"}, now.Add(-31*24*time.Hour))
	_ = st.MarkKeysRemoved(ctx, []string{"recent"}, now.Add(-24*time.Hour))

	j := New(Options{Store: st, Snapshot: snapshot(30, 1024), Now: func() time.Time { return now }})
	s := j.Pass(ctx)
	if s.AgeDeleted != 3 || s.Rows != 1 || s.KeysPurged != 1 || s.Err != "" || !s.Oldest.Equal(now.Add(-29*24*time.Hour)) {
		t.Fatalf("status = %+v", s) // 1 log + its 2 attempts
	}
	if got := j.Status(); !got.At.Equal(now) || got.SizeBytes <= 0 {
		t.Fatalf("last status = %+v", got)
	}
	if ks, _ := st.ListKeyStates(ctx, "exa"); len(ks) != 1 || ks[0].KeyID != "recent" {
		t.Fatalf("key states = %+v", ks)
	}
}

func TestSizeCapTrimsOldestFirst(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	fill(t, st, now, 6000) // ~6+ MB
	before, err := st.LogsSize(ctx)
	if err != nil || before < 5<<20 {
		t.Fatalf("size = %d %v", before, err)
	}
	maxMB := int(before>>20) / 2 // the cap is half of what is stored
	j := New(Options{Store: st, Snapshot: snapshot(30, maxMB), Now: func() time.Time { return now }})
	s := j.Pass(ctx)
	if s.SizeDeleted < 3000 || s.SizeDeleted > 4000 || s.AgeDeleted != 0 {
		t.Fatalf("deleted %d for size (status %+v)", s.SizeDeleted, s)
	}
	// incremental_vacuum gave the space back: the logs fit under the cap.
	if s.SizeBytes > int64(maxMB)<<20 {
		t.Fatalf("size after trim = %d MB, cap %d MB", s.SizeBytes>>20, maxMB)
	}
	// The newest logs survive.
	rows, _ := st.QueryRequestLogs(ctx, store.LogQuery{Limit: 1})
	if len(rows) != 1 || !rows[0].TS.Equal(now) {
		t.Fatalf("newest = %+v", rows)
	}
	if s.Oldest.Before(now.Add(-time.Duration(6000-s.SizeDeleted) * time.Minute)) {
		t.Fatalf("oldest left = %v", s.Oldest)
	}
	// Under the cap: nothing more to do.
	if s2 := j.Pass(ctx); s2.SizeDeleted != 0 {
		t.Fatalf("second pass deleted %d", s2.SizeDeleted)
	}
}

func TestRunStops(t *testing.T) {
	st := openStore(t)
	j := New(Options{Store: st, Snapshot: snapshot(30, 1024), Delay: -1, Interval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { j.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for j.Status().At.IsZero() {
		select {
		case <-deadline:
			t.Fatal("no pass ran")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
