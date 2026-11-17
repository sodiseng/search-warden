package store

import (
	"context"
	"testing"
	"time"
)

func TestTokenUsage(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		rows := []TokenUsage{{TokenID: "t1", Period: "d2026-11-10", Requests: 3}, {TokenID: "t1", Period: "m2026-11", Requests: 9, Credits: 4, MicroUSD: 7000}}
		if err := s.UpsertTokenUsage(ctx, rows); err != nil {
			t.Fatal(err)
		}
		rows[1].Requests = 10
		if err := s.UpsertTokenUsage(ctx, rows[1:]); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListTokenUsage(ctx, "d2026-11-10", "m2026-11", "m2026-10")
		if err != nil || len(got) != 2 || got[1].Requests != 10 || got[1].MicroUSD != 7000 {
			t.Fatalf("usage = %+v %v", got, err)
		}
		if n, err := s.PruneTokenUsage(ctx, time.Now().Add(time.Hour)); err != nil || n != 2 {
			t.Fatalf("prune = %d %v", n, err)
		}
	})
}
