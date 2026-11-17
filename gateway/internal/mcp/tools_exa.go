package mcp

// Exa tools (ARCHITECTURE.md §8.2).
//
// The argument mapping, output formats and the response sanitizer are ported
// from exa-labs/exa-mcp-server (MIT License, Copyright (c) Exa Labs), files
// src/tools/{webSearch,webFetch,webSearchAdvanced,agentRun,highlights}.ts,
// src/tools/searchNotes.ts and src/utils/{exaResponseSanitizer,errorHandler,
// agentErrorHandler}.ts at commit 93c0d5e — and checked against the official
// remote server (3.5.0, 2026-11-09), which is newer: web_search_exa gained an
// `objective` argument and every result reports its cost in
// _meta["ai.exa/usage"]. See NOTICE.

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/sodiseng/search-warden/gateway/internal/provider"
)

const (
	exaDynamicHighlightsBeta = "dynamic-highlights-2026-08-28"
	exaDefaultResults        = 10
	exaDefaultFetchChars     = 3000
	exaBillingURL            = "https://dashboard.exa.ai/billing"
	exaKeysURL               = "https://dashboard.exa.ai/api-keys"
)

func (s *Server) registerExa() {
	s.tools["web_search_exa"] = exaWebSearch
	s.tools["web_fetch_exa"] = exaWebFetch
	s.tools["web_search_advanced_exa"] = exaAdvancedSearch
	s.tools["agent_run"] = exaAgentRun
}

// ---- lenient argument parsing (validation.ts) --------------------------------

// lenientString coerces numbers and booleans to strings and trims; "" if absent.
func lenientString(v gjson.Result) string {
	switch v.Type {
	case gjson.String:
		return strings.TrimSpace(v.String())
	case gjson.Number, gjson.True, gjson.False:
		return v.Raw
	}
	return ""
}

// lenientNumber accepts numbers and numeric strings; ok=false otherwise.
func lenientNumber(v gjson.Result) (float64, bool) {
	switch v.Type {
	case gjson.Number:
		return v.Float(), true
	case gjson.String:
		f, err := strconv.ParseFloat(strings.TrimSpace(v.String()), 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return 0, false
}

func lenientBool(v gjson.Result) (bool, bool) {
	switch v.Type {
	case gjson.True:
		return true, true
	case gjson.False:
		return false, true
	case gjson.String:
		switch strings.ToLower(v.String()) {
		case "true", "1", "yes":
			return true, true
		case "false", "0", "no":
			return false, true
		}
	}
	return false, false
}

func stringList(v gjson.Result) []string {
	var out []string
	if v.IsArray() {
		for _, x := range v.Array() {
			if x.Type == gjson.String {
				out = append(out, x.String())
			}
		}
	}
	return out
}

// number renders a JSON number like JavaScript does (integers without ".0").
func number(f float64) json.RawMessage {
	return json.RawMessage(strconv.FormatFloat(f, 'f', -1, 64))
}

func invalidArgs(msg string) *result {
	return errorResult("MCP error -32602: Input validation error: " + msg)
}

// ---- shared output ------------------------------------------------------------

// exaError is formatToolError: "<tool> error (<status>): <message>".
func exaError(tool string, u *upstream) *result {
	return errorResult(tool + " error (" + strconv.Itoa(u.status) + "): " + u.message())
}

// exaMeta reports the call's cost like the official server: _meta["ai.exa/usage"].
func exaMeta(body []byte) *ordered {
	if c := gjson.GetBytes(body, "costDollars.total"); c.Type == gjson.Number {
		return newOrdered().set("ai.exa/usage", newOrdered().set("costDollars", json.RawMessage(c.Raw)))
	}
	return nil
}

func searchTimeMeta(body []byte) *ordered {
	if t := gjson.GetBytes(body, "searchTime"); t.Type == gjson.Number {
		return newOrdered().set("searchTime", json.RawMessage(t.Raw))
	}
	return nil
}

// notices are messages written for the model; warnings are for developers and never shown.
func notices(body []byte) []string {
	var out []string
	for _, n := range gjson.GetBytes(body, "notices").Array() {
		if n.Get("code").Type == gjson.String && n.Get("path").Type == gjson.String && n.Get("message").Type == gjson.String {
			out = append(out, n.Get("message").String())
		}
	}
	return out
}

func withNotes(text string, notes []string) string {
	if len(notes) == 0 {
		return text
	}
	lead := make([]string, len(notes))
	for i, n := range notes {
		lead[i] = "Note: " + n
	}
	if text == "" {
		return strings.Join(lead, "\n")
	}
	return strings.Join(lead, "\n") + "\n\n" + text
}

func betaHeader(dynamic bool) http.Header {
	if !dynamic {
		return nil
	}
	return http.Header{"Exa-Beta": {exaDynamicHighlightsBeta}}
}

// ---- web_search_exa -----------------------------------------------------------

var categoryRe = regexp.MustCompile(`(?i)\bcategory:(company|publication|news|personal\s*site|people)\b`)
var spacesRe = regexp.MustCompile(`\s+`)

func exaWebSearch(ctx context.Context, cc *callCtx, args gjson.Result) *result {
	query := lenientString(args.Get("query"))
	if query == "" {
		return invalidArgs("query: a non-empty string is required")
	}
	category := ""
	if m := categoryRe.FindStringSubmatchIndex(query); m != nil {
		category = spacesRe.ReplaceAllString(strings.ToLower(query[m[2]:m[3]]), " ")
		query = strings.TrimSpace(spacesRe.ReplaceAllString(query[:m[0]]+query[m[1]:], " "))
	}
	n := exaDefaultResults
	if f, ok := lenientNumber(args.Get("numResults")); ok && f > 0 {
		n = int(f)
	}
	stype := cc.rq.sType
	if stype == "" {
		stype = "auto"
	}
	body := newOrdered().set("query", query).set("type", stype).set("numResults", n)
	if category != "" {
		body.set("category", category)
	}
	if obj := lenientString(args.Get("objective")); obj != "" {
		body.set("objective", obj)
	}
	var highlights any = true
	if cc.rq.dynamic {
		highlights = map[string]bool{"dynamic": true}
	}
	body.set("contents", newOrdered().set("highlights", highlights))

	u := cc.rest(ctx, http.MethodPost, "/search", marshal(body), betaHeader(cc.rq.dynamic), false)
	if !u.ok() {
		return exaError(cc.tool, u)
	}
	notes := notices(u.body)
	results := gjson.GetBytes(u.body, "results").Array()
	if len(results) == 0 {
		r := textResult(withNotes("No search results found. Please try a different query.", notes))
		r.meta = exaMeta(u.body)
		return r
	}
	parts := make([]string, 0, len(results))
	for _, r := range results {
		lines := []string{
			"Title: " + orNA(r.Get("title")),
			"URL: " + r.Get("url").String(),
			"Published: " + orNA(r.Get("publishedDate")),
			"Author: " + orNA(r.Get("author")),
		}
		if hl := stringList(r.Get("highlights")); len(hl) > 0 {
			lines = append(lines, "Highlights:\n"+strings.Join(hl, "\n"))
		} else if t := r.Get("text"); t.Type == gjson.String && t.String() != "" {
			lines = append(lines, "Text: "+t.String())
		}
		parts = append(parts, strings.Join(lines, "\n"))
	}
	return &result{meta: exaMeta(u.body), content: []block{{text: withNotes(strings.Join(parts, "\n\n---\n\n"), notes), meta: searchTimeMeta(u.body)}}}
}

func orNA(v gjson.Result) string {
	if v.Type == gjson.String && v.String() != "" {
		return v.String()
	}
	return "N/A"
}

// ---- web_fetch_exa ------------------------------------------------------------

func exaWebFetch(ctx context.Context, cc *callCtx, args gjson.Result) *result {
	var urls []string
	switch v := args.Get("urls"); {
	case v.IsArray():
		for _, x := range v.Array() {
			if x.Type != gjson.String {
				return invalidArgs("urls: expected an array of strings")
			}
			urls = append(urls, x.String())
		}
	case v.Type == gjson.String:
		if p := gjson.Parse(v.String()); gjson.Valid(v.String()) && p.IsArray() {
			urls = stringList(p)
		} else {
			urls = []string{v.String()}
		}
	default:
		return invalidArgs("urls: expected an array of strings")
	}
	maxChars := float64(exaDefaultFetchChars)
	if f, ok := lenientNumber(args.Get("maxCharacters")); ok && f >= 1 {
		maxChars = f
	}
	body := newOrdered().set("urls", urls).set("text", newOrdered().set("maxCharacters", number(maxChars)))
	u := cc.rest(ctx, http.MethodPost, "/contents", marshal(body), nil, false)
	if !u.ok() {
		return exaError(cc.tool, u)
	}
	var errs []string
	for _, st := range gjson.GetBytes(u.body, "statuses").Array() {
		if st.Get("status").String() == "error" {
			tag := st.Get("error.tag").String()
			if tag == "" {
				tag = "unknown error"
			}
			errs = append(errs, st.Get("id").String()+"\x00"+tag)
		}
	}
	results := gjson.GetBytes(u.body, "results").Array()
	if len(results) == 0 {
		if len(errs) > 0 {
			msgs := make([]string, len(errs))
			for i, e := range errs {
				id, tag, _ := strings.Cut(e, "\x00")
				msgs[i] = id + ": " + tag
			}
			return errorResult("Error fetching URL(s): " + strings.Join(msgs, "; "))
		}
		r := textResult("No content found for the provided URL(s).")
		return r
	}
	var lines []string
	for _, r := range results {
		title := r.Get("title").String()
		if title == "" {
			title = "(no title)"
		}
		lines = append(lines, "# "+title, "URL: "+r.Get("url").String())
		if d := r.Get("publishedDate").String(); d != "" {
			lines = append(lines, "Published: "+strings.SplitN(d, "T", 2)[0])
		}
		if a := r.Get("author").String(); a != "" {
			lines = append(lines, "Author: "+a)
		}
		lines = append(lines, "")
		if t := r.Get("text").String(); t != "" {
			lines = append(lines, t)
		}
		lines = append(lines, "")
	}
	for _, e := range errs {
		id, tag, _ := strings.Cut(e, "\x00")
		lines = append(lines, "Error fetching "+id+": "+tag)
	}
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	return &result{meta: exaMeta(u.body), content: []block{{text: text, meta: searchTimeMeta(u.body)}}}
}

// ---- web_search_advanced_exa ----------------------------------------------------

// Fields the `lite` search type serves; it rejects requests setting others.
var liteFields = map[string]bool{"query": true, "type": true, "numResults": true, "category": true, "contents": true,
	"includeDomains": true, "excludeDomains": true, "startPublishedDate": true, "endPublishedDate": true, "userLocation": true, "moderation": true}
var liteContents = map[string]bool{"text": true, "highlights": true}
var liteParam = map[string]string{"contents.context": "contextMaxCharacters", "contents.summary": "enableSummary",
	"contents.maxAgeHours": "maxAgeHours", "contents.livecrawlTimeout": "livecrawlTimeout", "contents.subpages": "subpages", "contents.subpageTarget": "subpageTarget"}

var advCategories = map[string]bool{"company": true, "publication": true, "news": true, "pdf": true, "github": true,
	"personal site": true, "people": true, "financial report": true}

func exaAdvancedSearch(ctx context.Context, cc *callCtx, args gjson.Result) *result {
	query := lenientString(args.Get("query"))
	if query == "" {
		return invalidArgs("query: a non-empty string is required")
	}
	num := func(k string) (float64, bool) { return lenientNumber(args.Get(k)) }
	pos := func(k string) (float64, bool) { f, ok := num(k); return f, ok && f >= 1 }
	str := func(k string) string {
		if v := args.Get(k); v.Type == gjson.String {
			return v.String()
		}
		return ""
	}
	stype := str("type")
	switch stype {
	case "", "auto", "lite", "fast", "instant":
	default:
		return invalidArgs("type: must be one of auto, lite, fast, instant")
	}
	if c := str("category"); c != "" && !advCategories[c] {
		return invalidArgs("category: unsupported value " + strconv.Quote(c))
	}
	enableHighlights, _ := lenientBool(args.Get("enableHighlights"))
	enableSummary, _ := lenientBool(args.Get("enableSummary"))

	contents := newOrdered()
	if f, ok := pos("textMaxCharacters"); ok {
		contents.set("text", newOrdered().set("maxCharacters", number(f)))
	} else if !enableHighlights && !enableSummary {
		contents.set("text", true)
	}
	if f, ok := num("maxAgeHours"); ok {
		contents.set("maxAgeHours", number(f))
	} else if stype != "lite" {
		contents.set("livecrawl", "fallback") // lite serves cached content only
	}
	if f, ok := num("livecrawlTimeout"); ok && f != 0 {
		contents.set("livecrawlTimeout", number(f))
	}
	if f, ok := pos("contextMaxCharacters"); ok {
		contents.set("context", newOrdered().set("maxCharacters", number(f)))
	}
	if enableSummary {
		if q := str("summaryQuery"); q != "" {
			contents.set("summary", newOrdered().set("query", q))
		} else {
			contents.set("summary", true)
		}
	}
	if enableHighlights {
		h := newOrdered()
		for _, k := range [][2]string{{"highlightsMaxCharacters", "maxCharacters"}, {"highlightsNumSentences", "numSentences"}, {"highlightsPerUrl", "highlightsPerUrl"}} {
			if f, ok := num(k[0]); ok {
				h.set(k[1], number(f))
			}
		}
		if q := str("highlightsQuery"); q != "" {
			h.set("query", q)
		}
		if cc.rq.dynamic {
			h.set("dynamic", true)
		}
		contents.set("highlights", h)
	}
	if f, ok := num("subpages"); ok && f != 0 {
		contents.set("subpages", number(f))
	}
	if t := stringList(args.Get("subpageTarget")); args.Get("subpageTarget").IsArray() {
		contents.set("subpageTarget", t)
	}

	n := float64(exaDefaultResults)
	if f, ok := num("numResults"); ok && f != 0 {
		n = f
	}
	t := stype
	if t == "" {
		t = "auto"
	}
	req := newOrdered().set("query", query).set("type", t).set("numResults", number(n)).set("contents", contents)
	if c := str("category"); c != "" {
		req.set("category", c)
	}
	for _, k := range []string{"includeDomains", "excludeDomains"} {
		if l := stringList(args.Get(k)); len(l) > 0 {
			req.set(k, l)
		}
	}
	for _, k := range []string{"startPublishedDate", "endPublishedDate", "startCrawlDate", "endCrawlDate"} {
		if v := str(k); v != "" {
			req.set(k, v)
		}
	}
	for _, k := range []string{"includeText", "excludeText"} {
		if l := stringList(args.Get(k)); len(l) > 0 {
			req.set(k, l)
		}
	}
	if v := str("userLocation"); v != "" {
		req.set("userLocation", v)
	}
	if b, ok := lenientBool(args.Get("moderation")); ok {
		req.set("moderation", b)
	}
	if l := stringList(args.Get("additionalQueries")); len(l) > 0 {
		req.set("additionalQueries", l)
	}

	if t == "lite" {
		var bad []string
		seen := map[string]bool{}
		addBad := func(f string) {
			if p, ok := liteParam[f]; ok {
				f = p
			}
			if !seen[f] {
				seen[f] = true
				bad = append(bad, f)
			}
		}
		for _, k := range req.keys {
			if !liteFields[k] {
				addBad(k)
			}
		}
		for _, k := range contents.keys {
			if !liteContents[k] {
				addBad("contents." + k)
			}
		}
		if len(bad) > 0 {
			pronoun := "them"
			if len(bad) == 1 {
				pronoun = "it"
			}
			return errorResult("web_search_advanced_exa error: search type 'lite' does not support " + strings.Join(bad, ", ") +
				". Remove " + pronoun + ", or set type to 'auto'.")
		}
	}
	if cc.rq.dynamic && enableHighlights && args.Get("highlightsMaxCharacters").Exists() {
		if _, ok := num("highlightsMaxCharacters"); ok {
			return errorResult("web_search_advanced_exa error: this server uses dynamic highlights (beta), which share one budget across all results, so highlightsMaxCharacters is not supported. Remove it.")
		}
	}

	u := cc.rest(ctx, http.MethodPost, "/search", marshal(req), betaHeader(cc.rq.dynamic && enableHighlights), false)
	if !u.ok() {
		return exaError(cc.tool, u)
	}
	clean := sanitizeResponse(gjson.ParseBytes(u.body))
	if notes := notices(u.body); len(notes) > 0 {
		clean = withNoticesBeforeResults(clean, notes)
	}
	return &result{meta: exaMeta(u.body), content: []block{{text: string(marshal(clean)), meta: searchTimeMeta(u.body)}}}
}

func withNoticesBeforeResults(o *ordered, notes []string) *ordered {
	out := newOrdered()
	placed := false
	for _, k := range o.keys {
		if k == "results" {
			out.set("notices", notes)
			placed = true
		}
		out.set(k, o.vals[k])
	}
	if !placed {
		out.set("notices", notes)
	}
	return out
}

// sanitizeResponse keeps only the fields the official server shows a model,
// in its order (exaResponseSanitizer.ts sanitizeTopLevelResponse).
func sanitizeResponse(v gjson.Result) *ordered {
	o := newOrdered()
	for _, k := range []string{"requestId", "autopromptString", "autoDate", "resolvedSearchType", "context"} {
		if f := v.Get(k); f.Type == gjson.String {
			o.set(k, json.RawMessage(f.Raw))
		}
	}
	if out := sanitizeOutput(v.Get("output")); out != nil {
		o.set("output", out)
	}
	var statuses []*ordered
	for _, s := range v.Get("statuses").Array() {
		if s.Get("id").Type == gjson.String && s.Get("status").Type == gjson.String && s.Get("source").Type == gjson.String {
			statuses = append(statuses, newOrdered().set("id", s.Get("id").String()).set("status", s.Get("status").String()).set("source", s.Get("source").String()))
		}
	}
	if len(statuses) > 0 {
		o.set("statuses", statuses)
	}
	if rs := sanitizeResults(v.Get("results")); len(rs) > 0 {
		o.set("results", rs)
	}
	if t := v.Get("searchTime"); t.Type == gjson.Number {
		o.set("searchTime", json.RawMessage(t.Raw))
	}
	if c := v.Get("costDollars"); c.IsObject() {
		o.set("costDollars", stripSensitive(c))
	}
	return o
}

func sanitizeResults(v gjson.Result) []*ordered {
	var out []*ordered
	for _, r := range v.Array() {
		if s := sanitizeResult(r); s != nil {
			out = append(out, s)
		}
	}
	return out
}

func sanitizeResult(r gjson.Result) *ordered {
	if !r.IsObject() {
		return nil
	}
	o := newOrdered()
	for _, k := range []string{"id", "url", "publishedDate", "author", "text", "summary", "image", "favicon"} {
		if f := r.Get(k); f.Type == gjson.String {
			o.set(k, json.RawMessage(f.Raw))
		}
	}
	if t := r.Get("title"); t.Type == gjson.String || t.Type == gjson.Null && t.Exists() {
		o.set("title", json.RawMessage(t.Raw))
	}
	if s := r.Get("score"); s.Type == gjson.Number {
		o.set("score", json.RawMessage(s.Raw))
	}
	if h := stringList(r.Get("highlights")); len(h) > 0 {
		o.set("highlights", h)
	}
	var scores []json.RawMessage
	for _, x := range r.Get("highlightScores").Array() {
		if x.Type == gjson.Number {
			scores = append(scores, json.RawMessage(x.Raw))
		}
	}
	if len(scores) > 0 {
		o.set("highlightScores", scores)
	}
	var entities []json.RawMessage
	for _, e := range r.Get("entities").Array() {
		if e.IsObject() {
			entities = append(entities, stripSensitive(e))
		}
	}
	if len(entities) > 0 {
		o.set("entities", entities)
	}
	if ex := r.Get("extras"); ex.IsObject() {
		e := newOrdered()
		if l := stringList(ex.Get("links")); len(l) > 0 {
			e.set("links", l)
		}
		if l := stringList(ex.Get("imageLinks")); len(l) > 0 {
			e.set("imageLinks", l)
		}
		if e.len() > 0 {
			o.set("extras", e)
		}
	}
	if sp := sanitizeResults(r.Get("subpages")); len(sp) > 0 {
		o.set("subpages", sp)
	}
	return o
}

func sanitizeOutput(v gjson.Result) *ordered {
	if !v.IsObject() {
		return nil
	}
	o := newOrdered()
	if c := v.Get("content"); c.Exists() {
		o.set("content", stripSensitive(c))
	}
	var grounding []*ordered
	for _, g := range v.Get("grounding").Array() {
		if !g.IsObject() {
			continue
		}
		var cites []*ordered
		for _, c := range g.Get("citations").Array() {
			if c.Get("url").Type == gjson.String && c.Get("title").Type == gjson.String {
				cites = append(cites, newOrdered().set("url", c.Get("url").String()).set("title", c.Get("title").String()))
			}
		}
		e := newOrdered().set("citations", nonNil(cites))
		if f := g.Get("field"); f.Type == gjson.String {
			e.set("field", f.String())
		}
		if f := g.Get("confidence"); f.Type == gjson.String {
			e.set("confidence", f.String())
		}
		grounding = append(grounding, e)
	}
	if len(grounding) > 0 {
		o.set("grounding", grounding)
	}
	if o.len() == 0 {
		return nil
	}
	return o
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// stripSensitive drops requestTags anywhere in a value.
func stripSensitive(v gjson.Result) json.RawMessage {
	switch {
	case v.IsObject():
		o := newOrdered()
		v.ForEach(func(k, x gjson.Result) bool {
			if k.String() != "requestTags" {
				o.set(k.String(), stripSensitive(x))
			}
			return true
		})
		return marshal(o)
	case v.IsArray():
		var out []json.RawMessage
		for _, x := range v.Array() {
			out = append(out, stripSensitive(x))
		}
		return marshal(nonNil(out))
	}
	return json.RawMessage(v.Raw)
}

// ---- agent_run --------------------------------------------------------------------

func agentError(u *upstream) *result {
	text := "agent_run error (" + strconv.Itoa(u.status) + "): " + u.message()
	var guidance string
	switch u.status {
	case 400:
		guidance = "Check the run body and outputSchema. Use a top-level object schema, bound arrays with maxItems when possible, and use input.data for known rows."
	case 402:
		guidance = "Not retryable: the account is out of credits or over its spending budget, so every agent_run call will be rejected until that changes. Do not call agent_run again; tell the user to add credits or raise the budget at " + exaBillingURL + "."
	case 401, 403:
		guidance = "Authenticate with an Exa API key. API keys are available at " + exaKeysURL + "."
	case 404:
		guidance = "Run not found or not visible to this API key. Verify the agent_run_... ID and account."
	case 429:
		guidance = "Rate or concurrency limit reached. Wait for active Agent runs to finish and avoid submitting duplicate runs."
	}
	if guidance != "" {
		text += "\n\n" + guidance
	}
	return errorResult(text)
}

func exaAgentRun(ctx context.Context, cc *callCtx, args gjson.Result) *result {
	query, runID := args.Get("query"), args.Get("runId")
	hasQuery, hasRun := query.Exists() && query.Type != gjson.Null, runID.Exists() && runID.Type != gjson.Null
	if hasQuery == hasRun {
		return errorResult("agent_run error: Provide exactly one of query or runId.")
	}
	createOpts := []string{"systemPrompt", "outputSchema", "input", "dataSources", "previousRunId", "effort", "budget"}
	o := cc.rq.s.o
	start := o.Now()

	var run gjson.Result
	id := ""
	if hasRun {
		for _, k := range createOpts {
			if v := args.Get(k); v.Exists() && v.Type != gjson.Null {
				return errorResult("agent_run error: A runId resume call accepts only runId; create options apply only to a new query.")
			}
		}
		id = runID.String()
		if !strings.HasPrefix(id, "agent_run_") {
			return invalidArgs("runId: must start with agent_run_")
		}
	} else {
		q := lenientString(query)
		if q == "" {
			return invalidArgs("query: a non-empty string is required")
		}
		body := newOrdered().set("query", q)
		for _, k := range []string{"systemPrompt", "outputSchema", "input"} {
			if v := args.Get(k); v.Exists() && v.Type != gjson.Null {
				body.set(k, json.RawMessage(v.Raw))
			}
		}
		if ds := args.Get("dataSources"); ds.IsArray() {
			var out []*ordered
			for _, d := range ds.Array() {
				if p := d.Get("provider"); p.Type == gjson.String {
					out = append(out, newOrdered().set("provider", p.String()))
				}
			}
			body.set("dataSources", nonNil(out))
		}
		if v := args.Get("previousRunId"); v.Type == gjson.String {
			body.set("previousRunId", v.String())
		}
		effort := "low"
		if v := args.Get("effort"); v.Type == gjson.String && v.String() != "" {
			effort = v.String()
		}
		body.set("effort", effort)
		if v := args.Get("budget"); v.IsObject() {
			body.set("budget", json.RawMessage(v.Raw))
		}
		u := cc.rest(ctx, http.MethodPost, "/agent/runs", marshal(body), nil, false)
		if !u.ok() {
			return agentError(u)
		}
		run = gjson.ParseBytes(u.body)
		id = run.Get("id").String()
		if id == "" {
			return errorResult("agent_run error: the upstream run reported status=" + orUnknown(run.Get("status").String()) + " without a run ID, so the result cannot be resumed or referenced.")
		}
		cc.report("run " + id + " created")
	}

	// Poll until the run ends or the call window closes (official: 4 s, 45 s).
	for !run.Exists() || !provider.AgentRunEnded(run.Get("status").String()) {
		if run.Exists() {
			left := o.AgentCallWindow - o.Now().Sub(start)
			if left <= 0 || !o.Sleep(ctx, min(o.AgentPollInterval, left)) {
				return agentPayload(id, run, "running")
			}
		}
		u := cc.rest(ctx, http.MethodGet, "/agent/runs/"+url.PathEscape(id), nil, nil, true)
		if !u.ok() {
			if u.status == http.StatusNotFound || (!u.gateway && u.status < 500 && u.status != http.StatusTooManyRequests) {
				return agentError(u)
			}
			if !run.Exists() { // a resume call that could not reach the run at all
				return agentError(u)
			}
			continue // transient: the run keeps going upstream; try again within the window
		}
		run = gjson.ParseBytes(u.body)
		cc.report("waiting: run " + id + " status=" + run.Get("status").String())
	}
	return agentPayload(id, run, run.Get("status").String())
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// agentPayload is outcomeToToolContent: one flat object matching the official
// outputSchema, as structuredContent and as compact JSON text.
func agentPayload(id string, run gjson.Result, status string) *result {
	o := newOrdered()
	raw := func(k string) (json.RawMessage, bool) {
		if v := run.Get(k); v.Exists() && v.Type != gjson.Null {
			return json.RawMessage(v.Raw), true
		}
		return nil, false
	}
	var failed bool
	switch status {
	case "completed":
		o.set("success", true).set("id", id).set("status", "completed").set("outputReady", true)
		if v, ok := raw("output"); ok {
			o.set("output", v)
		} else {
			o.set("output", nil)
		}
		if v, ok := raw("stopReason"); ok {
			o.set("stopReason", v)
		}
		for _, k := range []string{"usage", "costDollars"} {
			if v := run.Get(k); v.IsObject() {
				o.set(k, json.RawMessage(v.Raw))
			}
		}
	case "cancelled", "failed":
		o.set("success", false).set("id", id).set("status", status).set("outputReady", false)
		if v, ok := raw("stopReason"); ok {
			o.set("stopReason", v)
		}
		if v := run.Get("error"); status == "failed" && v.IsObject() {
			o.set("error", json.RawMessage(v.Raw))
		}
		failed = status == "failed"
	default: // queued / running: hand the run back to the caller
		o.set("success", true).set("id", id).set("status", "running").set("outputReady", false).
			set("message", "The Agent run is still active. Call agent_run again with runId="+id+" to continue waiting; do not create a replacement run or cancel it unless the user explicitly requests cancellation.")
	}
	text := marshal(o)
	r := &result{content: []block{{text: string(text)}}, structured: text}
	if failed {
		r.content = append(r.content, block{text: "The Agent run failed. Inspect the error above and verify any outputSchema before retrying."})
		r.isError = boolp(true)
	}
	return r
}
