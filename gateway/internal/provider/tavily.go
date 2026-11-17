package provider

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TavilyBaseURL is the production REST endpoint.
const TavilyBaseURL = "https://api.tavily.com"

// Tavily implements Provider for api.tavily.com.
type Tavily struct{ base string }

// NewTavily returns a Tavily provider; base "" means production.
func NewTavily(base string) *Tavily {
	if base == "" {
		base = TavilyBaseURL
	}
	return &Tavily{base: strings.TrimRight(base, "/")}
}

func (t *Tavily) ID() string      { return "tavily" }
func (t *Tavily) BaseURL() string { return t.base }

func (t *Tavily) Match(method, path string) (Operation, bool) {
	p := cleanPath(path)
	op := Operation{Provider: "tavily", Method: method, Path: p}
	switch {
	case method == http.MethodPost && p == "/search":
		op.Name, op.Billable, op.RetryAfterSend, op.UsageParam = "search", true, true, true
	case method == http.MethodPost && p == "/extract":
		op.Name, op.Billable, op.RetryAfterSend, op.UsageParam = "extract", true, true, true
	case method == http.MethodPost && p == "/crawl":
		// Crawls are expensive; a resend after a timeout could bill twice.
		op.Name, op.Billable, op.UsageParam = "crawl", true, true
	case method == http.MethodPost && p == "/map":
		op.Name, op.Billable, op.RetryAfterSend, op.UsageParam = "map", true, true, true
	case method == http.MethodPost && p == "/research":
		op.Name, op.Billable, op.Task = "research", true, TaskCreate
	case method == http.MethodGet && strings.HasPrefix(p, "/research/") && strings.Count(p, "/") == 2:
		op.Name, op.RetryAfterSend, op.Task, op.TaskID = "research_get", true, TaskPoll, strings.TrimPrefix(p, "/research/")
	case method == http.MethodPost && p == "/feedback":
		op.Name, op.RetryAfterSend = "feedback", true
	case method == http.MethodGet && p == "/usage":
		op.Name, op.Local = "usage", true
	default:
		return Operation{}, false
	}
	return op, true
}

func (t *Tavily) Authorize(h http.Header, key string) { h.Set("Authorization", "Bearer "+key) }

func (t *Tavily) RewriteRequest(op Operation, body []byte, preciseBilling bool) ([]byte, bool, error) {
	if len(body) == 0 || !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return body, false, nil // not JSON: upstream will answer 400
	}
	var err error
	for _, k := range []string{"api_key", "apiKey"} { // clients may send the gateway token here
		if gjson.GetBytes(body, k).Exists() {
			if body, err = sjson.DeleteBytes(body, k); err != nil {
				return nil, false, err
			}
		}
	}
	if preciseBilling && op.UsageParam && !gjson.GetBytes(body, "include_usage").Bool() {
		if body, err = sjson.SetBytes(body, "include_usage", true); err != nil {
			return nil, false, err
		}
		return body, true, nil
	}
	return body, false, nil
}

func (t *Tavily) RewriteResponse(op Operation, body []byte, injected bool) []byte {
	if !injected || !gjson.GetBytes(body, "usage").Exists() {
		return body
	}
	if out, err := sjson.DeleteBytes(body, "usage"); err == nil {
		return out
	}
	return body
}

func (t *Tavily) Classify(op Operation, status int, h http.Header, body []byte) Outcome {
	reason := errorText(body, "detail.error", "detail", "message", "error")
	if o, ok := classifyCommon(status, h, body, reason); ok {
		if o.Class == ClassRateLimited {
			o.CooldownBase, o.CooldownMax = time.Minute, 5*time.Minute // limits are per minute
		}
		return o
	}
	switch status {
	case 432, 433: // plan limit / pay-as-you-go limit reached
		return Outcome{Class: ClassKeyExhausted, Status: status, Retry: true, Reason: reason}
	case 434: // flex tier full: only for max_age_hours on non-paying plans, not the key's fault
		return Outcome{Class: ClassUpstreamBusy, Status: status, Reason: reason}
	}
	return Outcome{Class: ClassClientError, Status: status, Reason: reason}
}

func (t *Tavily) ActualCost(op Operation, body []byte) (Cost, bool) {
	r := gjson.GetBytes(body, "usage.credits")
	if !r.Exists() {
		return Cost{}, false
	}
	return Cost{Credits: int64(math.Round(r.Float()))}, true
}

// EstimateCost follows Tavily's published credit table; used for quota
// reservation and when a response carries no usage.
func (t *Tavily) EstimateCost(op Operation, body []byte) Cost {
	advanced := gjson.GetBytes(body, "search_depth").String() == "advanced" || gjson.GetBytes(body, "extract_depth").String() == "advanced"
	c := int64(0)
	switch op.Name {
	case "search":
		c = 1
		if advanced {
			c = 2
		}
	case "extract":
		n := int64(ceilDiv(countURLs(gjson.GetBytes(body, "urls")), 5))
		c = n
		if advanced {
			c = 2 * n
		}
	case "map", "crawl":
		limit := int(gjson.GetBytes(body, "limit").Int())
		if limit <= 0 {
			limit = 50
		}
		c = int64(ceilDiv(limit, 10)) // map: 1 credit / 10 pages
		if gjson.GetBytes(body, "instructions").String() != "" {
			c *= 2
		}
		if op.Name == "crawl" {
			per := 5
			c += int64(ceilDiv(limit, per)) // extraction: 1 credit / 5 pages (basic)
			if advanced {
				c += int64(ceilDiv(limit, per))
			}
		}
	case "research":
		// Research is priced dynamically (mini 4–110 credits, pro 15–250) and the
		// API never reports what a task cost; only GET /usage shows it, much later.
		// The estimate is the documented minimum: a quota sync raises a count that
		// is too low but never lowers one that is too high (pool.SetQuota), so only
		// a lower bound converges on the real balance. Verified live 2026-11-10: a
		// short mini task cost 4 credits.
		c = 4
		if gjson.GetBytes(body, "model").String() == "pro" {
			c = 15
		}
	}
	return Cost{Credits: c, Estimated: true}
}

// Limits follows docs.tavily.com/documentation/rate-limits (checked 2026-11-09):
// development keys 100 RPM, production keys 1000 RPM; crawl has its own 100 RPM
// and creating research tasks its own 20 RPM on both key types.
func (t *Tavily) Limits(op Operation, body []byte, secret string, override float64) []Limit {
	switch op.Name {
	case "crawl":
		return []Limit{{"crawl", 100, time.Minute}}
	case "research":
		return []Limit{{"research", 20, time.Minute}}
	}
	rpm := 100.0
	if strings.HasPrefix(secret, "tvly-prod-") {
		rpm = 1000
	}
	if override > 0 {
		rpm = override
	}
	return []Limit{{"default", rpm, time.Minute}}
}

func (t *Tavily) TaskIDFromResponse(op Operation, body []byte) string {
	if op.Task != TaskCreate {
		return ""
	}
	return gjson.GetBytes(body, "request_id").String()
}

func (t *Tavily) QueryPreview(op Operation, body []byte, n int) string {
	if n <= 0 || !gjson.ValidBytes(body) {
		return ""
	}
	switch op.Name {
	case "search":
		return previewString(gjson.GetBytes(body, "query").String(), n)
	case "extract":
		return previewURLs(gjson.GetBytes(body, "urls"), n)
	case "crawl", "map":
		return previewURLs(gjson.GetBytes(body, "url"), n)
	case "research":
		return previewString(gjson.GetBytes(body, "input").String(), n)
	}
	return ""
}

func (t *Tavily) UsageRequest(ctx context.Context, key string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+"/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	t.Authorize(req.Header, key)
	return req, nil
}

// ParseUsage reads GET /usage (docs.tavily.com/documentation/api-reference/endpoint/usage,
// checked 2026-11-09): key.usage / key.limit (null = unlimited) for the key,
// plan_* and paygo_* for its account. See Quota for how they combine.
func (t *Tavily) ParseUsage(body []byte) (Quota, error) {
	if !gjson.ValidBytes(body) {
		return Quota{}, errors.New("usage: response is not JSON")
	}
	key, acct := gjson.GetBytes(body, "key"), gjson.GetBytes(body, "account")
	if !key.IsObject() && !acct.IsObject() {
		return Quota{}, errors.New("usage: unexpected response shape")
	}
	num := func(r gjson.Result) (int64, bool) {
		if r.Type != gjson.Number {
			return 0, false // absent or null
		}
		return int64(math.Round(r.Float())), true
	}
	q := Quota{Plan: strings.TrimSpace(acct.Get("current_plan").String())}

	keyUsed, hasKeyUsed := num(key.Get("usage"))
	planUsed, hasPlanUsed := num(acct.Get("plan_usage"))
	payUsed, _ := num(acct.Get("paygo_usage"))
	switch {
	case hasKeyUsed:
		q.Used = &keyUsed
	case hasPlanUsed: // no per-key figure: the account's is the best we have
		u := planUsed + payUsed
		q.Used = &u
	}

	var rem *int64
	tighter := func(v int64) {
		v = max(v, 0)
		if rem == nil || v < *rem {
			rem = &v
		}
	}
	if kl, ok := num(key.Get("limit")); ok {
		tighter(kl - keyUsed)
	}
	if pl, ok := num(acct.Get("plan_limit")); ok {
		// Clamping each part gives the same answer whether plan_usage stops at
		// plan_limit or keeps counting while pay-as-you-go absorbs the overage.
		left := max(pl-planUsed, 0)
		if gl, ok := num(acct.Get("paygo_limit")); ok && gl > 0 {
			left += max(gl-payUsed, 0)
		}
		tighter(left)
	}
	if rem != nil {
		q.Remaining = rem
		if q.Used != nil {
			l := *q.Used + *rem
			q.Limit = &l
		}
	}
	return q, nil
}
