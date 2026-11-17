// Package mcp is the gateway's MCP facade (ARCHITECTURE.md §8): the official
// Tavily and Exa MCP tools, served from snapshots of the official servers'
// definitions, with every tool call turned into REST calls through the relay —
// so key selection, retries, rate limits, egress binding, billing and request
// logs are the same as for REST.
//
// The Streamable HTTP server is implemented here, statelessly, for both
// protocol eras: modern (2026-07-28: no handshake, version and capabilities in
// every request's _meta) and legacy (2025-03-26 … 2025-11-25: initialize
// handshake). No session is ever issued, so any key can serve any call.
package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"

	"github.com/sodiseng/search-warden/gateway/internal/auth/clientauth"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/httpx"
	"github.com/sodiseng/search-warden/gateway/internal/relay"
)

const (
	maxBody          = 4 << 20 // agent_run may carry input rows
	defaultKeepAlive = 15 * time.Second
)

// Options wires a Server. The timing fields exist for tests; zero means the
// documented default.
type Options struct {
	Conf   *confstore.Manager
	Relay  *relay.Relay
	Logger *slog.Logger
	Now    func() time.Time

	KeepAlive         time.Duration                                   // SSE comment interval during a tool call (15 s)
	ResearchPollStart time.Duration                                   // tavily_research: first poll delay (2 s)
	ResearchPollMax   time.Duration                                   // tavily_research: longest poll delay (10 s)
	ResearchMiniWait  time.Duration                                   // tavily_research: give up after (mini: 5 min)
	ResearchOtherWait time.Duration                                   // tavily_research: give up after (other models: 15 min)
	AgentPollInterval time.Duration                                   // agent_run: poll interval (4 s)
	AgentCallWindow   time.Duration                                   // agent_run: wait per call before handing back a runId (45 s)
	Sleep             func(ctx context.Context, d time.Duration) bool // waits between polls; false when ctx ends
}

// Server serves /tavily/mcp, /exa/mcp and /mcp.
type Server struct {
	o     Options
	conf  *confstore.Manager
	relay *relay.Relay
	log   *slog.Logger
	snaps map[string]*providerSnapshot
	tools map[string]toolFunc
}

type toolFunc func(ctx context.Context, cc *callCtx, args gjson.Result) *result

// New loads the snapshots and registers the tools.
func New(o Options) (*Server, error) {
	if o.Conf == nil || o.Relay == nil {
		return nil, errors.New("mcp: Conf and Relay are required")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.KeepAlive, defaultKeepAlive)
	def(&o.ResearchPollStart, 2*time.Second)
	def(&o.ResearchPollMax, 10*time.Second)
	def(&o.ResearchMiniWait, 5*time.Minute)
	def(&o.ResearchOtherWait, 15*time.Minute)
	def(&o.AgentPollInterval, 4*time.Second)
	def(&o.AgentCallWindow, 45*time.Second)
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	s := &Server{o: o, conf: o.Conf, relay: o.Relay, log: o.Logger, tools: map[string]toolFunc{}}
	s.registerTavily()
	s.registerExa()
	implemented := map[string]bool{}
	for name := range s.tools {
		implemented[name] = true
	}
	snaps, skipped, err := loadSnapshots(implemented)
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		s.log.Warn("official MCP tools not implemented by the gateway are hidden", "tools", strings.Join(skipped, ","))
	}
	s.snaps = snaps
	return s, nil
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

// Handler serves one MCP endpoint: scope "tavily", "exa", or "" (merged).
func (s *Server) Handler(scope string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serve(w, r, scope) })
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(responseJSON(id, nil, &rpcError{Code: code, Message: msg, Data: data}))
}

// originAllowed implements the DNS-rebinding check (MUST in the transport
// spec): an Origin, when sent, must be this host or a loopback address.
func originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// scopeFor applies the endpoint's URL-parameter rules (§8.1): on a provider
// path, a key parameter named for the other provider is a mistake; on /mcp,
// tavilyApiKey or exaApiKey alone selects that provider.
func scopeFor(path string, q url.Values) (string, error) {
	tv, ex := q.Has("tavilyApiKey"), q.Has("exaApiKey")
	switch {
	case tv && ex:
		return "", errors.New("send either tavilyApiKey or exaApiKey, not both")
	case path == "tavily" && ex:
		return "", errors.New("exaApiKey on the Tavily endpoint: use /exa/mcp for Exa, or /mcp for both")
	case path == "exa" && tv:
		return "", errors.New("tavilyApiKey on the Exa endpoint: use /tavily/mcp for Tavily, or /mcp for both")
	case path == "" && tv:
		return "tavily", nil
	case path == "" && ex:
		return "exa", nil
	}
	return path, nil
}

// request is one HTTP request's MCP context.
type request struct {
	s        *Server
	r        *http.Request
	view     *view
	tok      *confstore.Token
	ip       httpx.ClientIP
	sse      bool // the client accepts an event stream
	calls    atomic.Int32
	defaults gjson.Result
	dynamic  bool
	sType    string
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, pathScope string) {
	if r.Method != http.MethodPost {
		// No standalone SSE stream (GET) and no sessions to delete (DELETE).
		w.Header().Set("Allow", http.MethodPost)
		writeRPCError(w, http.StatusMethodNotAllowed, nil, codeInvalidRequest, "this MCP endpoint accepts POST only", nil)
		return
	}
	if !originAllowed(r) {
		writeRPCError(w, http.StatusForbidden, nil, codeInvalidRequest, "Origin not allowed", nil)
		return
	}
	scope, err := scopeFor(pathScope, r.URL.Query())
	if err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, codeInvalidRequest, err.Error(), nil)
		return
	}
	snap := s.conf.Snapshot()
	ip := httpx.ClientIPFrom(r.Context())
	tokStr, err := clientauth.Extract(r, nil)
	var tok *confstore.Token
	if err == nil {
		tok, err = clientauth.Authorize(snap, tokStr, scope, "mcp", ip.Addr, s.o.Now())
	}
	if err != nil {
		d, _ := clientauth.IsDenied(err)
		if d.Status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="search-warden"`)
		}
		writeRPCError(w, d.Status, nil, codeInvalidRequest, d.Msg, nil)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return // client went away
	}
	if len(body) > maxBody {
		writeRPCError(w, http.StatusRequestEntityTooLarge, nil, codeInvalidRequest, "request body too large", nil)
		return
	}
	q := r.URL.Query()
	rq := &request{s: s, r: r, tok: tok, ip: ip, view: s.selectTools(scope, tok, q.Get("providers"), q.Get("tools")),
		sse:     strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream"),
		dynamic: q.Get("dynamicHighlights") == "true", sType: searchType(q.Get("defaultSearchType"))}
	if dp := strings.TrimSpace(r.Header.Get("DEFAULT_PARAMETERS")); dp != "" {
		if d := gjson.Parse(dp); gjson.Valid(dp) && d.IsObject() {
			rq.defaults = d
		}
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' { // legacy (2025-03-26) batch
		rq.serveBatch(w, trimmed)
		return
	}
	var m message
	if err := json.Unmarshal(trimmed, &m); err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, codeParseError, "Parse error: the body is not a JSON-RPC message", nil)
		return
	}
	rq.serveOne(w, &m)
}

func searchType(v string) string {
	switch v {
	case "auto", "lite", "fast", "instant":
		return v
	}
	return ""
}

// era decides how a message is served. It returns an HTTP status and error
// when the request must be rejected (transport spec: Server Validation).
func (rq *request) era(m *message) (modern bool, status int, e *rpcError) {
	hdr := strings.TrimSpace(rq.r.Header.Get("MCP-Protocol-Version"))
	metaVer := gjson.GetBytes(m.Params, "_meta."+gjsonKey(metaProtocolVersion)).String()
	if metaVer == "" {
		switch {
		case m.Method == "initialize" || hdr == "" || slices.Contains(legacyVersions, hdr):
			return false, 0, nil
		case slices.Contains(modernVersions, hdr):
			return false, http.StatusBadRequest, &rpcError{Code: codeHeaderMismatch,
				Message: "Header mismatch: MCP-Protocol-Version is " + hdr + " but params._meta has no " + metaProtocolVersion}
		}
		return false, http.StatusBadRequest, unsupported(hdr)
	}
	if !slices.Contains(modernVersions, metaVer) {
		return true, http.StatusBadRequest, unsupported(metaVer)
	}
	if hdr != metaVer {
		return true, http.StatusBadRequest, &rpcError{Code: codeHeaderMismatch,
			Message: fmt.Sprintf("Header mismatch: MCP-Protocol-Version header %q does not match body value %q", hdr, metaVer)}
	}
	if m.isNotification() {
		return true, 0, nil // no header rules are defined for notification POSTs
	}
	if got := rq.r.Header.Get("Mcp-Method"); got != m.Method {
		return true, http.StatusBadRequest, &rpcError{Code: codeHeaderMismatch,
			Message: fmt.Sprintf("Header mismatch: Mcp-Method header %q does not match body value %q", got, m.Method)}
	}
	if m.Method == "tools/call" || m.Method == "resources/read" || m.Method == "prompts/get" {
		want := gjson.GetBytes(m.Params, "name").String()
		if m.Method == "resources/read" {
			want = gjson.GetBytes(m.Params, "uri").String()
		}
		got, ok := decodeHeaderValue(rq.r.Header.Get("Mcp-Name"))
		if !ok || got != want {
			return true, http.StatusBadRequest, &rpcError{Code: codeHeaderMismatch,
				Message: fmt.Sprintf("Header mismatch: Mcp-Name header does not match body value %q", want)}
		}
	}
	return true, 0, nil
}

func unsupported(v string) *rpcError {
	return &rpcError{Code: codeUnsupportedVersion, Message: "Unsupported protocol version",
		Data: map[string]any{"supported": allVersions, "requested": v}}
}

// gjsonKey escapes a _meta key for a gjson path.
func gjsonKey(k string) string {
	return strings.NewReplacer(".", `\.`, "*", `\*`, "?", `\?`).Replace(k)
}

// decodeHeaderValue undoes the Base64 sentinel encoding (=?base64?…?=).
func decodeHeaderValue(v string) (string, bool) {
	if v == "" {
		return "", false
	}
	if strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=") && len(v) >= len("=?base64??=") {
		b, err := base64.StdEncoding.DecodeString(v[len("=?base64?") : len(v)-2])
		return string(b), err == nil
	}
	return v, true
}

func (rq *request) serveOne(w http.ResponseWriter, m *message) {
	if m.JSONRPC != "2.0" || (m.Method == "" && !m.isResponse()) {
		writeRPCError(w, http.StatusBadRequest, m.ID, codeInvalidRequest, "Invalid Request", nil)
		return
	}
	modern, status, e := rq.era(m)
	if e != nil {
		writeRPCError(w, status, m.ID, e.Code, e.Message, e.Data)
		return
	}
	if m.isNotification() || m.isResponse() {
		w.WriteHeader(http.StatusAccepted) // nothing to answer; the gateway sends no requests to clients
		return
	}
	if m.Method == "tools/call" && rq.sse {
		rq.serveCallStream(w, m, modern)
		return
	}
	res, rerr := rq.dispatch(rq.r.Context(), m, modern, nil)
	if rq.r.Context().Err() != nil {
		return
	}
	status = http.StatusOK
	if rerr != nil && rerr.Code == codeMethodNotFound && modern {
		status = http.StatusNotFound // modern servers answer unknown methods with 404 (transport spec)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(responseJSON(m.ID, res, rerr))
}

func (rq *request) serveBatch(w http.ResponseWriter, body []byte) {
	var msgs []message
	if err := json.Unmarshal(body, &msgs); err != nil || len(msgs) == 0 {
		writeRPCError(w, http.StatusBadRequest, nil, codeInvalidRequest, "Invalid Request: empty or malformed batch", nil)
		return
	}
	var out [][]byte
	for i := range msgs {
		m := &msgs[i]
		if m.JSONRPC != "2.0" {
			out = append(out, responseJSON(m.ID, nil, &rpcError{Code: codeInvalidRequest, Message: "Invalid Request"}))
			continue
		}
		modern, _, e := rq.era(m)
		switch {
		case e != nil:
			out = append(out, responseJSON(m.ID, nil, e))
		case modern:
			out = append(out, responseJSON(m.ID, nil, &rpcError{Code: codeInvalidRequest, Message: "Invalid Request: batches are not part of protocol " + latestVersion}))
		case m.isNotification() || m.isResponse():
		default:
			res, rerr := rq.dispatch(rq.r.Context(), m, false, nil)
			out = append(out, responseJSON(m.ID, res, rerr))
		}
	}
	if rq.r.Context().Err() != nil {
		return
	}
	if len(out) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(append(append([]byte("["), bytes.Join(out, []byte(","))...), ']'))
}

// stream is a request-scoped SSE response.
type stream struct {
	mu     sync.Mutex
	w      http.ResponseWriter
	f      http.Flusher
	closed bool
}

func (st *stream) write(s string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return
	}
	_, _ = io.WriteString(st.w, s)
	if st.f != nil {
		st.f.Flush()
	}
}

func (st *stream) event(data []byte) { st.write("event: message\ndata: " + string(data) + "\n\n") }

// serveCallStream answers a tools/call over SSE: keep-alive comments while the
// tool runs, progress notifications when asked for, then the response.
func (rq *request) serveCallStream(w http.ResponseWriter, m *message, modern bool) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	st := &stream{w: w, f: f}
	if f != nil {
		f.Flush()
	}
	ctx, cancel := context.WithCancel(rq.r.Context())
	defer cancel()
	go func() {
		t := time.NewTicker(rq.s.o.KeepAlive)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				st.write(": keepalive\n\n")
			}
		}
	}()
	res, rerr := rq.dispatch(ctx, m, modern, st)
	cancel()
	if rq.r.Context().Err() != nil {
		return // the client closed the stream: that is the cancellation
	}
	st.event(responseJSON(m.ID, res, rerr))
	st.mu.Lock()
	st.closed = true
	st.mu.Unlock()
}

// complete adds what modern results carry: resultType and the server identity.
func (rq *request) complete(o *ordered, modern bool) json.RawMessage {
	if modern {
		meta, _ := o.vals["_meta"].(*ordered)
		if meta == nil {
			meta = newOrdered()
		}
		meta.set(metaServerInfo, rq.s.serverInfo(rq.view.scope))
		o.set("_meta", meta)
		o.set("resultType", "complete")
	}
	return marshal(o)
}

func (rq *request) cacheable(o *ordered, modern bool) {
	if modern {
		o.set("ttlMs", listTTLMillis)
		o.set("cacheScope", "private") // the tool list depends on the caller's token
	}
}

func capabilities() *ordered {
	return newOrdered().set("tools", map[string]bool{"listChanged": false})
}

func (rq *request) dispatch(ctx context.Context, m *message, modern bool, st *stream) (json.RawMessage, *rpcError) {
	switch m.Method {
	case "initialize":
		if modern {
			return nil, &rpcError{Code: codeInvalidRequest, Message: "initialize is not part of protocol " + latestVersion + "; send requests with _meta directly"}
		}
		ver := gjson.GetBytes(m.Params, "protocolVersion").String()
		if !slices.Contains(legacyVersions, ver) {
			ver = legacyVersions[0]
		}
		o := newOrdered().set("protocolVersion", ver).set("capabilities", capabilities()).set("serverInfo", rq.s.serverInfo(rq.view.scope))
		if in := rq.s.instructions(rq.view); in != "" {
			o.set("instructions", in)
		}
		return marshal(o), nil
	case "server/discover":
		o := newOrdered().set("supportedVersions", modernVersions).set("capabilities", capabilities())
		if in := rq.s.instructions(rq.view); in != "" {
			o.set("instructions", in)
		}
		rq.cacheable(o, true)
		return rq.complete(o, true), nil
	case "ping":
		if !modern { // removed in 2026-07-28
			return marshal(newOrdered()), nil
		}
	case "tools/list":
		tools := make([]json.RawMessage, len(rq.view.tools))
		for i, t := range rq.view.tools {
			tools[i] = t.raw
		}
		o := newOrdered().set("tools", tools)
		rq.cacheable(o, modern)
		return rq.complete(o, modern), nil
	case "resources/list", "resources/templates/list", "prompts/list":
		field := map[string]string{"resources/list": "resources", "resources/templates/list": "resourceTemplates", "prompts/list": "prompts"}[m.Method]
		o := newOrdered().set(field, []any{})
		rq.cacheable(o, modern)
		return rq.complete(o, modern), nil
	case "tools/call":
		return rq.call(ctx, m, modern, st)
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "Method not found: " + m.Method}
}

func (rq *request) call(ctx context.Context, m *message, modern bool, st *stream) (json.RawMessage, *rpcError) {
	p := gjson.ParseBytes(m.Params)
	name := p.Get("name").String()
	t, ok := rq.view.tool(name)
	fn := rq.s.tools[name]
	if !ok || fn == nil {
		var names []string
		for _, t := range rq.view.tools {
			names = append(names, t.name)
		}
		return nil, &rpcError{Code: codeInvalidParams, Message: "Unknown tool: " + name, Data: map[string]any{"availableTools": names}}
	}
	args := p.Get("arguments")
	if args.Exists() && !args.IsObject() && args.Type != gjson.Null {
		return nil, &rpcError{Code: codeInvalidParams, Message: "arguments must be an object"}
	}
	cc := &callCtx{rq: rq, provider: t.provider, tool: name}
	if tokRaw := p.Get("_meta.progressToken"); st != nil && (tokRaw.Type == gjson.String || tokRaw.Type == gjson.Number) {
		var n atomic.Int64
		progressToken := json.RawMessage(tokRaw.Raw)
		cc.progress = func(msg string) {
			st.event(notificationJSON("notifications/progress", newOrdered().
				set("progressToken", progressToken).set("progress", n.Add(1)).set("message", msg)))
		}
	}
	res := fn(ctx, cc, args)
	if res.rpcErr != nil {
		return nil, res.rpcErr
	}
	return rq.complete(res.render(), modern), nil
}

// nextRequestID gives every upstream call of one MCP request its own log ID.
func (rq *request) nextRequestID() string {
	base := httpx.RequestIDFrom(rq.r.Context())
	if n := rq.calls.Add(1); n > 1 {
		return base + "-" + strconv.Itoa(int(n))
	}
	return base
}
