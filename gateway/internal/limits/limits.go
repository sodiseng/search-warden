// Package limits enforces per-token limits (ARCHITECTURE.md §6.4, §10.1):
// requests per minute, concurrent requests, requests per UTC day and month,
// and a monthly spend cap in Tavily credits and in Exa dollars.
//
// Counters live in memory and are flushed to the store every few seconds, so
// the request path never writes to the database; they are loaded back at
// startup. A call is admitted when no limit is reached yet and charged with
// its actual cost afterwards, so one in-flight call can overshoot a spend cap
// by its own cost — never more.
package limits

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/provider"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

// Store is the part of *store.Store the limiter uses.
type Store interface {
	UpsertTokenUsage(ctx context.Context, rows []store.TokenUsage) error
	ListTokenUsage(ctx context.Context, periods ...string) ([]store.TokenUsage, error)
	PruneTokenUsage(ctx context.Context, cutoff time.Time) (int64, error)
}

// Exceeded is a rejection: which limit, and when it frees up.
type Exceeded struct {
	Limit      string // rpm | concurrency | daily_requests | monthly_requests | monthly_credits | monthly_usd
	Message    string
	RetryAfter time.Duration
}

func (e *Exceeded) Error() string { return e.Message }

// Status is the HTTP status of a rejection: 429 for every limit.
func (e *Exceeded) Status() int { return http.StatusTooManyRequests }

// RetryAfterSeconds is the Retry-After value (at least 1).
func (e *Exceeded) RetryAfterSeconds() int { return RetryAfterSeconds(e.RetryAfter) }

type counter struct {
	requests, credits, microUSD int64
	dirty                       bool
}

type tokenState struct {
	bucket   *rate.Limiter
	rpm      int
	inFlight int
}

// Limiter is safe for concurrent use.
type Limiter struct {
	mu     sync.Mutex
	st     Store
	log    *slog.Logger
	now    func() time.Time
	tokens map[string]*tokenState
	usage  map[string]*counter // tokenID + "|" + period
}

// New returns a Limiter; st may be nil (counters then reset on restart).
func New(st Store, log *slog.Logger, now func() time.Time) *Limiter {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{st: st, log: log, now: now, tokens: map[string]*tokenState{}, usage: map[string]*counter{}}
}

// Periods are UTC calendar days and months.
func dayPeriod(t time.Time) string   { return "d" + t.UTC().Format("2006-01-02") }
func monthPeriod(t time.Time) string { return "m" + t.UTC().Format("2006-01") }

func nextDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
}

func nextMonth(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

// Load reads the current day's and month's counters (call once at startup).
func (l *Limiter) Load(ctx context.Context) error {
	if l.st == nil {
		return nil
	}
	now := l.now()
	rows, err := l.st.ListTokenUsage(ctx, dayPeriod(now), monthPeriod(now))
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range rows {
		l.usage[r.TokenID+"|"+r.Period] = &counter{requests: r.Requests, credits: r.Credits, microUSD: r.MicroUSD}
	}
	return nil
}

func (l *Limiter) counter(tokenID, period string) *counter {
	k := tokenID + "|" + period
	c := l.usage[k]
	if c == nil {
		c = &counter{}
		l.usage[k] = c
	}
	return c
}

func (l *Limiter) state(t *confstore.Token) *tokenState {
	s := l.tokens[t.ID]
	if s == nil {
		s = &tokenState{}
		l.tokens[t.ID] = s
	}
	if s.rpm != t.Limits.RPM { // config changed: new bucket
		s.rpm, s.bucket = t.Limits.RPM, nil
		if t.Limits.RPM > 0 {
			// Refills the whole minute's allowance over a minute; holds at most that much.
			s.bucket = rate.NewLimiter(rate.Limit(float64(t.Limits.RPM)/60), t.Limits.RPM)
		}
	}
	return s
}

// Admit checks every limit of the token for a new request to provider. On
// success it counts the request and returns release, which the caller must
// call when the request ends (it frees the concurrency slot).
func (l *Limiter) Admit(t *confstore.Token, providerID string) (release func(), err error) {
	lim := t.Limits
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	s := l.state(t)
	day, month := l.counter(t.ID, dayPeriod(now)), l.counter(t.ID, monthPeriod(now))

	switch {
	case lim.Concurrency > 0 && s.inFlight >= lim.Concurrency:
		return nil, &Exceeded{"concurrency", fmt.Sprintf("this token already has %d requests in progress (limit %d)", s.inFlight, lim.Concurrency), time.Second}
	case lim.DailyRequests > 0 && day.requests >= lim.DailyRequests:
		return nil, &Exceeded{"daily_requests", fmt.Sprintf("this token reached its daily limit of %d requests (UTC day)", lim.DailyRequests), nextDay(now).Sub(now)}
	case lim.MonthlyRequests > 0 && month.requests >= lim.MonthlyRequests:
		return nil, &Exceeded{"monthly_requests", fmt.Sprintf("this token reached its monthly limit of %d requests (UTC month)", lim.MonthlyRequests), nextMonth(now).Sub(now)}
	case providerID == "tavily" && lim.MonthlyCredits > 0 && month.credits >= lim.MonthlyCredits:
		return nil, &Exceeded{"monthly_credits", fmt.Sprintf("this token used its monthly budget of %d Tavily credits", lim.MonthlyCredits), nextMonth(now).Sub(now)}
	case providerID == "exa" && lim.MonthlyUSD > 0 && float64(month.microUSD) >= lim.MonthlyUSD*1e6:
		return nil, &Exceeded{"monthly_usd", fmt.Sprintf("this token used its monthly Exa budget of $%g", lim.MonthlyUSD), nextMonth(now).Sub(now)}
	}
	if s.bucket != nil {
		if r := s.bucket.ReserveN(now, 1); r.DelayFrom(now) > 0 {
			wait := r.DelayFrom(now)
			r.CancelAt(now)
			return nil, &Exceeded{"rpm", fmt.Sprintf("this token is limited to %d requests per minute", lim.RPM), wait}
		}
	}
	s.inFlight++
	day.requests++
	month.requests++
	day.dirty, month.dirty = true, true
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			s.inFlight = max(s.inFlight-1, 0)
		})
	}, nil
}

// Charge adds the actual cost of one upstream call to the token's month.
func (l *Limiter) Charge(tokenID string, c provider.Cost) {
	if c.Credits == 0 && c.MicroUSD == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, p := range []string{dayPeriod(now), monthPeriod(now)} {
		k := l.counter(tokenID, p)
		k.credits += c.Credits
		k.microUSD += c.MicroUSD
		k.dirty = true
	}
}

// Usage is a token's use in the current UTC day and month.
type Usage struct {
	DayRequests, MonthRequests int64
	MonthCredits               int64
	MonthMicroUSD              int64
	InFlight                   int
}

// Usage returns the current counters of a token.
func (l *Limiter) Usage(tokenID string) Usage {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	u := Usage{}
	if c := l.usage[tokenID+"|"+dayPeriod(now)]; c != nil {
		u.DayRequests = c.requests
	}
	if c := l.usage[tokenID+"|"+monthPeriod(now)]; c != nil {
		u.MonthRequests, u.MonthCredits, u.MonthMicroUSD = c.requests, c.credits, c.microUSD
	}
	if s := l.tokens[tokenID]; s != nil {
		u.InFlight = s.inFlight
	}
	return u
}

// Flush persists changed counters and forgets past periods. Run it every few
// seconds and at shutdown.
func (l *Limiter) Flush(ctx context.Context) error {
	l.mu.Lock()
	now := l.now()
	keep := map[string]bool{dayPeriod(now): true, monthPeriod(now): true}
	var rows []store.TokenUsage
	for k, c := range l.usage {
		tokenID, period := splitKey(k)
		if c.dirty {
			c.dirty = false
			rows = append(rows, store.TokenUsage{TokenID: tokenID, Period: period, Requests: c.requests, Credits: c.credits, MicroUSD: c.microUSD})
		}
		if !keep[period] {
			delete(l.usage, k)
		}
	}
	l.mu.Unlock()
	if l.st == nil || len(rows) == 0 {
		return nil
	}
	if err := l.st.UpsertTokenUsage(ctx, rows); err != nil {
		l.mu.Lock()
		for _, r := range rows { // retry with the next flush
			if c := l.usage[r.TokenID+"|"+r.Period]; c != nil {
				c.dirty = true
			}
		}
		l.mu.Unlock()
		return err
	}
	return nil
}

// Prune deletes stored counters older than two months.
func (l *Limiter) Prune(ctx context.Context) {
	if l.st == nil {
		return
	}
	if _, err := l.st.PruneTokenUsage(ctx, l.now().AddDate(0, -2, 0)); err != nil {
		l.log.Warn("prune token usage", "err", err)
	}
}

func splitKey(k string) (string, string) {
	for i := len(k) - 1; i >= 0; i-- {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

// RetryAfterSeconds renders a wait as a Retry-After value (at least 1).
func RetryAfterSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}
