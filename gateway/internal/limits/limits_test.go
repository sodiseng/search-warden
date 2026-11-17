package limits

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/config"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/provider"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func tok(l confstore.TokenLimits) *confstore.Token {
	return &confstore.Token{ID: "t1", Name: "laptop", Limits: l}
}

func exceeded(t *testing.T, err error, limit string) *Exceeded {
	t.Helper()
	var ex *Exceeded
	if !errors.As(err, &ex) || ex.Limit != limit {
		t.Fatalf("err = %v, want %s", err, limit)
	}
	return ex
}

func TestRequestsPerMinute(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	l := New(nil, nil, c.now)
	tk := tok(confstore.TokenLimits{RPM: 3})
	for i := 0; i < 3; i++ {
		release, err := l.Admit(tk, "tavily")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		release()
	}
	_, err := l.Admit(tk, "tavily")
	if ex := exceeded(t, err, "rpm"); ex.RetryAfter <= 0 || ex.RetryAfter > 20*time.Second || ex.Status() != 429 {
		t.Fatalf("retry after %v", ex.RetryAfter)
	}
	c.add(20 * time.Second) // one request per 20 s refills
	if _, err := l.Admit(tk, "tavily"); err != nil {
		t.Fatal(err)
	}
	// A rejected request is not counted.
	if u := l.Usage("t1"); u.DayRequests != 4 || u.MonthRequests != 4 {
		t.Fatalf("usage = %+v", u)
	}
	// Changing the limit takes effect at once.
	tk.Limits.RPM = 0
	for i := 0; i < 50; i++ {
		if _, err := l.Admit(tk, "tavily"); err != nil {
			t.Fatal("unlimited token was limited")
		}
	}
}

func TestConcurrency(t *testing.T) {
	l := New(nil, nil, nil)
	tk := tok(confstore.TokenLimits{Concurrency: 2})
	r1, _ := l.Admit(tk, "exa")
	r2, _ := l.Admit(tk, "exa")
	_, err := l.Admit(tk, "exa")
	exceeded(t, err, "concurrency")
	r1()
	r1() // releasing twice frees one slot only
	r3, err := l.Admit(tk, "exa")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Admit(tk, "exa"); err == nil {
		t.Fatal("double release freed two slots")
	}
	r2()
	r3()
	if u := l.Usage("t1"); u.InFlight != 0 {
		t.Fatalf("in flight = %d", u.InFlight)
	}
}

func TestDailyMonthlyAndBudgets(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)}
	l := New(nil, nil, c.now)
	tk := tok(confstore.TokenLimits{DailyRequests: 2, MonthlyRequests: 3, MonthlyCredits: 5, MonthlyUSD: 0.01})
	l.Admit(tk, "tavily")
	l.Admit(tk, "tavily")
	_, err := l.Admit(tk, "tavily")
	if ex := exceeded(t, err, "daily_requests"); ex.RetryAfter != time.Hour {
		t.Fatalf("daily retry %v", ex.RetryAfter)
	}
	c.add(time.Hour) // a new UTC day and a new month
	l.Admit(tk, "tavily")
	l.Admit(tk, "exa")
	c.add(24 * time.Hour)
	l.Admit(tk, "exa")
	_, err = l.Admit(tk, "exa")
	exceeded(t, err, "monthly_requests")

	// Spend caps are per provider: Tavily credits, Exa dollars.
	tk.Limits.MonthlyRequests, tk.Limits.DailyRequests = 0, 0
	l.Charge("t1", provider.Cost{Credits: 5})
	_, err = l.Admit(tk, "tavily")
	exceeded(t, err, "monthly_credits")
	if _, err := l.Admit(tk, "exa"); err != nil {
		t.Fatalf("credits cap blocked Exa: %v", err)
	}
	l.Charge("t1", provider.Cost{MicroUSD: 10_000})
	_, err = l.Admit(tk, "exa")
	exceeded(t, err, "monthly_usd")
	if u := l.Usage("t1"); u.MonthCredits != 5 || u.MonthMicroUSD != 10_000 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestCountersSurviveRestart(t *testing.T) {
	st, err := store.Open(context.Background(), config.DB{Driver: config.DriverSQLite, SQLiteDir: filepath.Join(t.TempDir(), "db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := &clock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	l := New(st, nil, c.now)
	tk := tok(confstore.TokenLimits{DailyRequests: 2})
	l.Admit(tk, "exa")
	l.Charge("t1", provider.Cost{MicroUSD: 7000})
	if err := l.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	l2 := New(st, nil, c.now)
	if err := l2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u := l2.Usage("t1"); u.DayRequests != 1 || u.MonthMicroUSD != 7000 {
		t.Fatalf("restored = %+v", u)
	}
	l2.Admit(tk, "exa")
	_, err = l2.Admit(tk, "exa")
	exceeded(t, err, "daily_requests")

	// Past periods drop out of memory at the next flush.
	c.add(40 * 24 * time.Hour)
	_ = l2.Flush(context.Background())
	if u := l2.Usage("t1"); u.MonthMicroUSD != 0 || u.DayRequests != 0 {
		t.Fatalf("old period still counted: %+v", u)
	}
}
