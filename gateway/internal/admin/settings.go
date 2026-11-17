package admin

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/retention"
)

// Settings page (ARCHITECTURE.md §14.6): config.yaml by group. A save writes
// only the fields that changed, all in one edit, keeping the file's comments.

type fieldKind int

const (
	kInt fieldKind = iota
	kFloat
	kBool
	kString
	kSelect
	kList
)

type setting struct {
	Path        string
	Label, Help msg
	Kind        fieldKind
	Options     []string
	get         func(c confstore.Config) any
}

type settingGroup struct {
	ID          string
	Title, Help msg
	Fields      []setting
}

var settingGroups = []settingGroup{
	{"routing", msg{"选 key 与重试", "Key selection and retries"}, msg{}, []setting{
		{"routing.strategy", msg{"选 key 策略", "Key selection strategy"},
			msg{"balanced：均衡（默认）；drain-first：用完一个再用下一个；round-robin：轮询；random：随机",
				"balanced: spread by remaining quota (default); drain-first: use one key up before the next; round-robin; random"},
			kSelect, []string{"balanced", "drain-first", "round-robin", "random"}, func(c confstore.Config) any { return c.Routing.Strategy }},
		{"routing.max_attempts", msg{"单个请求最多尝试几个 key", "Keys tried per request, at most"}, msg{"1–10", "1–10"},
			kInt, nil, func(c confstore.Config) any { return c.Routing.MaxAttempts }},
		{"routing.deadline_seconds", msg{"单个请求的总时限（秒）", "Overall deadline per request (seconds)"}, msg{"10–1800", "10–1800"},
			kInt, nil, func(c confstore.Config) any { return c.Routing.DeadlineSeconds }},
		{"routing.affinity_minutes", msg{"同一调用方固定使用相同 key 的时长（分钟）", "Keep a caller on the same key for (minutes)"},
			msg{"0 表示不固定", "0 = no affinity"}, kInt, nil, func(c confstore.Config) any { return c.Routing.AffinityMinutes }},
		{"routing.per_key_rpm", msg{"单个 key 每分钟最多请求数", "Requests per minute per key, at most"},
			msg{"在官方限速之外再收紧；0 表示不额外限制", "Tighter than the official limit; 0 = no extra limit"},
			kInt, nil, func(c confstore.Config) any { return c.Routing.PerKeyRPM }},
		{"routing.per_key_concurrency", msg{"单个 key 同时进行的请求数上限", "Concurrent requests per key, at most"},
			msg{"0 表示不限制", "0 = unlimited"}, kInt, nil, func(c confstore.Config) any { return c.Routing.PerKeyConcurrency }},
	}},
	{"upstream", msg{"上游请求", "Upstream requests"}, msg{}, []setting{
		{"upstream.precise_billing", msg{"精确计费", "Precise billing"},
			msg{"Tavily 请求附加 include_usage 以获取实际消耗，返回给客户端前会去掉",
				"Adds include_usage to Tavily requests to learn the actual cost; it is removed before the response reaches the client"},
			kBool, nil, func(c confstore.Config) any { return c.Upstream.PreciseBilling }},
		{"upstream.user_agent.tavily", msg{"发往 Tavily 的 User-Agent", "User-Agent sent to Tavily"},
			msg{"留空使用内置默认值 " + confstore.DefaultUserAgent, "Blank = the built-in default " + confstore.DefaultUserAgent},
			kString, nil, func(c confstore.Config) any { return c.Upstream.UserAgent["tavily"] }},
		{"upstream.user_agent.exa", msg{"发往 Exa 的 User-Agent", "User-Agent sent to Exa"},
			msg{"留空使用内置默认值", "Blank = the built-in default"},
			kString, nil, func(c confstore.Config) any { return c.Upstream.UserAgent["exa"] }},
		{"upstream.extra_headers", msg{"额外放行的请求头", "Extra request headers to forward"},
			msg{"逗号分隔。默认只转发必要的请求头（§7.7）", "Comma-separated. By default only the necessary headers are forwarded (§7.7)"},
			kList, nil, func(c confstore.Config) any { return c.Upstream.ExtraHeaders }},
	}},
	{"quota", msg{"额度同步", "Quota sync"}, msg{}, []setting{
		{"quota.sync", msg{"后台同步 Tavily 额度", "Sync Tavily quota in the background"},
			msg{"关闭后只能在面板手动同步", "When off, quota is only synced by hand from the panel"},
			kBool, nil, func(c confstore.Config) any { return c.Quota.Sync }},
		{"quota.hot_interval_minutes", msg{"最近用过的 key 的同步间隔（分钟）", "Sync interval for recently used keys (minutes)"},
			msg{"5–1440", "5–1440"}, kInt, nil, func(c confstore.Config) any { return c.Quota.HotIntervalMinutes }},
		{"quota.cold_interval_hours", msg{"其他 key 的同步间隔（小时）", "Sync interval for the other keys (hours)"},
			msg{"1–720", "1–720"}, kInt, nil, func(c confstore.Config) any { return c.Quota.ColdIntervalHours }},
		{"quota.exa_free_monthly_usd", msg{"Exa 免费 key 的每月额度估算（美元）", "Estimated monthly grant of a free Exa key (USD)"},
			msg{"只用于显示和排序；0 表示不估算", "Only used for display and ordering; 0 = no estimate"},
			kFloat, nil, func(c confstore.Config) any { return c.Quota.ExaFreeMonthlyUSD }},
	}},
	{"logs", msg{"请求日志", "Request log"}, msg{}, []setting{
		{"logs.retention_days", msg{"原始日志保留天数", "Days to keep raw logs"},
			msg{"1–3650。概览页的 30 天统计也来自原始日志，保留期短于 30 天时只能统计到保留期内的数据",
				"1–3650. The overview's 30-day figures come from the raw log too, so a shorter retention shortens them"},
			kInt, nil, func(c confstore.Config) any { return c.Logs.RetentionDays }},
		{"logs.max_size_mb", msg{"日志库容量上限（MB）", "Log database size limit (MB)"},
			msg{"16 以上。超过时删除最旧的日志，降到上限的 90%", "16 or more. Above it the oldest logs are deleted, down to 90% of the limit"},
			kInt, nil, func(c confstore.Config) any { return c.Logs.MaxSizeMB }},
		{"logs.query_preview_chars", msg{"记录搜索词的前几个字符", "Characters of the search query to record"},
			msg{"0 表示不记录", "0 = record nothing"}, kInt, nil, func(c confstore.Config) any { return c.Logs.QueryPreviewChars }},
	}},
	{"network", msg{"客户端 IP", "Client IP"},
		msg{"修改前请确认反向代理的配置，填错会让所有请求都显示为代理的 IP。",
			"Check your reverse proxy's configuration before changing these: a wrong value makes every request appear to come from the proxy."},
		[]setting{
			{"network.trusted_proxies", msg{"可信代理网段", "Trusted proxy networks"},
				msg{"逗号分隔的 IP 或 CIDR", "Comma-separated IPs or CIDRs"},
				kList, nil, func(c confstore.Config) any { return c.Network.TrustedProxies }},
			{"network.client_ip_headers", msg{"识别客户端 IP 的请求头", "Headers that carry the client IP"},
				msg{"逗号分隔；X-Real-IP、CF-Connecting-IP 只有在反向代理会覆盖时才添加",
					"Comma-separated; add X-Real-IP or CF-Connecting-IP only if your reverse proxy overwrites them"},
				kList, nil, func(c confstore.Config) any { return c.Network.ClientIPHeaders }},
		}},
	{"egress", msg{"出口", "Egress"}, msg{}, []setting{
		{"egress.max_keys_per_egress", msg{"单个出口绑定的 key 超过多少时提示", "Warn when one egress carries more keys than"},
			msg{"0 表示不提示", "0 = never warn"}, kInt, nil, func(c confstore.Config) any { return c.Egress.MaxKeysPerEgress }},
		{"egress.health_interval_seconds", msg{"出口健康检查间隔（秒）", "Egress health check interval (seconds)"},
			msg{"10–3600", "10–3600"}, kInt, nil, func(c confstore.Config) any { return c.Egress.HealthIntervalSeconds }},
	}},
}

// fieldView is one rendered form field.
type fieldView struct {
	setting
	Name  string
	Value string
	On    bool
}

type groupView struct {
	settingGroup
	Views []fieldView
}

type settingsData struct {
	Groups []groupView
	Failed string // the group whose save failed (its fields keep the input)
	Logs   retention.Status
}

func formValue(v any) string {
	switch x := v.(type) {
	case []string:
		return strings.Join(x, ", ")
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func (p *Panel) settingsData(input map[string]string, failed string) settingsData {
	c := p.conf.Snapshot().Config
	d := settingsData{Failed: failed, Logs: p.logStat()}
	for _, g := range settingGroups {
		gv := groupView{settingGroup: g}
		for _, f := range g.Fields {
			v := f.get(c)
			fv := fieldView{setting: f, Name: strings.ReplaceAll(f.Path, ".", "__"), Value: formValue(v)}
			if b, ok := v.(bool); ok {
				fv.On = b
			}
			if g.ID == failed {
				if in, ok := input[fv.Name]; ok {
					fv.Value, fv.On = in, in == "on"
				}
			}
			gv.Views = append(gv.Views, fv)
		}
		d.Groups = append(d.Groups, gv)
	}
	return d
}

func settingsTitle(l lang) string { return l.tr("设置", "Settings") }

func (p *Panel) settingsPage(w http.ResponseWriter, r *http.Request) {
	p.render(w, r, http.StatusOK, "settings", page{Title: settingsTitle(langOf(r)), Nav: "settings", Data: p.settingsData(nil, "")})
}

func (p *Panel) settingsSave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("group")
	i := slices.IndexFunc(settingGroups, func(g settingGroup) bool { return g.ID == id })
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	l := langOf(r)
	input := map[string]string{}
	c := p.conf.Snapshot().Config
	var vals []confstore.ConfigValue
	var bad []string
	for _, f := range settingGroups[i].Fields {
		name := strings.ReplaceAll(f.Path, ".", "__")
		raw := strings.TrimSpace(r.PostFormValue(name))
		input[name] = raw
		var v any
		switch f.Kind {
		case kInt:
			n, err := strconv.Atoi(raw)
			if err != nil {
				bad = append(bad, f.Label.in(l))
				continue
			}
			v = n
		case kFloat:
			x, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				bad = append(bad, f.Label.in(l))
				continue
			}
			v = x
		case kBool:
			v = raw == "on"
		case kList:
			v = confstore.ParseList(raw)
		case kSelect:
			if !slices.Contains(f.Options, raw) {
				bad = append(bad, f.Label.in(l))
				continue
			}
			v = raw
		default:
			v = raw
		}
		if formValue(v) != formValue(f.get(c)) {
			vals = append(vals, confstore.ConfigValue{Path: f.Path, Value: v})
		}
	}
	fail := func(msg string) {
		p.render(w, r, http.StatusBadRequest, "settings", page{Title: settingsTitle(l), Nav: "settings", Error: msg, Data: p.settingsData(input, id)})
	}
	if len(bad) > 0 {
		fail(l.tr("格式不正确：", "Not a valid value: ") + strings.Join(bad, l.tr("、", ", ")))
		return
	}
	if len(vals) > 0 {
		if _, err := p.conf.SetConfigs(nil, vals); err != nil {
			fail(saveError(l, err))
			return
		}
	}
	p.redirect(w, r, "settings?ok=saved#"+id)
}
