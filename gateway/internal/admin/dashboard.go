package admin

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/outbound"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

// Dashboard (ARCHITECTURE.md §13.3).

type rangeOpt struct {
	ID, Label string
	Span      time.Duration
	Bucket    time.Duration
}

func ranges(l lang) []rangeOpt {
	return []rangeOpt{
		{"6h", l.tr("6 小时", "6 h"), 6 * time.Hour, 5 * time.Minute},
		{"24h", l.tr("24 小时", "24 h"), 24 * time.Hour, 15 * time.Minute},
		{"7d", l.tr("7 天", "7 d"), 7 * 24 * time.Hour, time.Hour},
		{"30d", l.tr("30 天", "30 d"), 30 * 24 * time.Hour, 24 * time.Hour},
	}
}

type counts struct{ Tavily, Exa, Egresses, Tokens int }

// statusCount is one segment of a provider's key bar.
type statusCount struct {
	Status pool.Status
	N      int
}

// keyStats counts keys by runtime status for one provider.
type keyStats struct {
	Provider                                                   string
	Active, Cooldown, Exhausted, Invalid, Disabled, EgressDown int
	Total                                                      int
	Parts                                                      []statusCount // the statuses that have keys, in display order
	Bar                                                        template.HTML // Parts as one stacked bar
	Quota                                                      string
	Forecast                                                   string
}

type syncError struct{ Provider, Label, Error string }

type topRow struct {
	Label            string
	Link             string
	Dialog           bool // Link opens in the shared dialog (a key, a token)
	Requests, Errors int64
	Max              int64 // the list's largest Requests, for the share bar
	Cost             string
}

type dashData struct {
	Range                    rangeOpt
	Ranges                   []rangeOpt
	Today, Week, Month       int64
	T                        store.Totals
	Chart                    template.HTML
	KeyStats                 []keyStats
	EgressOK, EgressTotal    int
	Tools, Tokens, Keys, IPs []topRow
	Errors                   []topRow
	SyncErrors               []syncError
	CanSync                  bool
	Files                    []confstore.FileStatus
	FilesPending             int // files whose content on disk is not in effect
	EnvPassword              bool
	Counts                   counts
	NoStore                  bool
}

func (p *Panel) home(w http.ResponseWriter, r *http.Request) {
	l := langOf(r)
	s := p.conf.Snapshot()
	now := p.now()
	all := ranges(l)
	rg := all[1] // 24 hours
	for _, o := range all {
		if o.ID == r.URL.Query().Get("range") {
			rg = o
		}
	}
	views := p.keys()
	d := dashData{Range: rg, Ranges: all, CanSync: p.sync != nil, Files: p.conf.Status(), EnvPassword: p.envPw, NoStore: p.st == nil,
		Counts: counts{Tavily: len(s.KeysFor("tavily")), Exa: len(s.KeysFor("exa")), Egresses: len(s.Egresses.Nodes()), Tokens: len(s.Tokens)}}
	for _, f := range d.Files {
		if !f.Applied {
			d.FilesPending++
		}
	}
	for _, v := range views {
		if v.SyncError != "" {
			d.SyncErrors = append(d.SyncErrors, syncError{Provider: v.Provider, Label: v.Label, Error: v.SyncError})
		}
	}
	for _, e := range p.egresses() {
		if !e.Enabled {
			continue
		}
		d.EgressTotal++
		if e.Health == outbound.HealthHealthy {
			d.EgressOK++
		}
	}

	var week store.Totals
	if p.st != nil {
		ctx := r.Context()
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		total := func(from time.Time) store.Totals {
			t, err := p.st.Totals(ctx, from, now.Add(time.Second))
			if err != nil {
				p.log.Warn("dashboard totals", "err", err)
			}
			return t
		}
		d.Today = total(midnight).Requests
		week = total(now.Add(-7 * 24 * time.Hour))
		d.Week = week.Requests
		d.Month = total(now.Add(-30 * 24 * time.Hour)).Requests
		from := now.Add(-rg.Span).Truncate(rg.Bucket)
		d.T = total(from)
		series, err := p.st.Series(ctx, from, now.Add(time.Second), rg.Bucket)
		if err != nil {
			p.log.Warn("dashboard series", "err", err)
		}
		d.Chart = chartSVG(l, series, from, now, rg.Bucket)

		keyLabel := map[string]string{}
		for _, v := range views {
			keyLabel[v.ID] = v.Provider + " · " + v.Label
		}
		tokName := map[string]string{}
		for _, t := range s.Tokens {
			tokName[t.ID] = t.Name
		}
		deleted := l.tr("（已删除）", " (deleted)")
		top := func(dim string, label func(string) (string, string)) []topRow {
			rows, err := p.st.Top(ctx, from, now.Add(time.Second), dim, 10)
			if err != nil {
				p.log.Warn("dashboard top", "dim", dim, "err", err)
			}
			out := make([]topRow, 0, len(rows))
			var most int64
			for _, x := range rows {
				most = max(most, x.Requests)
			}
			for _, x := range rows {
				lb, link := label(x.Value)
				inDialog := link != "" && (dim == store.ByKey || dim == store.ByToken)
				cost := ""
				if x.Credits > 0 {
					cost = thousands(x.Credits) + " credits"
				}
				if x.MicroUSD > 0 {
					cost = strings.TrimPrefix(cost+" · "+usd(x.MicroUSD), " · ")
				}
				out = append(out, topRow{Label: lb, Link: link, Dialog: inDialog, Requests: x.Requests, Errors: x.Errors, Max: most, Cost: cost})
			}
			return out
		}
		d.Tools = top(store.ByTool, func(v string) (string, string) { return v, "" })
		d.Tokens = top(store.ByToken, func(v string) (string, string) {
			if n, ok := tokName[v]; ok {
				return n, p.base + "tokens/" + v
			}
			return v + deleted, ""
		})
		d.Keys = top(store.ByKey, func(v string) (string, string) {
			if lb, ok := keyLabel[v]; ok {
				return lb, p.base + "keys/" + v
			}
			return v + deleted, ""
		})
		d.IPs = top(store.ByClientIP, func(v string) (string, string) { return v, p.base + "requests?ip=" + url.QueryEscape(v) })
		d.Errors = top(store.ByErrorKind, func(v string) (string, string) { return errorKindText(l, v), "" })
		for i := range d.Errors {
			d.Errors[i].Errors = 0 // every row here is a failure: a "100% failed" next to each says nothing
		}
	}
	d.KeyStats = statsOf(l, views, week)
	p.render(w, r, http.StatusOK, "home", page{Title: l.tr("概览", "Overview"), Nav: "home", Data: d})
}

// syncQuota queues a quota sync of every syncable key; the gates decide when each runs.
func (p *Panel) syncQuota(w http.ResponseWriter, r *http.Request) {
	back := "?ok="
	if to := r.PostFormValue("back"); to == "keys" {
		back = "keys?ok="
	}
	if p.sync == nil || p.sync() == 0 {
		p.redirect(w, r, back+"sync_none")
		return
	}
	p.redirect(w, r, back+"sync")
}

var errorKinds = map[string]msg{
	"rate_limited":   {"限速", "Rate limited"},
	"no_keys":        {"无可用 key", "No key available"},
	"deadline":       {"超时", "Timed out"},
	"client_gone":    {"客户端断开", "Client disconnected"},
	"egress_connect": {"出口连接失败", "Egress connection failed"},
	"upstream_io":    {"上游连接中断", "Upstream connection broke"},
	"client_error":   {"请求参数错误", "Bad request parameters"},
	"key_invalid":    {"key 失效", "Key invalid"},
	"key_exhausted":  {"额度用尽", "Quota exhausted"},
	"key_forbidden":  {"key 被拒绝", "Key refused"},
	"edge_blocked":   {"出口被拦截", "Egress blocked"},
	"server_error":   {"上游 5xx", "Upstream 5xx"},
	"upstream_busy":  {"上游繁忙", "Upstream busy"},
	"gateway_error":  {"网关错误", "Gateway error"},
}

func errorKindText(l lang, k string) string {
	if t, ok := errorKinds[k]; ok {
		return t.in(l) + l.tr("（", " (") + k + l.tr("）", ")")
	}
	return k
}

func statsOf(l lang, views []pool.View, week store.Totals) []keyStats {
	byP := map[string]*keyStats{}
	group := map[string][]pool.View{}
	var out []*keyStats
	for _, v := range views {
		s := byP[v.Provider]
		if s == nil {
			s = &keyStats{Provider: v.Provider}
			byP[v.Provider] = s
			out = append(out, s)
		}
		group[v.Provider] = append(group[v.Provider], v)
		s.Total++
		switch v.Status {
		case pool.StatusActive:
			s.Active++
		case pool.StatusCooldown:
			s.Cooldown++
		case pool.StatusExhausted:
			s.Exhausted++
		case pool.StatusInvalid:
			s.Invalid++
		case pool.StatusDisabled:
			s.Disabled++
		case pool.StatusEgressDown:
			s.EgressDown++
		}
	}
	res := make([]keyStats, len(out))
	for i, s := range out {
		vs := group[s.Provider]
		for _, c := range []statusCount{{pool.StatusActive, s.Active}, {pool.StatusCooldown, s.Cooldown}, {pool.StatusEgressDown, s.EgressDown},
			{pool.StatusExhausted, s.Exhausted}, {pool.StatusInvalid, s.Invalid}, {pool.StatusDisabled, s.Disabled}} {
			if c.N > 0 {
				s.Parts = append(s.Parts, c)
			}
		}
		s.Bar = stackBar(l, s.Parts, s.Total)
		s.Quota = quotaSummary(l, s.Provider, vs)
		left, ok := poolRemaining(s.Provider, vs)
		perDay := float64(week.Credits) / 7
		if s.Provider == "exa" {
			perDay = float64(week.MicroUSD) / 7
		}
		if ok && perDay > 0 {
			days := float64(left) / perDay
			s.Forecast = fmt.Sprintf(l.tr("按最近 7 天的用量，约 %.0f 天后用完", "At the last 7 days' rate it lasts about %.0f more days"), math.Floor(days))
			if days < 1 {
				s.Forecast = l.tr("按最近 7 天的用量，不到 1 天就会用完", "At the last 7 days' rate it runs out within a day")
			}
		}
		res[i] = *s
	}
	return res
}

// stackBar draws key counts by status as one bar. It is an SVG whose segments
// are sized by attributes, because the CSP allows no inline style.
func stackBar(l lang, parts []statusCount, total int) template.HTML {
	if total == 0 {
		return ""
	}
	const gap = 0.8
	var b strings.Builder
	b.WriteString(`<svg class="stackbar" viewBox="0 0 100 8" preserveAspectRatio="none" role="img">`)
	x := 0.0
	for i, c := range parts {
		w := 100 * float64(c.N) / float64(total)
		draw := w
		if i < len(parts)-1 {
			draw = math.Max(w-gap, 0.5)
		}
		fmt.Fprintf(&b, `<rect class="st-%s" x="%.2f" y="0" width="%.2f" height="8"><title>%s %d</title></rect>`,
			template.HTMLEscapeString(string(c.Status)), x, draw, template.HTMLEscapeString(keyStatusText(l, c.Status)), c.N)
		x += w
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// poolRemaining is a provider's total balance, a team's shared balance counted once.
func poolRemaining(provider string, vs []pool.View) (int64, bool) {
	var left int64
	known := false
	teams := map[string]bool{}
	for _, v := range vs {
		if v.Remaining == nil || (v.SharedRemaining && teams[v.Team]) || v.Status == pool.StatusDisabled {
			continue
		}
		if v.SharedRemaining {
			teams[v.Team] = true
		}
		left, known = left+*v.Remaining, true
	}
	return left, known
}

// quotaSummary is the balance of one provider's keys in a line. A balance a
// team shares (Exa) is counted once per team.
func quotaSummary(l lang, provider string, vs []pool.View) string {
	if provider == "exa" {
		var used int64
		estimated := false
		for _, v := range vs {
			if v.Used != nil {
				used += *v.Used
			}
			estimated = estimated || (v.Remaining != nil && v.LimitEstimated)
		}
		s := l.tr("本月已用 ", "Used this month: ") + usd(used)
		if left, ok := poolRemaining(provider, vs); ok {
			s += l.tr("，剩余 ", ", left: ") + usd(left)
			if estimated {
				s += l.tr("（免费 key 按每月免费额度估算）", " (free keys estimated from the monthly free grant)")
			}
		}
		return s
	}
	known := 0
	for _, v := range vs {
		if v.Remaining != nil {
			known++
		}
	}
	if known == 0 {
		return l.tr("尚未同步", "Not synced yet")
	}
	left, _ := poolRemaining(provider, vs)
	return fmt.Sprintf(l.tr("剩余 %s credits（%d/%d 个 key 已同步）", "%s credits left (%d/%d keys synced)"), thousands(left), known, len(vs))
}

// chartSVG draws requests per bucket, stacked by provider, with the error rate
// as a line on its own 0–100% scale. Server-side SVG: no chart library, and
// only CSS classes (the CSP forbids inline styles).
//
// The plot is an SVG that stretches to whatever box the card gives it
// (preserveAspectRatio="none"), so the chart fills its card at every window
// width. Text would be distorted by that, so the axis labels are HTML around
// the plot, laid out by the stylesheet.
func chartSVG(l lang, points []store.SeriesPoint, from, to time.Time, bucket time.Duration) template.HTML {
	const w, h = 1000.0, 100.0 // the plot's own units; the stylesheet decides its real size
	n := int(to.Sub(from)/bucket) + 1
	if n < 1 {
		n = 1
	}
	type cell struct{ tavily, exa, errs int64 }
	cells := make([]cell, n)
	for _, pt := range points {
		i := int(pt.Bucket.Sub(from) / bucket)
		if i < 0 || i >= n {
			continue
		}
		if pt.Provider == "tavily" {
			cells[i].tavily += pt.Requests
		} else {
			cells[i].exa += pt.Requests
		}
		cells[i].errs += pt.Errors
	}
	var maxV int64
	for _, c := range cells {
		maxV = max(maxV, c.tavily+c.exa)
	}
	if maxV == 0 {
		return template.HTML(`<p class="empty">` + template.HTMLEscapeString(l.tr("这段时间没有请求。", "No requests in this period.")) + `</p>`)
	}
	scale := niceCeil(maxV)
	bw := w / float64(n)
	y := func(v int64) float64 { return h - h*float64(v)/float64(scale) }

	var b strings.Builder
	fmt.Fprintf(&b, `<div class="chart" role="img" aria-label="%s">`, template.HTMLEscapeString(l.tr("请求量趋势", "Request volume over time")))
	fmt.Fprintf(&b, `<div class="chart-y"><span>%s</span><span>%s</span><span>0</span></div>`, thousands(scale), thousands(scale/2))
	fmt.Fprintf(&b, `<div class="chart-plot"><svg viewBox="0 0 %.0f %.0f" preserveAspectRatio="none">`, w, h)
	for _, f := range []float64{0, 0.5, 1} {
		fmt.Fprintf(&b, `<line class="rule" x1="0" x2="%.0f" y1="%.1f" y2="%.1f"/>`, w, h*f, h*f)
	}
	smooth := n / 40
	failed := l.tr("失败", "failed")
	var line []string
	for i, c := range cells {
		x := float64(i) * bw
		t := from.Add(time.Duration(i) * bucket)
		total := c.tavily + c.exa
		tip := fmt.Sprintf("%s  Tavily %d · Exa %d · %s %d", t.Local().Format("01-02 15:04"), c.tavily, c.exa, failed, c.errs)
		fmt.Fprintf(&b, `<g><title>%s</title>`, template.HTMLEscapeString(tip))
		gap := math.Min(2, bw*0.2)
		if c.tavily > 0 {
			fmt.Fprintf(&b, `<rect class="s-tavily" x="%.2f" y="%.2f" width="%.2f" height="%.2f"/>`, x+gap/2, y(c.tavily), bw-gap, y(0)-y(c.tavily))
		}
		if c.exa > 0 {
			fmt.Fprintf(&b, `<rect class="s-exa" x="%.2f" y="%.2f" width="%.2f" height="%.2f"/>`, x+gap/2, y(total), bw-gap, y(c.tavily)-y(total))
		}
		fmt.Fprintf(&b, `<rect class="hit" x="%.2f" y="0" width="%.2f" height="%.0f"/></g>`, x, bw, h)
		// The failure rate is taken over a few neighbouring buckets: on a quiet
		// gateway one failed request in an hour would otherwise spike to 100%.
		var errs, reqs int64
		for j := max(0, i-smooth); j <= min(n-1, i+smooth); j++ {
			errs += cells[j].errs
			reqs += cells[j].tavily + cells[j].exa
		}
		if reqs > 0 {
			line = append(line, fmt.Sprintf("%.2f,%.2f", x+bw/2, h-h*float64(errs)/float64(reqs)))
		}
	}
	if len(line) > 1 {
		fmt.Fprintf(&b, `<polyline class="s-err" points="%s"/>`, strings.Join(line, " "))
	}
	b.WriteString(`</svg></div><div class="chart-y err"><span>100%</span><span>50%</span><span>0%</span></div><div class="chart-x">`)
	// The time labels share the width equally; each names the bucket that starts there.
	format := "15:04"
	if bucket >= 24*time.Hour {
		format = "01-02"
	} else if to.Sub(from) > 24*time.Hour {
		format = "01-02 15h"
	}
	labels := min(n, 8)
	for j := 0; j < labels; j++ {
		fmt.Fprintf(&b, `<span>%s</span>`, from.Add(time.Duration(j*n/labels)*bucket).Local().Format(format))
	}
	b.WriteString(`</div></div>`)
	return template.HTML(b.String())
}

// niceCeil rounds up to 1, 2 or 5 times a power of ten.
func niceCeil(v int64) int64 {
	if v <= 1 {
		return 1
	}
	p := int64(1)
	for p*10 < v {
		p *= 10
	}
	for _, m := range []int64{1, 2, 5, 10} {
		if p*m >= v {
			return p * m
		}
	}
	return p * 10
}
