package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Tavily tools (ARCHITECTURE.md §8.2). The official remote server (tavily-mcp
// 4.1.5, checked 2026-11-10) validates the arguments against the tool schema
// (validate.go), returns the API's JSON plus "auth_mode" as both the text and
// the structuredContent of every result, and an {"error","status","detail",
// "documentation"} object when the API answers with an error.

var tavilyDocs = map[string]string{
	"search":   "https://docs.tavily.com/documentation/api-reference/endpoint/search",
	"extract":  "https://docs.tavily.com/documentation/api-reference/endpoint/extract",
	"crawl":    "https://docs.tavily.com/documentation/api-reference/endpoint/crawl",
	"map":      "https://docs.tavily.com/documentation/api-reference/endpoint/map",
	"research": "https://docs.tavily.com/documentation/api-reference/endpoint/research",
}

var tavilyFailed = map[string]string{
	"search": "Search failed", "extract": "Extract failed", "crawl": "Crawl failed", "map": "Map failed", "research": "Research request failed",
}

// tavilyAuthMode is what the official server appends to every successful
// result for a caller that brought an API key.
const tavilyAuthMode = "keyed"

// tavilyArgs validates the call like the official server: a violation of the
// tool's schema is answered with a JSON-RPC error, and lax values ("5", "yes")
// become their declared type.
func tavilyArgs(cc *callCtx, args gjson.Result) (gjson.Result, *result) {
	t, _ := cc.rq.view.tool(cc.tool)
	clean, msg := validateArgs(cc.tool, gjson.GetBytes(t.raw, "inputSchema"), args)
	if msg != "" {
		return gjson.Result{}, &result{rpcErr: &rpcError{Code: codeInternal, Message: msg}}
	}
	return gjson.ParseBytes(marshal(clean)), nil
}

func (s *Server) registerTavily() {
	for tool, endpoint := range map[string]string{"tavily_search": "search", "tavily_extract": "extract", "tavily_crawl": "crawl", "tavily_map": "map"} {
		s.tools[tool] = func(ctx context.Context, cc *callCtx, args gjson.Result) *result {
			return tavilySimple(ctx, cc, endpoint, args)
		}
	}
	s.tools["tavily_research"] = tavilyResearch
}

// tavilyBody turns validated tool arguments into the API request, with
// DEFAULT_PARAMETERS filling only what the call left out. As observed on the
// official server (2026-11-10, from the request bodies the API echoes in its
// errors): search drops every empty value (null, "", []), so an empty query
// reaches the API as a missing one; the other tools send "" and [] as they are
// (an empty url is answered "Invalid URL.") and drop only nulls.
func tavilyBody(cc *callCtx, args gjson.Result, dropEmpty bool) *ordered {
	body := newOrdered()
	add := func(k string, v gjson.Result) {
		if v.Type == gjson.Null || (dropEmpty && empty(v)) || body.has(k) {
			return
		}
		body.set(k, json.RawMessage(v.Raw))
	}
	args.ForEach(func(k, v gjson.Result) bool { add(k.String(), v); return true })
	if d := cc.rq.defaults; d.Exists() {
		props := cc.toolProperties()
		d.ForEach(func(k, v gjson.Result) bool {
			if props[k.String()] {
				add(k.String(), v)
			}
			return true
		})
	}
	return body
}

func empty(v gjson.Result) bool {
	switch {
	case !v.Exists(), v.Type == gjson.Null:
		return true
	case v.Type == gjson.String:
		return v.String() == ""
	case v.IsArray():
		return len(v.Array()) == 0
	}
	return false
}

// toolProperties are the parameter names the tool's official schema declares.
func (cc *callCtx) toolProperties() map[string]bool {
	out := map[string]bool{}
	if t, ok := cc.rq.view.tool(cc.tool); ok {
		gjson.GetBytes(t.raw, "inputSchema.properties").ForEach(func(k, _ gjson.Result) bool {
			out[k.String()] = true
			return true
		})
	}
	return out
}

// tavilyResult renders an API answer the way the official server does.
func tavilyResult(endpoint string, u *upstream) *result {
	if u.gateway {
		return errorResult(u.message())
	}
	if u.ok() {
		var buf bytes.Buffer
		if json.Compact(&buf, u.body) != nil || !gjson.ValidBytes(u.body) {
			return errorResult("Tavily returned a response that is not JSON")
		}
		out := buf.Bytes()
		if gjson.ParseBytes(out).IsObject() && !gjson.GetBytes(out, "auth_mode").Exists() {
			if with, err := sjson.SetBytes(out, "auth_mode", tavilyAuthMode); err == nil {
				out = with
			}
		}
		return &result{content: []block{{text: string(out)}}, structured: out, isError: boolp(false)}
	}
	detail := json.RawMessage(nil)
	if b := gjson.ParseBytes(u.body); gjson.ValidBytes(u.body) && b.Get("detail").Exists() {
		detail = json.RawMessage(b.Get("detail").Raw)
	} else if gjson.ValidBytes(u.body) {
		detail = json.RawMessage(bytes.TrimSpace(u.body))
	} else {
		detail = marshal(string(u.body))
	}
	o := newOrdered().set("error", tavilyFailed[endpoint]).set("status", u.status).set("detail", detail).set("documentation", tavilyDocs[endpoint])
	text := marshal(o)
	return &result{content: []block{{text: string(text)}}, structured: text, isError: boolp(false)}
}

func tavilySimple(ctx context.Context, cc *callCtx, endpoint string, args gjson.Result) *result {
	args, bad := tavilyArgs(cc, args)
	if bad != nil {
		return bad
	}
	body := tavilyBody(cc, args, endpoint == "search")
	if endpoint == "search" {
		// As in the official server: explicit dates replace a relative time range.
		// (It sends a country together with any topic; verified 2026-11-10.)
		if (body.has("start_date") || body.has("end_date")) && body.has("time_range") {
			body.del("time_range")
		}
	}
	return tavilyResult(endpoint, cc.rest(ctx, http.MethodPost, "/"+endpoint, marshal(body), nil, false))
}

// tavilyResearch creates a research task and polls it until it ends, like the
// official server: 2 s first, ×1.5 up to 10 s; mini gives up after 5 minutes,
// other models after 15.
func tavilyResearch(ctx context.Context, cc *callCtx, args gjson.Result) *result {
	args, bad := tavilyArgs(cc, args)
	if bad != nil {
		return bad
	}
	body := tavilyBody(cc, args, false)
	if !body.has("model") {
		body.set("model", "auto")
	}
	created := cc.rest(ctx, http.MethodPost, "/research", marshal(body), nil, false)
	if !created.ok() {
		return tavilyResult("research", created)
	}
	id := gjson.GetBytes(created.body, "request_id").String()
	if id == "" {
		return errorResult("Research Error: No request_id returned from research endpoint. Documentation: " + tavilyDocs["research"])
	}
	o := cc.rq.s.o
	wait := o.ResearchOtherWait
	if args.Get("model").String() == "mini" {
		wait = o.ResearchMiniWait
	}
	deadline := o.Now().Add(wait)
	interval := o.ResearchPollStart
	cc.report("research " + id + " started")
	for {
		if !o.Sleep(ctx, interval) {
			return errorResult("Research Error: the call was cancelled")
		}
		poll := cc.rest(ctx, http.MethodGet, "/research/"+url.PathEscape(id), nil, nil, true)
		if !poll.ok() {
			if poll.status == http.StatusNotFound {
				return errorResult("Research Error: Research task not found")
			}
			if poll.gateway || poll.status >= 500 {
				if o.Now().After(deadline) {
					return errorResult("Research Error: " + poll.message())
				}
				continue // transient: keep polling the same task
			}
			return tavilyResult("research", poll)
		}
		status := gjson.GetBytes(poll.body, "status").String()
		cc.report("research " + id + " status=" + status)
		switch status {
		case "completed":
			return tavilyResult("research", poll)
		case "failed":
			r := tavilyResult("research", poll)
			r.isError = boolp(true)
			return r
		}
		if !o.Now().Add(interval).Before(deadline) {
			return errorResult("Research Error: Research task timed out (request_id " + id + "). Documentation: " + tavilyDocs["research"])
		}
		interval = min(time.Duration(float64(interval)*1.5), o.ResearchPollMax)
	}
}
