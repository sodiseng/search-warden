// Package relay sends one logical call upstream, retrying across keys
// (ARCHITECTURE.md §4.1, §6.3, §7.7, §9). REST passthrough and the MCP facade
// both go through Do, so routing, billing and logging exist in exactly one place.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/auth/clientauth"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/httpx"
	"github.com/sodiseng/search-warden/gateway/internal/limits"
	"github.com/sodiseng/search-warden/gateway/internal/outbound"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/provider"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

const (
	maxRequestBody  = 2 << 20
	maxResponseBody = 32 << 20
	taskTTL         = 24 * time.Hour
	maxStreamTime   = 30 * time.Minute
	retryAfterHint  = "30"
	defaultRateWait = 15 * time.Second // how long a call may wait for local rate limits
)

// Recorder receives finished request logs (obs.Writer).
type Recorder interface {
	Record(l store.RequestLog, attempts []store.Attempt)
}

// Options wires a Relay.
type Options struct {
	Conf      *confstore.Manager
	Pool      *pool.Pool
	Out       *outbound.Runtime
	Rec       Recorder
	Providers map[string]provider.Provider // nil = production endpoints
	Logger    *slog.Logger
	Now       func() time.Time
	// MaxRateWait bounds how long one call waits for local rate limits (0 = 15 s).
	MaxRateWait time.Duration
	// Limits enforces per-token limits; nil = none.
	Limits TokenLimiter
}

// TokenLimiter is implemented by *limits.Limiter.
type TokenLimiter interface {
	Admit(t *confstore.Token, providerID string) (release func(), err error)
	Charge(tokenID string, c provider.Cost)
}

// Relay is safe for concurrent use.
type Relay struct {
	conf      *confstore.Manager
	pool      *pool.Pool
	out       *outbound.Runtime
	rec       Recorder
	providers map[string]provider.Provider
	log       *slog.Logger
	now       func() time.Time
	tasks     taskMap
	rateWait  time.Duration
	lim       TokenLimiter
}

// New creates a Relay.
func New(o Options) *Relay {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Providers == nil {
		o.Providers = map[string]provider.Provider{}
		for _, id := range confstore.Providers {
			p, _ := provider.Get(id)
			o.Providers[id] = p
		}
	}
	if o.MaxRateWait <= 0 {
		o.MaxRateWait = defaultRateWait
	}
	return &Relay{conf: o.Conf, pool: o.Pool, out: o.Out, rec: o.Rec, providers: o.Providers, log: o.Logger, now: o.Now,
		tasks: taskMap{m: map[string]taskEntry{}}, rateWait: o.MaxRateWait, lim: o.Limits}
}

// Call is one logical upstream request.
type Call struct {
	Provider  provider.Provider
	Op        provider.Operation
	Body      []byte // already rewritten for upstream
	RawBody   []byte // as received; used for previews and estimates
	Injected  bool   // RewriteRequest added a field that must be stripped from the response
	Query     string // query string to forward (credentials already removed)
	Accept    string
	CType     string
	Extra     http.Header // client headers allowed by upstream.extra_headers
	Token     *confstore.Token
	Subject   string // affinity subject; defaults to the token ID
	Surface   string // "rest" or "mcp"
	ClientIP  httpx.ClientIP
	UserAgent string
	RequestID string
	Stream    http.ResponseWriter // non-nil: event streams are copied straight through
	// Quiet skips the request log for a successful, free call (the polls of an
	// async task: the call that created it is logged, and so is any error).
	Quiet bool
}

// Result is what to send back to the caller.
type Result struct {
	Status   int
	Header   http.Header
	Body     []byte
	Streamed bool
	Cost     provider.Cost
	Gateway  bool // the error was produced by the gateway, not upstream
}

func attemptTimeout(op provider.Operation) time.Duration {
	switch op.Name {
	case "crawl":
		return 150 * time.Second
	case "extract", "contents", "findSimilar", "answer":
		return 60 * time.Second
	}
	return 30 * time.Second
}

// attemptResult is the outcome of one try against one key.
type attemptResult struct {
	status     int
	header     http.Header
	body       []byte
	streamed   bool
	outcome    provider.Outcome
	cost       provider.Cost
	connectErr bool
	sendErr    error
	clientGone bool
}

// Do runs the attempt loop and records the request log.
func (h *Relay) Do(ctx context.Context, c *Call) *Result {
	start := h.now()
	snap := h.conf.Snapshot()
	routing := snap.Config.Routing
	deadline := start.Add(time.Duration(routing.DeadlineSeconds) * time.Second)
	subject := c.Subject
	if subject == "" {
		subject = c.Token.ID
	}
	pin := ""
	if c.Op.Task == provider.TaskPoll {
		pin = h.tasks.get(c.Provider.ID(), c.Op.TaskID, h.now())
	}

	entry := store.RequestLog{TS: start, RequestID: c.RequestID, ClientIP: c.ClientIP.String(), IPSource: c.ClientIP.Source,
		UserAgent: c.UserAgent, TokenID: c.Token.ID, Provider: c.Provider.ID(), Surface: c.Surface, Operation: c.Op.Name,
		ReqBytes: int64(len(c.RawBody)), QueryPreview: c.Provider.QueryPreview(c.Op, c.RawBody, snap.Config.Logs.QueryPreviewChars)}

	// Token limits (§6.4): a new request is admitted or rejected here. The polls
	// of an async task (Quiet) are part of the request that created it: not
	// counted again, but what they cost is charged.
	if h.lim != nil && !c.Quiet {
		release, err := h.lim.Admit(c.Token, c.Provider.ID())
		if err != nil {
			res := h.gatewayError(c, http.StatusTooManyRequests, "token_limit", err.Error())
			var ex interface{ RetryAfterSeconds() int }
			if errors.As(err, &ex) {
				res.Header.Set("Retry-After", strconv.Itoa(ex.RetryAfterSeconds()))
			}
			entry.Status, entry.Outcome, entry.ErrorKind = res.Status, "gateway_error", "token_limit"
			entry.LatencyMS = int(h.now().Sub(start).Milliseconds())
			if h.rec != nil {
				h.rec.Record(entry, nil)
			}
			return res
		}
		defer release()
	}

	var (
		attempts     []store.Attempt
		exclude      = map[string]bool{}
		sent         int
		connectFails int
		waited       time.Duration
		last         *attemptResult
		lastKey      string
		res          *Result
		upstreamMS   int
	)
	for {
		if !h.now().Before(deadline) {
			if last == nil {
				res = h.gatewayError(c, http.StatusGatewayTimeout, "upstream_timeout", "the request exceeded the gateway deadline")
				entry.ErrorKind = "deadline"
			}
			break
		}
		lease, err := h.pool.Pick(pool.Request{Provider: c.Provider.ID(), Subject: subject, KeyGroup: c.Token.KeyGroup, Exclude: exclude, PinKey: pin,
			Limits: func(k confstore.Key) []provider.Limit {
				return c.Provider.Limits(c.Op, c.RawBody, k.Secret, k.RateLimit)
			}})
		if err != nil {
			var nk *pool.NoKeyError
			if errors.As(err, &nk) && nk.RetryIn > 0 {
				// Only local rate limits are in the way: a short wait beats an error.
				wait := nk.RetryIn + 2*time.Millisecond
				if waited+wait <= h.rateWait && h.now().Add(wait).Before(deadline) {
					if !sleep(ctx, wait) {
						res = &Result{Status: 499, Header: http.Header{}, Gateway: true}
						entry.ErrorKind = "client_gone"
						break
					}
					waited += wait
					continue
				}
				if last == nil {
					res = h.gatewayError(c, http.StatusTooManyRequests, "rate_limited", "the upstream rate limit of every usable key is reached; retry shortly")
					res.Header.Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(nk.RetryIn.Seconds())))))
					entry.ErrorKind = "rate_limited"
				}
				break
			}
			if last == nil { // otherwise report the most informative upstream answer
				msg := err.Error()
				if connectFails > 0 {
					msg += fmt.Sprintf("; %d key(s) could not connect through their egress", connectFails)
				}
				res = h.gatewayError(c, http.StatusServiceUnavailable, "no_available_keys", msg)
				res.Header.Set("Retry-After", retryAfterHint)
				entry.ErrorKind = "no_keys"
			}
			break
		}
		key := lease.Key
		client := h.out.Client(key.ID)
		if client == nil {
			lease.Abort()
			exclude[key.ID] = true
			continue
		}
		t0 := h.now()
		r := h.attempt(ctx, c, snap, client, key, deadline)
		ms := int(h.now().Sub(t0).Milliseconds())
		upstreamMS += ms
		at := store.Attempt{RequestID: c.RequestID, N: len(attempts) + 1, TS: t0, KeyID: key.ID, EgressID: key.Egress.ID, LatencyMS: ms}
		entry.KeyID, entry.EgressID = key.ID, key.Egress.ID
		exclude[key.ID] = true

		stop := false
		switch {
		case r.clientGone:
			lease.Abort()
			at.ErrorKind = "client_gone"
			res = &Result{Status: 499, Header: http.Header{}, Gateway: true}
			stop = true
		case r.connectErr: // never reached upstream: the egress is at fault, not the key
			lease.Abort() // does not use up an attempt (§6.3)
			h.out.ReportConnectFailure(key.Egress.ID, r.sendErr)
			connectFails++
			at.ErrorKind = "egress_connect"
		case r.sendErr != nil: // sent, then lost: upstream may have processed and billed it
			h.out.ReportSuccess(key.Egress.ID)
			lease.Done(provider.Outcome{Class: provider.ClassServerError}, provider.Cost{})
			sent++
			at.ErrorKind = "upstream_io"
			if !c.Op.RetryAfterSend || sent >= routing.MaxAttempts {
				if last == nil {
					res = h.gatewayError(c, http.StatusBadGateway, "upstream_error", "the upstream connection failed after the request was sent")
				}
				stop = true
			}
		default:
			h.out.ReportSuccess(key.Egress.ID)
			if c.Op.Task == provider.TaskPoll && (r.cost.Credits != 0 || r.cost.MicroUSD != 0) && !h.tasks.markBilled(c.Provider.ID(), c.Op.TaskID, h.now().Add(taskTTL)) {
				r.cost = provider.Cost{} // a finished task is billed by the first poll that sees it end
			}
			lease.Done(r.outcome, r.cost)
			if r.outcome.Class == provider.ClassEdgeBlocked {
				h.out.MarkBlocked(key.Egress.ID)
			}
			sent++
			at.Status = r.status
			if r.outcome.Class != provider.ClassOK {
				at.ErrorKind = r.outcome.Class.String()
			}
			if r.outcome.Class == provider.ClassOK && c.Op.Task == provider.TaskCreate {
				if id := c.Provider.TaskIDFromResponse(c.Op, r.body); id != "" {
					h.tasks.set(c.Provider.ID(), id, key.ID, h.now().Add(taskTTL))
					if (r.cost.Credits != 0 || r.cost.MicroUSD != 0) && !r.cost.Estimated {
						// Charged at creation (a task that ended at once, or a fixed price):
						// the polls must not charge it a second time.
						h.tasks.markBilled(c.Provider.ID(), id, h.now().Add(taskTTL))
					}
				}
			}
			last, lastKey = r, key.Secret
			stop = !r.outcome.Retry || sent >= routing.MaxAttempts || pin != ""
		}
		attempts = append(attempts, at)
		entry.ErrorKind = at.ErrorKind
		if stop {
			break
		}
	}

	if res == nil && last != nil {
		res = h.finish(c, last, lastKey)
	}
	if res == nil { // only reachable if every path above declined to answer
		res = h.gatewayError(c, http.StatusBadGateway, "upstream_error", "no upstream response")
	}
	entry.Status = res.Status
	entry.Attempts = len(attempts)
	entry.Outcome = outcomeLabel(res, last)
	entry.LatencyMS = int(h.now().Sub(start).Milliseconds())
	entry.UpstreamLatencyMS = upstreamMS
	entry.CostCredits, entry.CostMicroUSD, entry.CostEstimated = res.Cost.Credits, res.Cost.MicroUSD, res.Cost.Estimated
	entry.RespBytes = int64(len(res.Body))
	entry.ExitIP = h.out.ExitIP(entry.EgressID)
	if res.Status < 400 {
		entry.ErrorKind = ""
	}
	if len(attempts) <= 1 {
		attempts = nil // details are only kept when a retry happened
	}
	if h.lim != nil {
		h.lim.Charge(c.Token.ID, res.Cost)
	}
	quiet := c.Quiet && res.Status < 400 && res.Cost.Credits == 0 && res.Cost.MicroUSD == 0 && len(attempts) == 0
	if h.rec != nil && !quiet {
		h.rec.Record(entry, attempts)
	}
	return res
}

// Provider returns the provider a relay sends to ("tavily" or "exa").
func (h *Relay) Provider(id string) (provider.Provider, bool) {
	p, ok := h.providers[id]
	return p, ok
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func outcomeLabel(res *Result, last *attemptResult) string {
	if res.Gateway || last == nil {
		return "gateway_error"
	}
	return last.outcome.Class.String()
}

// finish converts the last upstream answer into the response for the caller.
func (h *Relay) finish(c *Call, r *attemptResult, key string) *Result {
	if r.streamed {
		return &Result{Status: r.status, Header: http.Header{}, Streamed: true, Cost: r.cost}
	}
	body := r.body
	if r.status >= 400 {
		body = bytes.ReplaceAll(body, []byte(key), []byte("[key]")) // an upstream error must never echo a key
	} else {
		body = c.Provider.RewriteResponse(c.Op, body, c.Injected)
	}
	hdr := responseHeaders(r.header)
	if r.status == http.StatusTooManyRequests {
		hdr.Set("Retry-After", retryAfterHint) // that key's own limit says little: all keys were tried
	}
	return &Result{Status: r.status, Header: hdr, Body: body, Cost: r.cost}
}

// responseHeaders keeps only what a client needs (§7.7). Upstream rate-limit,
// cookie and infrastructure headers describe one key, not the gateway.
func responseHeaders(h http.Header) http.Header {
	out := http.Header{}
	if v := h.Get("Content-Type"); v != "" {
		out.Set("Content-Type", v)
	}
	return out
}

// attempt performs one try. It never decides about retries; Do does.
func (h *Relay) attempt(ctx context.Context, c *Call, snap *confstore.Snapshot, client *http.Client, key confstore.Key, deadline time.Time) *attemptResult {
	limit := min(attemptTimeout(c.Op), deadline.Sub(h.now()))
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(limit, cancel)
	defer timer.Stop()

	var wrote atomic.Bool
	actx = httptrace.WithClientTrace(actx, &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) {
		if i.Err == nil {
			wrote.Store(true)
		}
	}})

	u := c.Provider.BaseURL() + c.Op.Path
	if c.Query != "" {
		u += "?" + c.Query
	}
	var rd io.Reader
	if len(c.Body) > 0 {
		rd = bytes.NewReader(c.Body)
	}
	req, err := http.NewRequestWithContext(actx, c.Op.Method, u, rd)
	if err != nil {
		return &attemptResult{connectErr: true, sendErr: err}
	}
	h.setHeaders(req, c, snap, key)

	resp, err := client.Do(req)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return &attemptResult{clientGone: true, sendErr: err}
		case !wrote.Load():
			return &attemptResult{connectErr: true, sendErr: err}
		}
		return &attemptResult{sendErr: err}
	}
	defer resp.Body.Close()

	if c.Stream != nil && resp.StatusCode < 300 && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		timer.Stop()
		timer = time.AfterFunc(maxStreamTime, cancel)
		defer timer.Stop()
		return h.stream(c, resp)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return &attemptResult{clientGone: true, sendErr: err}
		}
		return &attemptResult{sendErr: err}
	}
	if len(body) > maxResponseBody {
		return &attemptResult{sendErr: errors.New("upstream response too large")}
	}
	o := c.Provider.Classify(c.Op, resp.StatusCode, resp.Header, body)
	o.Reason = provider.RedactSecret(o.Reason, key.Secret)
	r := &attemptResult{status: resp.StatusCode, header: resp.Header, body: body, outcome: o}
	if o.Class == provider.ClassOK && c.Op.Billable {
		if cost, ok := c.Provider.ActualCost(c.Op, body); ok {
			r.cost = cost
		} else {
			r.cost = c.Provider.EstimateCost(c.Op, c.RawBody)
		}
	}
	return r
}

// setHeaders builds the upstream header set from nothing (allowlist, §7.7):
// nothing about the caller — IP, cookies, User-Agent — reaches upstream.
func (h *Relay) setHeaders(req *http.Request, c *Call, snap *confstore.Snapshot, key confstore.Key) {
	hd := req.Header
	if len(c.Body) > 0 {
		ct := c.CType
		if ct == "" {
			ct = "application/json"
		}
		hd.Set("Content-Type", ct)
	}
	accept := c.Accept
	if accept == "" {
		accept = "application/json"
	}
	hd.Set("Accept", accept)
	hd.Set("User-Agent", snap.Config.Upstream.UserAgentFor(c.Provider.ID()))
	for k, vs := range c.Extra {
		hd[k] = vs
	}
	c.Provider.Authorize(hd, key.Secret)
}

// stream copies an event stream to the caller as it arrives.
func (h *Relay) stream(c *Call, resp *http.Response) *attemptResult {
	w := c.Stream
	if v := resp.Header.Get("Content-Type"); v != "" {
		w.Header().Set("Content-Type", v)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	buf := make([]byte, 16<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			_ = rc.Flush()
		}
		if err != nil {
			break
		}
	}
	cost := provider.Cost{}
	if c.Op.Billable {
		cost = c.Provider.EstimateCost(c.Op, c.RawBody)
	}
	return &attemptResult{status: resp.StatusCode, streamed: true, cost: cost,
		outcome: provider.Outcome{Class: provider.ClassOK, Status: resp.StatusCode}}
}

// ---- gateway-generated errors ----------------------------------------------

// errorJSON matches each provider's error shape so SDKs show the message.
func errorJSON(providerID, code, msg string) []byte {
	var v any
	if providerID == "tavily" {
		v = map[string]any{"detail": map[string]string{"error": msg}}
	} else {
		v = map[string]string{"error": msg, "tag": code}
	}
	b, _ := json.Marshal(v)
	return b
}

func (h *Relay) gatewayError(c *Call, status int, code, msg string) *Result {
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	return &Result{Status: status, Header: hdr, Body: errorJSON(c.Provider.ID(), code, msg), Gateway: true}
}

func writeError(w http.ResponseWriter, providerID string, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(errorJSON(providerID, code, msg))
}

// ---- REST passthrough ------------------------------------------------------

// REST returns the passthrough handler of a provider. Mount it with the
// "/<provider>" prefix stripped.
func (h *Relay) REST(id string) http.Handler {
	p := h.providers[id]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			writeError(w, id, http.StatusRequestEntityTooLarge, "request_too_large", fmt.Sprintf("request body exceeds %d bytes", maxRequestBody))
			return
		}
		snap := h.conf.Snapshot()
		ip := httpx.ClientIPFrom(r.Context())
		tokStr, err := clientauth.Extract(r, body)
		var tok *confstore.Token
		if err == nil {
			tok, err = clientauth.Authorize(snap, tokStr, id, "rest", ip.Addr, h.now())
		}
		if err != nil {
			d, _ := clientauth.IsDenied(err)
			writeError(w, id, d.Status, d.Code, d.Msg)
			return
		}
		op, ok := p.Match(r.Method, r.URL.Path)
		if !ok {
			writeError(w, id, http.StatusNotFound, "not_found", "unknown endpoint: "+r.Method+" "+r.URL.Path)
			return
		}
		if op.Local {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(h.usageView(id, tok))
			return
		}
		out, injected, err := p.RewriteRequest(op, body, snap.Config.Upstream.PreciseBilling)
		if err != nil {
			writeError(w, id, http.StatusBadRequest, "bad_request", "could not process the request body")
			return
		}
		extra := http.Header{}
		for _, name := range snap.Config.Upstream.ExtraHeaders {
			if vs := r.Header.Values(name); len(vs) > 0 {
				extra[http.CanonicalHeaderKey(name)] = vs
			}
		}
		res := h.Do(r.Context(), &Call{Provider: p, Op: op, Body: out, RawBody: body, Injected: injected,
			Query: clientauth.StripQuery(r.URL.RawQuery), Accept: r.Header.Get("Accept"), CType: r.Header.Get("Content-Type"),
			Extra: extra, Token: tok, Surface: "rest", ClientIP: ip, UserAgent: r.UserAgent(),
			RequestID: httpx.RequestIDFrom(r.Context()), Stream: w})
		if res.Streamed || res.Status == 499 {
			return
		}
		for k, vs := range res.Header {
			w.Header()[k] = vs
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(res.Status)
		_, _ = w.Write(res.Body)
	})
}

// usageView answers Tavily's GET /usage from the gateway's own books, in the
// API's shape: "key" is the caller's token (its credits this UTC month and
// its monthly_credits limit), "account" is the gateway's Tavily key pool.
// Upstream is never asked: /usage is rate-limited and belongs to the keys.
func (h *Relay) usageView(providerID string, tok *confstore.Token) []byte {
	var used, limit int64
	limitKnown := false
	for _, v := range h.pool.Views() {
		if v.Provider != providerID || v.Status == pool.StatusDisabled {
			continue
		}
		if v.Used != nil {
			used += *v.Used
		}
		if v.Limit != nil {
			limit, limitKnown = limit+*v.Limit, true
		}
	}
	var tokUsed int64
	if u, ok := h.lim.(interface{ Usage(string) limits.Usage }); ok {
		tokUsed = u.Usage(tok.ID).MonthCredits
	}
	key := map[string]any{"usage": tokUsed, "limit": nil}
	if tok.Limits.MonthlyCredits > 0 {
		key["limit"] = tok.Limits.MonthlyCredits
	}
	account := map[string]any{"current_plan": "search-warden", "plan_usage": used, "plan_limit": nil}
	if limitKnown {
		account["plan_limit"] = limit
	}
	b, _ := json.Marshal(map[string]any{"key": key, "account": account})
	return b
}

// ---- async task bindings ---------------------------------------------------

type taskEntry struct {
	keyID   string
	expires time.Time
	billed  bool
}

// taskMap remembers which key created an async task: follow-up calls must use
// the same key (§5.1). In memory for now; a restart loses in-flight bindings.
type taskMap struct {
	mu sync.Mutex
	m  map[string]taskEntry
}

func (t *taskMap) set(provider, id, keyID string, exp time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.m) > 10_000 {
		now := time.Now()
		for k, e := range t.m {
			if now.After(e.expires) {
				delete(t.m, k)
			}
		}
	}
	t.m[provider+":"+id] = taskEntry{keyID: keyID, expires: exp}
}

// markBilled records that a task's cost was charged; false if it already was.
// A task the gateway did not see created (e.g. after a restart) is billed too.
func (t *taskMap) markBilled(provider, id string, exp time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := provider + ":" + id
	e := t.m[k]
	if e.billed {
		return false
	}
	e.billed = true
	if e.expires.IsZero() {
		e.expires = exp
	}
	t.m[k] = e
	return true
}

func (t *taskMap) get(provider, id string, now time.Time) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[provider+":"+id]
	if !ok || now.After(e.expires) {
		return ""
	}
	return e.keyID
}
