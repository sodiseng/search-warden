package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Run probes egresses on their schedule until ctx is done (§7.4).
func (r *Runtime) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	sem := make(chan struct{}, probeConcurrent)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		for _, st := range r.due() {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				r.probeEgress(ctx, st)
			}()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-r.wake:
		}
	}
}

// due marks and returns the egresses whose probe time has come.
func (r *Runtime) due() []*probeJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.opts.Now()
	var jobs []*probeJob
	for _, st := range r.eg {
		if !st.enabled || st.probing || st.probe == nil || now.Before(st.nextProbe) {
			continue
		}
		st.probing = true
		jobs = append(jobs, &probeJob{st: st, client: st.probe, epoch: st.epoch,
			wantIP: st.exitIP == "" || now.Sub(st.lastIPCheck) >= exitIPRefresh || st.health != HealthHealthy})
	}
	return jobs
}

type probeJob struct {
	st     *egressState
	client *http.Client
	epoch  uint64
	wantIP bool
}

func (r *Runtime) probeEgress(ctx context.Context, j *probeJob) {
	start := r.opts.Now()
	var perr error
	for _, target := range r.opts.ProbeTargets {
		if perr = probeOnce(ctx, j.client, target); perr != nil {
			break
		}
	}
	latency := int(r.opts.Now().Sub(start).Milliseconds()) / max(len(r.opts.ProbeTargets), 1)

	var ip string
	if perr == nil && j.wantIP {
		ip = r.detectExitIP(ctx, j.client)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	st := j.st
	st.probing = false
	now := r.opts.Now()
	st.lastChecked, st.dirty = now, true
	if st.epoch != j.epoch {
		st.nextProbe = maxTime(st.nextProbe, now.Add(time.Second)) // state changed meanwhile: this result is stale
		return
	}
	if perr != nil {
		st.failStreak++
		st.lastError = sanitize(perr)
		if st.failStreak >= failThreshold || st.health == HealthQuarantined || st.health == HealthBlocked {
			if st.health == HealthBlocked {
				st.nextProbe = now.Add(backoffMin)
				return
			}
			r.quarantine(st, "probe failed")
			return
		}
		st.nextProbe = now.Add(backoffMin / 3) // re-check soon before quarantining
		return
	}
	if st.health == HealthQuarantined || st.health == HealthBlocked {
		r.log.Info("egress recovered", "egress", st.label)
	}
	st.health, st.failStreak, st.backoff, st.until, st.lastError = HealthHealthy, 0, 0, time.Time{}, ""
	st.latencyMS = latency
	st.nextProbe = now.Add(r.interval)
	if ip != "" {
		st.exitIP, st.lastIPCheck = ip, now
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// probeOnce requires only that an HTTP response comes back through the egress;
// no credentials are sent, so the status does not matter.
func probeOnce(ctx context.Context, c *http.Client, target string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return nil
}

// detectExitIP asks an IP-echo service through the egress.
func (r *Runtime) detectExitIP(ctx context.Context, c *http.Client) string {
	for _, u := range r.opts.IPEchoURLs {
		if ip, err := fetchIP(ctx, c, u); err == nil {
			return ip
		}
	}
	return ""
}

func fetchIP(ctx context.Context, c *http.Client, u string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", err
	}
	return parseIP(string(b))
}

// parseIP accepts a bare IP or Cloudflare's trace format ("ip=1.2.3.4" line).
func parseIP(body string) (string, error) {
	s := strings.TrimSpace(body)
	if net.ParseIP(s) != nil {
		return s, nil
	}
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ip="); ok && net.ParseIP(v) != nil {
			return v, nil
		}
	}
	return "", errors.New("no IP in response")
}
