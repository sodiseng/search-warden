package provider

import (
	"context"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// ExaBaseURL is the production REST endpoint.
const ExaBaseURL = "https://api.exa.ai"

// Exa implements Provider for api.exa.ai. Exa has no balance API: spend is
// accounted locally from costDollars and a 402 is authoritative (§10.2).
type Exa struct{ base string }

// NewExa returns an Exa provider; base "" means production.
func NewExa(base string) *Exa {
	if base == "" {
		base = ExaBaseURL
	}
	return &Exa{base: strings.TrimRight(base, "/")}
}

func (e *Exa) ID() string      { return "exa" }
func (e *Exa) BaseURL() string { return e.base }

func (e *Exa) Match(method, path string) (Operation, bool) {
	p := cleanPath(path)
	op := Operation{Provider: "exa", Method: method, Path: p}
	switch {
	case method == http.MethodPost && p == "/search":
		op.Name, op.Billable, op.RetryAfterSend = "search", true, true
	case method == http.MethodPost && p == "/contents":
		op.Name, op.Billable, op.RetryAfterSend = "contents", true, true
	case method == http.MethodPost && p == "/findSimilar":
		op.Name, op.Billable, op.RetryAfterSend = "findSimilar", true, true
	case method == http.MethodPost && p == "/answer":
		op.Name, op.Billable, op.RetryAfterSend = "answer", true, true
	case method == http.MethodPost && p == "/research/v1":
		op.Name, op.Billable, op.Task = "research", true, TaskCreate
	case method == http.MethodGet && strings.HasPrefix(p, "/research/v1/") && strings.Count(p, "/") == 3:
		op.Name, op.RetryAfterSend, op.Task, op.TaskID = "research_get", true, TaskPoll, strings.TrimPrefix(p, "/research/v1/")
	case method == http.MethodPost && p == "/agent/runs":
		// Starting a run is billed when it ends; a resend could start a second run.
		op.Name, op.Billable, op.Task = "agent_run", true, TaskCreate
	case method == http.MethodGet && strings.HasPrefix(p, "/agent/runs/") && strings.Count(p, "/") == 3:
		// Billable: the poll that sees the run end carries its costDollars.
		op.Name, op.Billable, op.RetryAfterSend, op.Task, op.TaskID = "agent_get", true, true, TaskPoll, strings.TrimPrefix(p, "/agent/runs/")
	case method == http.MethodPost && strings.HasPrefix(p, "/agent/runs/") && strings.HasSuffix(p, "/cancel") && strings.Count(p, "/") == 4:
		op.Name, op.RetryAfterSend, op.Task, op.TaskID = "agent_cancel", true, TaskPoll, strings.TrimSuffix(strings.TrimPrefix(p, "/agent/runs/"), "/cancel")
	default:
		return Operation{}, false
	}
	return op, true
}

func (e *Exa) Authorize(h http.Header, key string) { h.Set("x-api-key", key) }

// RewriteRequest leaves Exa bodies untouched: credentials travel in headers and
// every response already includes costDollars.
func (e *Exa) RewriteRequest(op Operation, body []byte, preciseBilling bool) ([]byte, bool, error) {
	return body, false, nil
}

func (e *Exa) RewriteResponse(op Operation, body []byte, injected bool) []byte { return body }

// Classify reads the error tag where Exa documents one (exa.ai/docs/admin/error-codes,
// checked 2026-11-09).
func (e *Exa) Classify(op Operation, status int, h http.Header, body []byte) Outcome {
	reason := errorText(body, "error", "message", "tag")
	tag := gjson.GetBytes(body, "tag").String()
	if status == http.StatusForbidden {
		switch tag {
		case "PROHIBITED_CONTENT", "CONTENT_FILTER_ERROR":
			// Content moderation rejected the request itself: every key would.
			return Outcome{Class: ClassClientError, Status: status, Reason: reason}
		case "FEATURE_DISABLED":
			// The plan lacks the feature: not the key's fault, but a key on another plan may have it.
			return Outcome{Class: ClassClientError, Status: status, Retry: true, Reason: reason}
		case "TEAM_BLOCKED":
			return Outcome{Class: ClassKeyInvalid, Status: status, Retry: true, Reason: reason, TeamWide: true}
		}
	}
	if o, ok := classifyCommon(status, h, body, reason); ok {
		if o.Class == ClassRateLimited { // per-second limits, shared by the whole team
			o.CooldownBase, o.CooldownMax, o.TeamWide = 2*time.Second, time.Minute, true
		}
		return o
	}
	if status == http.StatusPaymentRequired {
		switch tag {
		case "X402_PAYMENT_REQUIRED":
			// Verified live (2026-11): a request without a usable key gets 402 with the
			// x402 payment tag — a credential problem, not an exhausted balance.
			return Outcome{Class: ClassKeyInvalid, Status: status, Retry: true, Reason: "Exa did not accept the API key (x402 payment required)"}
		case "API_KEY_BUDGET_EXCEEDED": // this key's own budget; its siblings may still spend
			return Outcome{Class: ClassKeyExhausted, Status: status, Retry: true, Reason: reason}
		}
		// NO_MORE_CREDITS / TEAM_BUDGET_EXCEEDED: the team's balance is gone for all its keys.
		return Outcome{Class: ClassKeyExhausted, Status: status, Retry: true, Reason: reason, TeamWide: true}
	}
	return Outcome{Class: ClassClientError, Status: status, Reason: reason}
}

func (e *Exa) ActualCost(op Operation, body []byte) (Cost, bool) {
	// An agent run reports its (fixed-effort) price from the moment it is
	// created — verified live 2026-11-10: POST /agent/runs answered status
	// "running" with costDollars.total already set — and again on every poll.
	// It is billed once, by the first answer that shows the run ended.
	if (op.Name == "agent_run" || op.Name == "agent_get") && !AgentRunEnded(gjson.GetBytes(body, "status").String()) {
		return Cost{}, false
	}
	r := gjson.GetBytes(body, "costDollars.total")
	if !r.Exists() {
		return Cost{}, false
	}
	return Cost{MicroUSD: int64(math.Round(r.Float() * 1e6))}, true
}

// EstimateCost is a rough upper bound used only when a response lacks costDollars.
func (e *Exa) EstimateCost(op Operation, body []byte) Cost {
	n := int(gjson.GetBytes(body, "numResults").Int())
	if n <= 0 {
		n = 10
	}
	var micro int64
	switch op.Name {
	case "search", "findSimilar":
		micro = 7000 // live 2026-11-09: a neural search with 1 result cost $0.007
		if gjson.GetBytes(body, "contents").Exists() {
			micro += int64(n) * 1000
		}
	case "contents":
		urls := countURLs(gjson.GetBytes(body, "urls")) + countURLs(gjson.GetBytes(body, "ids"))
		micro = int64(max(urls, 1)) * 1000
	case "answer":
		micro = 5000
	case "research":
		micro = 100_000
	case "agent_run", "agent_get":
		micro = 0 // billed from costDollars when the run ends
	}
	return Cost{MicroUSD: micro, Estimated: true}
}

// AgentRunEnded reports whether an Exa Agent run status is terminal
// (exa-spec.yaml AgentRun.status: queued, running, completed, failed, cancelled).
func AgentRunEnded(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

// Limits follows exa.ai/docs/reference/rate-limits (checked 2026-11-09): QPS
// per team across all its keys; /search and /answer 10, deep search types 5,
// /contents 100. findSimilar and research are not listed: findSimilar is
// assumed to share /search capacity, research is capped like agent runs (5).
func (e *Exa) Limits(op Operation, body []byte, secret string, override float64) []Limit {
	qps := 10.0
	if override > 0 {
		qps = override
	}
	switch op.Name {
	case "search":
		ls := []Limit{{"search", qps, time.Second}}
		switch gjson.GetBytes(body, "type").String() {
		case "deep-lite", "deep", "deep-reasoning":
			ls = append(ls, Limit{"search_deep", 5, time.Second})
		}
		return ls
	case "answer", "findSimilar":
		return []Limit{{"search", qps, time.Second}}
	case "contents":
		return []Limit{{"contents", 100, time.Second}}
	case "research":
		return []Limit{{"research", 5, time.Second}}
	case "agent_run": // 5 QPS per team, and starting a run counts as two requests: 2.5 starts/s. Polls are free.
		return []Limit{{"agent", 2.5, time.Second}}
	}
	return nil
}

func (e *Exa) TaskIDFromResponse(op Operation, body []byte) string {
	if op.Task != TaskCreate {
		return ""
	}
	for _, k := range []string{"researchId", "id"} {
		if v := gjson.GetBytes(body, k).String(); v != "" {
			return v
		}
	}
	return ""
}

func (e *Exa) QueryPreview(op Operation, body []byte, n int) string {
	if n <= 0 || !gjson.ValidBytes(body) {
		return ""
	}
	switch op.Name {
	case "search", "answer", "agent_run":
		return previewString(gjson.GetBytes(body, "query").String(), n)
	case "contents":
		if r := gjson.GetBytes(body, "urls"); r.Exists() {
			return previewURLs(r, n)
		}
		return previewURLs(gjson.GetBytes(body, "ids"), n)
	case "findSimilar":
		return previewURLs(gjson.GetBytes(body, "url"), n)
	case "research":
		return previewString(gjson.GetBytes(body, "instructions").String(), n)
	}
	return ""
}

func (e *Exa) UsageRequest(ctx context.Context, key string) (*http.Request, error) {
	return nil, ErrUnsupported
}

func (e *Exa) ParseUsage(body []byte) (Quota, error) { return Quota{}, ErrUnsupported }

// probeRunID names an agent run that cannot exist: looking it up is free and
// tells a valid key (404 RUN_NOT_FOUND) from an invalid one (401). Verified
// live 2026-11-10; Exa has no balance API for normal keys.
const probeRunID = "agent_run_00000000000000000000"

// ProbeRequest builds a free request that checks whether key is accepted.
func (e *Exa) ProbeRequest(ctx context.Context, key string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.base+"/agent/runs/"+probeRunID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	e.Authorize(req.Header, key)
	return req, nil
}

// ProbeValid reads the answer to ProbeRequest: known=false when it proves nothing.
func (e *Exa) ProbeValid(status int, body []byte) (valid, known bool) {
	switch {
	case status == http.StatusOK, status == http.StatusNotFound && gjson.GetBytes(body, "error.code").String() == "RUN_NOT_FOUND":
		return true, true
	case status == http.StatusUnauthorized:
		return false, true
	case status == http.StatusPaymentRequired && gjson.GetBytes(body, "tag").String() == "X402_PAYMENT_REQUIRED":
		return false, true
	}
	return false, false
}
