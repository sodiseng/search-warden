package admin

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/limits"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

// Tokens page (ARCHITECTURE.md §11.2). A new or rotated token is shown once;
// tokens.yaml only ever holds its hash.

type tokenRow struct {
	T        confstore.Token
	Use      limits.Usage // today and this month (UTC), as the limiter counts
	Spec     confstore.TokenSpec
	Requests int64
	Errors   int64
	Cost     string
	LastUsed time.Time
	Expired  bool
}

type tokenForm struct {
	Spec      confstore.TokenSpec
	ToolsText string
	IPText    string
	Limits    map[string]string
	AllTools  []string
}

type tokensData struct {
	Rows []tokenRow
	Form tokenForm // the create form, kept after an error
}

var limitFields = []string{"rpm", "concurrency", "daily_requests", "monthly_requests", "monthly_credits", "monthly_usd"}

func (p *Panel) allTools() []string {
	return []string{"tavily_search", "tavily_extract", "tavily_crawl", "tavily_map", "tavily_research",
		"web_search_exa", "web_fetch_exa", "web_search_advanced_exa", "agent_run"}
}

func (p *Panel) newForm(s confstore.TokenSpec) tokenForm {
	l := s.Limits
	f := tokenForm{Spec: s, ToolsText: strings.Join(s.Tools, ", "), IPText: strings.Join(s.IPAllow, ", "), AllTools: p.allTools(),
		Limits: map[string]string{}}
	set := func(k string, v int64) {
		if v > 0 {
			f.Limits[k] = strconv.FormatInt(v, 10)
		}
	}
	set("rpm", int64(l.RPM))
	set("concurrency", int64(l.Concurrency))
	set("daily_requests", l.DailyRequests)
	set("monthly_requests", l.MonthlyRequests)
	set("monthly_credits", l.MonthlyCredits)
	if l.MonthlyUSD > 0 {
		f.Limits["monthly_usd"] = strconv.FormatFloat(l.MonthlyUSD, 'f', -1, 64)
	}
	return f
}

func (p *Panel) tokenRows(r *http.Request) []tokenRow {
	s := p.conf.Snapshot()
	usage := map[string]store.TopRow{}
	if p.st != nil {
		now := p.now()
		rows, err := p.st.Top(r.Context(), now.Add(-30*24*time.Hour), now.Add(time.Second), store.ByToken, 0)
		if err != nil {
			p.log.Warn("token usage", "err", err)
		}
		for _, x := range rows {
			usage[x.Value] = x
		}
	}
	var out []tokenRow
	for _, t := range s.Tokens {
		u := usage[t.ID]
		cost := ""
		if u.Credits > 0 {
			cost = thousands(u.Credits) + " credits"
		}
		if u.MicroUSD > 0 {
			cost = strings.TrimPrefix(cost+" · "+usd(u.MicroUSD), " · ")
		}
		out = append(out, tokenRow{T: t, Use: p.usage(t.ID), Spec: confstore.SpecOf(t), Requests: u.Requests, Errors: u.Errors, Cost: cost, LastUsed: u.LastTS,
			Expired: !t.Expires.IsZero() && !p.now().Before(t.Expires)})
	}
	return out
}

func (p *Panel) tokensPage(w http.ResponseWriter, r *http.Request) {
	d := tokensData{Rows: p.tokenRows(r), Form: p.newForm(confstore.TokenSpec{Enabled: true})}
	p.render(w, r, http.StatusOK, "tokens", page{Title: langOf(r).tr("令牌", "Tokens"), Nav: "tokens", Data: d})
}

func newTokenTitle(l lang) string { return l.tr("创建令牌", "Create a token") }

// tokenNew is the create dialog. It lives on the list page, so a direct visit shows that.
func (p *Panel) tokenNew(w http.ResponseWriter, r *http.Request) {
	if !htmx(r) {
		p.tokensPage(w, r)
		return
	}
	d := tokensData{Rows: p.tokenRows(r), Form: p.newForm(confstore.TokenSpec{Enabled: true})}
	p.modal(w, r, http.StatusOK, "tokens", page{Title: newTokenTitle(langOf(r)), Nav: "tokens", Data: d})
}

// specFromForm reads the token form shared by create and edit. A non-empty
// message says what is wrong with the input, in the request's language.
func (p *Panel) specFromForm(r *http.Request) (confstore.TokenSpec, tokenForm, string) {
	l := langOf(r)
	if err := r.ParseForm(); err != nil {
		return confstore.TokenSpec{}, tokenForm{}, l.tr("表单无效", "The form is invalid")
	}
	s := confstore.TokenSpec{Name: strings.TrimSpace(r.PostFormValue("name")), KeyGroup: r.PostFormValue("key_group"),
		Expires: strings.TrimSpace(r.PostFormValue("expires")), Enabled: r.PostFormValue("enabled") == "on", Note: strings.TrimSpace(r.PostFormValue("note")),
		Tools: confstore.ParseList(r.PostFormValue("tools")), IPAllow: confstore.ParseList(r.PostFormValue("ip_allowlist"))}
	for _, v := range r.PostForm["providers"] {
		if slices.Contains(confstore.Providers, v) {
			s.Providers = append(s.Providers, v)
		}
	}
	if len(s.Providers) == len(confstore.Providers) {
		s.Providers = nil // all
	}
	for _, v := range r.PostForm["surfaces"] {
		if v == "mcp" || v == "rest" {
			s.Surfaces = append(s.Surfaces, v)
		}
	}
	if len(s.Surfaces) == 2 {
		s.Surfaces = nil
	}
	form := p.newForm(s)
	form.ToolsText, form.IPText = r.PostFormValue("tools"), r.PostFormValue("ip_allowlist")
	for _, k := range limitFields {
		form.Limits[k] = strings.TrimSpace(r.PostFormValue("limit_" + k))
	}
	if len(r.PostForm["providers"]) == 0 {
		return s, form, l.tr("至少选择一个服务", "Choose at least one service")
	}
	if len(r.PostForm["surfaces"]) == 0 {
		return s, form, l.tr("至少选择一个入口", "Choose at least one entry point")
	}
	for _, t := range s.Tools {
		if !slices.Contains(p.allTools(), t) {
			return s, form, fmt.Sprintf(l.tr("未知的工具名 %q", "Unknown tool name %q"), t)
		}
	}
	ints := map[string]*int64{}
	var rpm, conc int64
	ints["rpm"], ints["concurrency"] = &rpm, &conc
	ints["daily_requests"], ints["monthly_requests"], ints["monthly_credits"] = &s.Limits.DailyRequests, &s.Limits.MonthlyRequests, &s.Limits.MonthlyCredits
	for k, dst := range ints {
		v, err := confstore.ParseLimit(form.Limits[k])
		if err != nil {
			return s, form, fmt.Sprintf(l.tr("限额 %s：%v", "Limit %s: %v"), k, err)
		}
		*dst = v
	}
	s.Limits.RPM, s.Limits.Concurrency = int(rpm), int(conc)
	if v := form.Limits["monthly_usd"]; v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return s, form, fmt.Sprintf(l.tr("限额 monthly_usd：%q 不是有效的非负数", "Limit monthly_usd: %q is not a valid non-negative number"), v)
		}
		s.Limits.MonthlyUSD = f
	}
	form.Spec = s
	return s, form, ""
}

type secretData struct {
	Name, Secret string
	Base         string // public gateway URL for the usage snippets ("" = placeholder)
	Tokens       string // panel base path, for the "back to tokens" link
	Rotated      bool
}

// showSecret renders the one-time token page.
func (p *Panel) showSecret(w http.ResponseWriter, r *http.Request, name, secret string, rotated bool) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	d := secretData{Name: name, Secret: secret, Rotated: rotated, Base: p.gwURL, Tokens: p.base}
	pg := page{Title: langOf(r).tr("保存令牌", "Save the token"), Nav: "tokens", Data: d}
	// A dialog request gets the secret as a fragment: it replaces the create form
	// in place, so the operator copies the token without leaving the list.
	p.dialog(w, r, http.StatusOK, "secret", pg)
}

func (p *Panel) tokenAdd(w http.ResponseWriter, r *http.Request) {
	l := langOf(r)
	s, form, bad := p.specFromForm(r)
	if bad == "" {
		plain, _, err := p.conf.AddToken(nil, s)
		if err == nil {
			p.showSecret(w, r, s.Name, plain, false)
			return
		}
		bad = saveError(l, err)
	}
	pg := page{Title: newTokenTitle(l), Nav: "tokens", Error: bad, Data: tokensData{Rows: p.tokenRows(r), Form: form}}
	p.dialog(w, r, http.StatusBadRequest, "tokens", pg)
}

func (p *Panel) tokenRow(r *http.Request, id string) (tokenRow, bool) {
	for _, t := range p.tokenRows(r) {
		if t.T.ID == id {
			return t, true
		}
	}
	return tokenRow{}, false
}

type tokenData struct {
	Row  tokenRow
	Form tokenForm
}

func (p *Panel) tokenPage(w http.ResponseWriter, r *http.Request) {
	row, ok := p.tokenRow(r, r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	pg := page{Title: row.T.Name, Nav: "tokens", Data: tokenData{Row: row, Form: p.newForm(row.Spec)}}
	p.dialog(w, r, http.StatusOK, "token", pg)
}

func (p *Panel) tokenSave(w http.ResponseWriter, r *http.Request) {
	row, ok := p.tokenRow(r, r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s, form, bad := p.specFromForm(r)
	if bad == "" {
		_, err := p.conf.UpdateToken(nil, row.T.ID, s)
		if err == nil {
			p.done(w, r, "tokens?ok=saved")
			return
		}
		bad = saveError(langOf(r), err)
	}
	pg := page{Title: row.T.Name, Nav: "tokens", Error: bad, Data: tokenData{Row: row, Form: form}}
	p.dialog(w, r, http.StatusBadRequest, "token", pg)
}

func (p *Panel) tokenRotate(w http.ResponseWriter, r *http.Request) {
	row, ok := p.tokenRow(r, r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	plain, _, err := p.conf.RotateToken(nil, row.T.ID)
	if err != nil {
		p.dialog(w, r, http.StatusBadRequest, "token", page{Title: row.T.Name, Nav: "tokens", Error: saveError(langOf(r), err),
			Data: tokenData{Row: row, Form: p.newForm(row.Spec)}})
		return
	}
	p.showSecret(w, r, row.T.Name, plain, true)
}

func (p *Panel) tokenToggle(w http.ResponseWriter, r *http.Request) {
	row, ok := p.tokenRow(r, r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	on := !row.T.Enabled
	if _, err := p.conf.SetTokenEnabled(nil, row.T.ID, on); err != nil {
		p.dialog(w, r, http.StatusBadRequest, "token", page{Title: row.T.Name, Nav: "tokens", Error: saveError(langOf(r), err),
			Data: tokenData{Row: row, Form: p.newForm(row.Spec)}})
		return
	}
	flash := "token_off"
	if on {
		flash = "token_on"
	}
	p.done(w, r, "tokens?ok="+flash)
}

func (p *Panel) tokenDelete(w http.ResponseWriter, r *http.Request) {
	row, ok := p.tokenRow(r, r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := p.conf.DeleteToken(nil, row.T.ID); err != nil {
		p.dialog(w, r, http.StatusBadRequest, "token", page{Title: row.T.Name, Nav: "tokens", Error: saveError(langOf(r), err),
			Data: tokenData{Row: row, Form: p.newForm(row.Spec)}})
		return
	}
	p.done(w, r, "tokens?ok=deleted")
}
