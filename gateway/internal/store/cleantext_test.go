package store

import (
	"context"
	"testing"
	"time"
	"unicode/utf8"
)

func TestClientTextIsMadeStorable(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		bad := "abc\xd3\xf7\x00def"
		now := time.Now()
		if err := s.InsertRequestLogs(ctx, []RequestLog{{TS: now, RequestID: "r", UserAgent: bad, QueryPreview: bad}}, nil); err != nil {
			t.Fatalf("invalid bytes must not fail the batch: %v", err)
		}
		rows, _ := s.QueryRequestLogs(ctx, LogQuery{})
		if len(rows) != 1 || !utf8.ValidString(rows[0].QueryPreview) || !utf8.ValidString(rows[0].UserAgent) || rows[0].QueryPreview != "abc\uFFFDdef" {
			t.Fatalf("rows = %+v", rows)
		}
		if _, err := s.AppendAudit(ctx, AuditEntry{Source: AuditPanel, Action: "x", Target: bad}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateSession(ctx, Session{IDHash: "h", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour), UserAgent: bad}); err != nil {
			t.Fatal(err)
		}
	})
}
