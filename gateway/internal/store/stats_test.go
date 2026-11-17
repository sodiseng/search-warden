package store

import (
	"context"
	"testing"
	"time"
)

func TestDashboardQueries(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
		var logs []RequestLog
		add := func(min int, provider, op, key, tok, ip string, status, latency int, credits, usd int64, kind string) {
			logs = append(logs, RequestLog{TS: base.Add(time.Duration(min) * time.Minute), RequestID: "r", Provider: provider, Operation: op,
				KeyID: key, TokenID: tok, ClientIP: ip, Status: status, LatencyMS: latency, CostCredits: credits, CostMicroUSD: usd, ErrorKind: kind, Outcome: "ok"})
		}
		add(0, "tavily", "search", "k1", "t1", "10.0.0.1", 200, 100, 1, 0, "")
		add(1, "tavily", "search", "k1", "t1", "10.0.0.1", 200, 300, 2, 0, "")
		add(2, "tavily", "extract", "k2", "t2", "10.0.0.2", 429, 50, 0, 0, "rate_limited")
		add(61, "exa", "search", "k3", "t1", "10.0.0.1", 200, 900, 0, 7000, "")
		add(62, "exa", "contents", "", "t1", "10.0.0.1", 503, 10, 0, 0, "no_keys")
		add(200, "exa", "search", "k3", "t1", "10.0.0.1", 200, 1, 0, 7000, "") // outside the range below
		if err := s.InsertRequestLogs(ctx, logs, nil); err != nil {
			t.Fatal(err)
		}
		from, to := base, base.Add(2*time.Hour)

		tot, err := s.Totals(ctx, from, to)
		if err != nil || tot.Requests != 5 || tot.Errors != 2 || tot.Credits != 3 || tot.MicroUSD != 7000 || tot.P50 != 100 || tot.P95 != 300 {
			t.Fatalf("totals = %+v %v", tot, err)
		}
		if empty, err := s.Totals(ctx, base.Add(-time.Hour), base); err != nil || empty.Requests != 0 || empty.P95 != 0 {
			t.Fatalf("empty totals = %+v %v", empty, err)
		}

		series, err := s.Series(ctx, from, to, time.Hour)
		if err != nil || len(series) != 2 {
			t.Fatalf("series = %+v %v", series, err)
		}
		if p := series[0]; !p.Bucket.Equal(base) || p.Provider != "tavily" || p.Requests != 3 || p.Errors != 1 {
			t.Fatalf("first bucket = %+v", p)
		}
		if p := series[1]; !p.Bucket.Equal(base.Add(time.Hour)) || p.Provider != "exa" || p.Requests != 2 || p.Errors != 1 {
			t.Fatalf("second bucket = %+v", p)
		}

		tools, _ := s.Top(ctx, from, to, ByTool, 10)
		if len(tools) != 4 || tools[0].Value != "tavily/search" || tools[0].Requests != 2 || tools[0].Credits != 3 {
			t.Fatalf("tools = %+v", tools)
		}
		keys, _ := s.Top(ctx, from, to, ByKey, 0)
		if len(keys) != 3 { // the request that never got a key is not a key's traffic
			t.Fatalf("keys = %+v", keys)
		}
		errs, _ := s.Top(ctx, from, to, ByErrorKind, 10)
		if len(errs) != 2 || errs[0].Requests != 1 {
			t.Fatalf("errors = %+v", errs)
		}
		ips, _ := s.Top(ctx, from, to, ByClientIP, 1)
		if len(ips) != 1 || ips[0].Value != "10.0.0.1" || ips[0].Requests != 4 || !ips[0].LastTS.Equal(base.Add(62*time.Minute)) {
			t.Fatalf("ips = %+v", ips)
		}
		if _, err := s.Top(ctx, from, to, "status; DROP TABLE request_logs", 1); err == nil {
			t.Fatal("unknown dimension accepted")
		}

		all, _ := s.QueryRequestLogs(ctx, LogQuery{})
		got, err := s.GetRequestLog(ctx, all[2].ID)
		if err != nil || got.ID != all[2].ID || got.Operation != all[2].Operation {
			t.Fatalf("get = %+v %v", got, err)
		}
		if _, err := s.GetRequestLog(ctx, 1<<40); err != ErrNotFound {
			t.Fatalf("missing log: %v", err)
		}
	})
}
