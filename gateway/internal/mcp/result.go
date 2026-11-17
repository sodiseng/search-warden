package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/sodiseng/search-warden/gateway/internal/relay"
)

// result is a CallToolResult under construction.
type result struct {
	meta       *ordered // result _meta (e.g. Exa's ai.exa/usage)
	content    []block
	structured json.RawMessage
	isError    *bool // nil: omitted (the official Exa server omits it on success)
	// rpcErr answers the call with a JSON-RPC error instead of a result (the
	// official Tavily server does this for arguments its schema rejects).
	rpcErr *rpcError
}

type block struct {
	text string
	meta *ordered // content _meta (e.g. Exa's searchTime)
}

func boolp(b bool) *bool { return &b }

func textResult(text string) *result { return &result{content: []block{{text: text}}} }

func errorResult(text string) *result {
	return &result{content: []block{{text: text}}, isError: boolp(true)}
}

func (r *result) render() *ordered {
	o := newOrdered()
	if r.meta != nil && r.meta.len() > 0 {
		o.set("_meta", r.meta)
	}
	content := make([]*ordered, len(r.content))
	for i, b := range r.content {
		c := newOrdered().set("type", "text").set("text", b.text)
		if b.meta != nil && b.meta.len() > 0 {
			c.set("_meta", b.meta)
		}
		content[i] = c
	}
	o.set("content", content)
	if len(r.structured) > 0 {
		o.set("structuredContent", r.structured)
	}
	if r.isError != nil {
		o.set("isError", *r.isError)
	}
	return o
}

// callCtx is one tool call.
type callCtx struct {
	rq       *request
	provider string
	tool     string
	progress func(msg string) // nil unless the client asked for progress over SSE
}

func (cc *callCtx) report(msg string) {
	if cc.progress != nil {
		cc.progress(msg)
	}
}

// upstream is the answer of one REST call made for a tool.
type upstream struct {
	status  int
	body    []byte
	gateway bool // produced by the gateway (no key, rate limited…), not upstream
}

func (u *upstream) ok() bool { return u.status >= 200 && u.status < 300 }

// message is the error text of a failed call.
func (u *upstream) message() string {
	b := gjson.ParseBytes(u.body)
	for _, p := range []string{"detail.error", "error", "message", "detail"} {
		if v := b.Get(p); v.Exists() && v.Type == gjson.String && v.String() != "" {
			return v.String()
		}
	}
	if s := string(bytes.TrimSpace(u.body)); s != "" && len(s) < 500 {
		return s
	}
	return http.StatusText(u.status)
}

// rest sends one REST call through the relay (key choice, retries, egress,
// billing and the request log all happen there).
func (cc *callCtx) rest(ctx context.Context, method, path string, body []byte, extra http.Header, quiet bool) *upstream {
	s := cc.rq.s
	p, ok := s.relay.Provider(cc.provider)
	if !ok {
		return &upstream{status: http.StatusInternalServerError, gateway: true, body: []byte(`{"error":"provider not available"}`)}
	}
	op, ok := p.Match(method, path)
	if !ok {
		return &upstream{status: http.StatusInternalServerError, gateway: true, body: []byte(`{"error":"endpoint not allowed: ` + method + " " + path + `"}`)}
	}
	snap := s.conf.Snapshot()
	out, injected, err := p.RewriteRequest(op, body, snap.Config.Upstream.PreciseBilling)
	if err != nil {
		return &upstream{status: http.StatusBadRequest, gateway: true, body: []byte(`{"error":"could not prepare the request body"}`)}
	}
	res := s.relay.Do(ctx, &relay.Call{Provider: p, Op: op, Body: out, RawBody: body, Injected: injected,
		Accept: "application/json", Extra: extra, Token: cc.rq.tok, Surface: "mcp", ClientIP: cc.rq.ip,
		UserAgent: cc.rq.r.UserAgent(), RequestID: cc.rq.nextRequestID(), Quiet: quiet})
	return &upstream{status: res.Status, body: res.Body, gateway: res.Gateway}
}
