package admin

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/egress"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
)

// iconSprite is the vendored Lucide sprite (web/static/vendor/README.md): icons
// are referenced as <use href="…#name">, so the page carries no path data and
// needs no inline style.
const iconSprite = "static/vendor/lucide-1.72.0.svg"

// funcs are the template helpers shared by every page. They are bound to one
// language: the panel parses its templates once per language (see New).
func funcs(now func() time.Time, l lang, base string) template.FuncMap {
	return template.FuncMap{
		"T":         l.tr,
		"zh":        func() bool { return l == langZH },
		"tr":        func(m msg) string { return m.in(l) },
		"icon":      func(name string) template.HTML { return iconHTML(base, name) },
		"dt":        func(t time.Time) string { return fmtTime(t) },
		"day":       fmtDay,
		"ago":       func(t time.Time) string { return ago(l, now(), t) },
		"until":     func(t time.Time) string { return untilText(l, now(), t) },
		"num":       func(n int64) string { return thousands(n) },
		"int":       func(n int) string { return thousands(int64(n)) },
		"usd":       usd,
		"pct":       pct,
		"ms":        func(n int) string { return msText(n) },
		"keyStatus": func(s pool.Status) string { return keyStatusText(l, s) },
		"health":    func(h string) string { return healthText(l, h) },
		"quota":     func(v pool.View) string { return quotaText(l, v) },
		"join":      strings.Join,
		"add":       func(a, b int) int { return a + b },
		"sub64":     func(a, b int64) int64 { return a - b },
		"mb": func(b int64) string {
			if b < 10<<20 {
				return strconv.FormatFloat(float64(b)/(1<<20), 'f', 1, 64) + " MB"
			}
			return thousands(b>>20) + " MB"
		},
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
		"has": func(list []string, v string) bool {
			for _, x := range list {
				if x == v {
					return true
				}
			}
			return false
		},
		"bar":    barClass,
		"redact": egress.Redact,
		"deref": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
	}
}

// iconHTML references one symbol of the sprite. The name is a literal from a
// template; TestIconsExistInSprite checks every one of them against the file.
func iconHTML(base, name string) template.HTML {
	return template.HTML(`<svg class="icon" aria-hidden="true" focusable="false"><use href="` +
		template.HTMLEscapeString(base+iconSprite+"#"+name) + `"/></svg>`)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("01-02 15:04:05")
}

// fmtDay is a date with its year (expiry dates): the day alone when the time is
// midnight UTC, which is how a date typed without a time is stored.
func fmtDay(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	if u := t.UTC(); u.Hour() == 0 && u.Minute() == 0 && u.Second() == 0 {
		return u.Format("2006-01-02")
	}
	return t.Local().Format("2006-01-02 15:04")
}

func ago(l lang, now, t time.Time) string {
	if t.IsZero() {
		return l.tr("从未", "never")
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return fmtTime(t)
	case d < time.Minute:
		return l.tr("刚刚", "just now")
	case d < time.Hour:
		return fmt.Sprintf(l.tr("%d 分钟前", "%d min ago"), int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf(l.tr("%d 小时前", "%d h ago"), int(d.Hours()))
	}
	return fmt.Sprintf(l.tr("%d 天前", "%d d ago"), int(d.Hours()/24))
}

// untilText says how far away t is: "5 分钟后" / "in 5 min".
func untilText(l lang, now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := t.Sub(now)
	if d <= 0 {
		return l.tr("即将", "soon")
	}
	return fmt.Sprintf(l.tr("%s后", "in %s"), humanDuration(l, d))
}

func usd(micro int64) string {
	if micro < 1_000_000 {
		return fmt.Sprintf("$%.3f", float64(micro)/1e6)
	}
	return fmt.Sprintf("$%.2f", float64(micro)/1e6)
}

func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// pct renders part/total as a percentage ("—" when total is 0).
func pct(part, total int64) string {
	if total <= 0 {
		return "—"
	}
	v := 100 * float64(part) / float64(total)
	if v > 0 && v < 0.1 {
		return "<0.1%"
	}
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) + "%"
}

func msText(n int) string {
	if n >= 10_000 {
		return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + " s"
	}
	return strconv.Itoa(n) + " ms"
}

var keyStatusNames = map[pool.Status]msg{
	pool.StatusActive:     {"可用", "Active"},
	pool.StatusCooldown:   {"冷却中", "Cooling down"},
	pool.StatusExhausted:  {"额度用尽", "Exhausted"},
	pool.StatusInvalid:    {"失效", "Invalid"},
	pool.StatusDisabled:   {"已停用", "Disabled"},
	pool.StatusEgressDown: {"出口不可用", "Egress down"},
}

func keyStatusText(l lang, s pool.Status) string {
	if t, ok := keyStatusNames[s]; ok {
		return t.in(l)
	}
	return string(s)
}

var healthNames = map[string]msg{
	"unknown":     {"检测中", "Checking"},
	"healthy":     {"正常", "Healthy"},
	"degraded":    {"不稳定", "Degraded"},
	"quarantined": {"已隔离", "Quarantined"},
	"blocked":     {"被上游拦截", "Blocked upstream"},
}

func healthText(l lang, h string) string {
	if t, ok := healthNames[h]; ok {
		return t.in(l)
	}
	return h
}

// barClass picks the colour of a usage bar from the share already used.
func barClass(v pool.View) string {
	if v.Limit == nil || v.Remaining == nil || *v.Limit <= 0 {
		return ""
	}
	left := float64(*v.Remaining) / float64(*v.Limit)
	switch {
	case left <= 0.1:
		return "err"
	case left <= 0.3:
		return "warn"
	}
	return "ok"
}

// quotaText is a key's balance in one line, in its provider's unit.
func quotaText(l lang, v pool.View) string {
	unit := func(n int64) string {
		if v.Unit == "micro_usd" {
			return usd(n)
		}
		return thousands(n)
	}
	switch {
	case v.Limit != nil && v.Remaining != nil:
		s := fmt.Sprintf(l.tr("剩余 %s / %s", "%s / %s left"), unit(*v.Remaining), unit(*v.Limit))
		if v.LimitEstimated {
			s += l.tr("（估算）", " (estimated)")
		}
		return s
	case v.Used != nil:
		return fmt.Sprintf(l.tr("已用 %s", "%s used"), unit(*v.Used))
	}
	return l.tr("未知", "unknown")
}

func humanDuration(l lang, d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf(l.tr("%d 秒", "%d s"), int(math.Ceil(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf(l.tr("%d 分钟", "%d min"), int(math.Ceil(d.Minutes())))
	case d < 48*time.Hour:
		return fmt.Sprintf(l.tr("%d 小时", "%d h"), int(math.Ceil(d.Hours())))
	}
	return fmt.Sprintf(l.tr("%d 天", "%d d"), int(math.Ceil(d.Hours()/24)))
}
