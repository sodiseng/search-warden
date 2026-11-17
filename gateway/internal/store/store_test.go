package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/config"
)

// WARDEN_TEST_PG_DSN enables the PostgreSQL leg (CI runs it with a postgres service).
// Each test gets its own schema so runs never interfere.
const pgEnv = "WARDEN_TEST_PG_DSN"

type backend struct {
	name string
	open func(t *testing.T) *Store
}

func backends(t *testing.T) []backend {
	t.Helper()
	b := []backend{{"sqlite", openTestSQLite}}
	if os.Getenv(pgEnv) != "" {
		b = append(b, backend{"postgres", openTestPG})
	}
	return b
}

func openTestSQLite(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), config.DB{Driver: config.DriverSQLite, SQLiteDir: filepath.Join(t.TempDir(), "db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openTestPG(t *testing.T) *Store {
	t.Helper()
	base := os.Getenv(pgEnv)
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	schema := "warden_test_" + hex.EncodeToString(rnd[:])

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	s, err := Open(context.Background(), config.DB{Driver: config.DriverPostgres, DSN: base + sep + "search_path=" + schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func eachBackend(t *testing.T, fn func(t *testing.T, s *Store)) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) { fn(t, b.open(t)) })
	}
}

func TestOpenIsIdempotentAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	for i := 0; i < 2; i++ { // second open re-runs migrations as a no-op
		s, err := Open(context.Background(), config.DB{Driver: config.DriverSQLite, SQLiteDir: dir})
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		if err := s.Ping(context.Background()); err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}
	for _, f := range []string{"state.db", "logs.db"} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", f, fi.Mode().Perm())
		}
	}
}

func TestSQLitePragmas(t *testing.T) {
	s := openTestSQLite(t)
	var mode string
	if err := s.state.w.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v", mode, err)
	}
	var av int
	if err := s.logs.w.QueryRow(`PRAGMA auto_vacuum`).Scan(&av); err != nil || av != 2 {
		t.Errorf("logs.db auto_vacuum = %d (want 2 = incremental), %v", av, err)
	}
	// Reader pool must be read-only.
	if _, err := s.state.r.Exec(`DELETE FROM admin_sessions`); err == nil {
		t.Error("reader pool accepted a write")
	}
}

func TestRebind(t *testing.T) {
	pg := &Store{driver: config.DriverPostgres}
	if got := pg.rebind("a = ? AND b IN (?,?)"); got != "a = $1 AND b IN ($2,$3)" {
		t.Errorf("rebind = %q", got)
	}
	sq := &Store{driver: config.DriverSQLite}
	if got := sq.rebind("a = ?"); got != "a = ?" {
		t.Errorf("sqlite rebind changed query: %q", got)
	}
}

func TestSessions(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Now().Truncate(time.Millisecond)
		live := Session{IDHash: "h1", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour), IP: "198.51.100.1", UserAgent: "ua"}
		dead := Session{IDHash: "h2", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(-time.Second)}
		for _, x := range []Session{live, dead} {
			if err := s.CreateSession(ctx, x); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.GetSession(ctx, "h1", now)
		if err != nil || got.IP != live.IP || !got.ExpiresAt.Equal(live.ExpiresAt) {
			t.Fatalf("GetSession = %+v, %v", got, err)
		}
		if _, err := s.GetSession(ctx, "h2", now); err != ErrNotFound {
			t.Errorf("expired session: %v", err)
		}
		if err := s.TouchSession(ctx, "h1", now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		got, _ = s.GetSession(ctx, "h1", now)
		if !got.LastSeenAt.Equal(now.Add(time.Minute)) {
			t.Errorf("touch not applied: %v", got.LastSeenAt)
		}
		list, _ := s.ListSessions(ctx, now)
		if len(list) != 1 {
			t.Errorf("ListSessions = %d", len(list))
		}
		n, err := s.DeleteExpiredSessions(ctx, now, now.Add(-time.Hour))
		if err != nil || n != 1 {
			t.Errorf("DeleteExpiredSessions = %d, %v", n, err)
		}
		n, _ = s.DeleteAllSessions(ctx)
		if n != 1 {
			t.Errorf("DeleteAllSessions = %d", n)
		}
	})
}

func TestAudit(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		for i, a := range []string{"key.add", "key.update", "egress.import", "settings.update"} {
			src := AuditPanel
			if i%2 == 1 {
				src = AuditFile
			}
			if _, err := s.AppendAudit(ctx, AuditEntry{Source: src, Action: a, Target: "t", Diff: "d"}); err != nil {
				t.Fatal(err)
			}
		}
		all, _ := s.ListAudit(ctx, AuditQuery{})
		if len(all) != 4 || all[0].Action != "settings.update" || all[0].ID <= all[1].ID {
			t.Fatalf("ListAudit order: %+v", all)
		}
		keys, _ := s.ListAudit(ctx, AuditQuery{Action: "key."})
		if len(keys) != 2 {
			t.Errorf("prefix filter: %d", len(keys))
		}
		files, _ := s.ListAudit(ctx, AuditQuery{Source: AuditFile})
		if len(files) != 2 {
			t.Errorf("source filter: %d", len(files))
		}
		page, _ := s.ListAudit(ctx, AuditQuery{BeforeID: all[1].ID, Limit: 1})
		if len(page) != 1 || page[0].ID != all[2].ID {
			t.Errorf("paging: %+v", page)
		}
		// LIKE wildcards in the filter must not widen the match.
		if wild, err := s.ListAudit(ctx, AuditQuery{Action: "%"}); err != nil || len(wild) != 0 {
			t.Errorf("'%%' must match literally, not as a wildcard: %d %v", len(wild), err)
		}
		if _, err := s.AppendAudit(ctx, AuditEntry{Source: AuditPanel, Action: "admin.login_failed"}); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.ListAudit(ctx, AuditQuery{Action: "admin.login_"}); len(got) != 1 {
			t.Errorf("underscore prefix: %d", len(got))
		}
		if got, _ := s.ListAudit(ctx, AuditQuery{Action: "admin.loginX"}); len(got) != 0 {
			t.Errorf("'_' must not act as a single-char wildcard: %d", len(got))
		}
	})
}

func i64(v int64) *int64 { return &v }

func TestKeyState(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Now().Truncate(time.Millisecond)
		states := []KeyState{
			{KeyID: "k1", Provider: "tavily", Status: "active", QuotaLimit: i64(1000), QuotaUsed: i64(10), QuotaRemaining: i64(990), QuotaUnit: "credits", LastUsedAt: now,
				QuotaPlan: "Researcher", SyncError: "rate limited (429)", NextSyncAt: now.Add(5 * time.Minute)},
			{KeyID: "k2", Provider: "exa", Status: "cooldown", StatusUntil: now.Add(time.Minute), StatusReason: "429", FailScore: 1.5},
		}
		if err := s.UpsertKeyStates(ctx, states); err != nil {
			t.Fatal(err)
		}
		states[0].Status, states[0].QuotaRemaining = "exhausted", i64(0)
		if err := s.UpsertKeyStates(ctx, states[:1]); err != nil {
			t.Fatal(err)
		}
		all, err := s.ListKeyStates(ctx, "")
		if err != nil || len(all) != 2 {
			t.Fatalf("ListKeyStates = %v, %v", all, err)
		}
		k1 := all[0]
		if k1.Status != "exhausted" || *k1.QuotaRemaining != 0 || *k1.QuotaLimit != 1000 || !k1.LastUsedAt.Equal(now) ||
			k1.QuotaPlan != "Researcher" || k1.SyncError != "rate limited (429)" || !k1.NextSyncAt.Equal(now.Add(5*time.Minute)) {
			t.Errorf("k1 = %+v", k1)
		}
		k2 := all[1]
		if k2.QuotaLimit != nil || k2.FailScore != 1.5 || !k2.StatusUntil.Equal(now.Add(time.Minute)) || k2.QuotaPlan != "" || !k2.NextSyncAt.IsZero() {
			t.Errorf("k2 = %+v", k2)
		}
		exa, _ := s.ListKeyStates(ctx, "exa")
		if len(exa) != 1 {
			t.Errorf("provider filter: %d", len(exa))
		}

		old := now.Add(-31 * 24 * time.Hour)
		if err := s.MarkKeysRemoved(ctx, []string{"k1", "k2"}, old); err != nil {
			t.Fatal(err)
		}
		if err := s.RestoreKeys(ctx, []string{"k2"}); err != nil {
			t.Fatal(err)
		}
		n, err := s.PurgeRemovedKeys(ctx, now.Add(-30*24*time.Hour))
		if err != nil || n != 1 {
			t.Fatalf("PurgeRemovedKeys = %d, %v", n, err)
		}
		left, _ := s.ListKeyStates(ctx, "")
		if len(left) != 1 || left[0].KeyID != "k2" || !left[0].RemovedAt.IsZero() {
			t.Errorf("after purge: %+v", left)
		}
	})
}

func TestEgressState(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		err := s.UpsertEgressStates(ctx, []EgressState{
			{EgressID: "direct", ExitIP: "203.0.113.1", Health: "healthy"},
			{EgressID: "eabc", ExitIP: "198.51.100.2", ExitCountry: "HK", LatencyMS: 120, FailStreak: 2, Health: "degraded"},
			{EgressID: "egone"},
		})
		if err != nil {
			t.Fatal(err)
		}
		m, err := s.ListEgressStates(ctx)
		if err != nil || len(m) != 3 || m["eabc"].ExitCountry != "HK" || m["egone"].Health != "unknown" {
			t.Fatalf("ListEgressStates = %+v, %v", m, err)
		}
		n, err := s.DeleteEgressStatesExcept(ctx, []string{"direct", "eabc"})
		if err != nil || n != 1 {
			t.Errorf("DeleteEgressStatesExcept = %d, %v", n, err)
		}
	})
}

func TestRequestLogs(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Now().Truncate(time.Millisecond)
		long := strings.Repeat("ü", 100) // multi-byte: must not be split
		logs := []RequestLog{
			{TS: now.Add(-40 * 24 * time.Hour), RequestID: "old", Provider: "tavily", Status: 200},
			{TS: now.Add(-2 * time.Minute), RequestID: "r1", ClientIP: "198.51.100.7", Provider: "tavily", Surface: "mcp",
				Operation: "tavily_search", KeyID: "k1", TokenID: "t1", Status: 200, CostCredits: 2, UserAgent: long, QueryPreview: "golang 并发"},
			{TS: now.Add(-time.Minute), RequestID: "r2", Provider: "exa", Operation: "web_search_exa", KeyID: "k2", Status: 429,
				ErrorKind: "rate_limited", Attempts: 3, CostMicroUSD: 7000, CostEstimated: true},
		}
		attempts := []Attempt{
			{RequestID: "r2", N: 1, TS: now, KeyID: "k3", Status: 429},
			{RequestID: "r2", N: 2, TS: now, KeyID: "k4", Status: 429},
			{RequestID: "old", N: 1, TS: now.Add(-40 * 24 * time.Hour), Status: 500},
		}
		if err := s.InsertRequestLogs(ctx, logs, attempts); err != nil {
			t.Fatal(err)
		}
		// Duplicate attempts are ignored (writer retries must be safe).
		if err := s.InsertRequestLogs(ctx, nil, attempts[:1]); err != nil {
			t.Fatal(err)
		}

		recent, err := s.QueryRequestLogs(ctx, LogQuery{From: now.Add(-time.Hour)})
		if err != nil || len(recent) != 2 || recent[0].RequestID != "r2" {
			t.Fatalf("recent = %+v, %v", recent, err)
		}
		r2, r1 := recent[0], recent[1]
		if !r2.CostEstimated || r2.CostMicroUSD != 7000 || r2.Attempts != 3 {
			t.Errorf("r2 = %+v", r2)
		}
		if len([]rune(r1.UserAgent)) != MaxUserAgentLen || r1.QueryPreview != "golang 并发" || r1.ClientIP != "198.51.100.7" {
			t.Errorf("r1 = %+v", r1)
		}
		if errs, _ := s.QueryRequestLogs(ctx, LogQuery{ErrorOnly: true}); len(errs) != 1 || errs[0].RequestID != "r2" {
			t.Errorf("ErrorOnly = %+v", errs)
		}
		if byKey, _ := s.QueryRequestLogs(ctx, LogQuery{KeyID: "k1", TokenID: "t1", Provider: "tavily"}); len(byKey) != 1 {
			t.Errorf("filters = %+v", byKey)
		}
		if page, _ := s.QueryRequestLogs(ctx, LogQuery{BeforeID: r2.ID, Limit: 1}); len(page) != 1 || page[0].RequestID != "r1" {
			t.Errorf("paging = %+v", page)
		}
		at, err := s.ListAttempts(ctx, "r2")
		if err != nil || len(at) != 2 || at[1].KeyID != "k4" {
			t.Errorf("attempts = %+v, %v", at, err)
		}

		n, err := s.PurgeRequestLogsBefore(ctx, now.Add(-30*24*time.Hour))
		if err != nil || n != 2 { // one log + one attempt
			t.Fatalf("purge = %d, %v", n, err)
		}
		if left, _ := s.QueryRequestLogs(ctx, LogQuery{}); len(left) != 2 {
			t.Errorf("after purge: %d rows", len(left))
		}
	})
}

func TestConcurrentWritersSerialize(t *testing.T) {
	s := openTestSQLite(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- s.InsertRequestLogs(ctx, []RequestLog{{TS: time.Now(), RequestID: "x"}}, nil)
		}()
		go func() {
			defer wg.Done()
			_, err := s.AppendAudit(ctx, AuditEntry{Source: AuditSystem, Action: "x"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write failed (SQLITE_BUSY?): %v", err)
		}
	}
	if rows, _ := s.QueryRequestLogs(ctx, LogQuery{Limit: 1000}); len(rows) != 20 {
		t.Errorf("rows = %d", len(rows))
	}
}

func TestTruncateRunes(t *testing.T) {
	if truncateRunes("héllo", 2) != "hé" || truncateRunes("ab", 5) != "ab" {
		t.Error("truncateRunes")
	}
}

func TestPartitionNamesForPG(t *testing.T) {
	if os.Getenv(pgEnv) == "" {
		t.Skip(pgEnv + " not set")
	}
	s := openTestPG(t)
	ctx := context.Background()
	old := time.Now().AddDate(0, 0, -40)
	// Simulate an old partition, insert into it, then purge.
	from := dayStartUTC(old)
	name := "request_logs_p" + from.Format("20060102")
	if _, err := s.logs.w.Exec(`CREATE TABLE ` + name + ` PARTITION OF request_logs FOR VALUES FROM (` +
		itoa(from.UnixMilli()) + `) TO (` + itoa(from.AddDate(0, 0, 1).UnixMilli()) + `)`); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRequestLogs(ctx, []RequestLog{{TS: old, RequestID: "p"}}, nil); err != nil {
		t.Fatal(err)
	}
	n, err := s.PurgeRequestLogsBefore(ctx, time.Now().AddDate(0, 0, -30))
	if err != nil || n != 1 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	var exists bool
	_ = s.logs.r.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists)
	if exists {
		t.Error("expired partition not dropped")
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
