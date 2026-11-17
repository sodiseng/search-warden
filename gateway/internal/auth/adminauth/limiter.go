package adminauth

import (
	"sync"
	"time"
)

// Login throttling (ARCHITECTURE.md §11.1), checked BEFORE the expensive
// argon2 verification so failed logins cannot be used to burn CPU:
//   - per IP: 5 failures within 15 minutes → lockout 1m, 2m, 4m … capped at 1h;
//   - per account (any IP): at most 20 attempts per minute, against IP rotation.
const (
	ipWindow      = 15 * time.Minute
	ipMaxFailures = 5
	lockoutBase   = time.Minute
	lockoutMax    = time.Hour
	globalWindow  = time.Minute
	globalMax     = 20
	maxTrackedIPs = 10_000
)

type ipState struct {
	failures    []time.Time
	lockouts    int
	lockedUntil time.Time
	lastSeen    time.Time
}

type limiter struct {
	mu     sync.Mutex
	ips    map[string]*ipState
	global []time.Time
}

func newLimiter() *limiter { return &limiter{ips: map[string]*ipState{}} }

// allow reports whether an attempt from ip may proceed; if not, how long to wait.
func (l *limiter) allow(ip string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st := l.ips[ip]; st != nil && now.Before(st.lockedUntil) {
		return st.lockedUntil.Sub(now), false
	}
	l.global = prune(l.global, now.Add(-globalWindow))
	if len(l.global) >= globalMax {
		return l.global[0].Add(globalWindow).Sub(now), false
	}
	l.global = append(l.global, now)
	return 0, true
}

// record notes the outcome of an allowed attempt.
func (l *limiter) record(ip string, now time.Time, success bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if success {
		delete(l.ips, ip)
		return
	}
	st := l.ips[ip]
	if st == nil {
		if len(l.ips) >= maxTrackedIPs {
			l.evictOldest()
		}
		st = &ipState{}
		l.ips[ip] = st
	}
	st.lastSeen = now
	st.failures = append(prune(st.failures, now.Add(-ipWindow)), now)
	if len(st.failures) >= ipMaxFailures {
		st.lockouts++
		d := lockoutBase << min(st.lockouts-1, 6)
		if d > lockoutMax {
			d = lockoutMax
		}
		st.lockedUntil = now.Add(d)
		st.failures = nil
	}
}

func (l *limiter) evictOldest() {
	var oldestIP string
	var oldest time.Time
	for ip, st := range l.ips {
		if oldestIP == "" || st.lastSeen.Before(oldest) {
			oldestIP, oldest = ip, st.lastSeen
		}
	}
	delete(l.ips, oldestIP)
}

// gc drops state that no longer matters (called from the periodic cleanup).
func (l *limiter) gc(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, st := range l.ips {
		if now.After(st.lockedUntil) && now.Sub(st.lastSeen) > 24*time.Hour {
			delete(l.ips, ip)
		}
	}
}

func prune(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	return ts[i:]
}
