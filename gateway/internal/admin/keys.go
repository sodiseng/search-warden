package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/egress"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

// Keys page (ARCHITECTURE.md §16). Every change is written to keys/<provider>.yaml.

type keyRow struct {
	pool.View
	Key          confstore.Key
	Masked       string
	EgressHealth string
	Req24, Err24 int64
}

type keyGroup struct {
	Provider string
	Rows     []keyRow
	Stats    keyStats
}

type keysData struct {
	Groups      []keyGroup
	Egress      []option // direct and the nodes
	BatchEgress []option // the batch picker: an explicit choice is required
	CanSync     bool
	CanTest     bool
	Lines       string // the add form, kept after an error
}

// Egress picker sentinels: never valid egress values (§7.9.2 rule 2).
const (
	egressKeep = "__keep__" // leave the key's egress as it is
	egressNone = "__none__" // nothing chosen
)

func (p *Panel) egressOptions(l lang) []option {
	out := []option{{egress.Direct, l.tr("直连（服务器 IP）", "Direct (the server's IP)")}}
	for _, n := range p.conf.Snapshot().Egresses.Nodes() {
		label := n.Name
		if !n.Enabled {
			label += l.tr("（已停用）", " (disabled)")
		}
		out = append(out, option{n.Name, label})
	}
	return out
}

func (p *Panel) keysData(r *http.Request) keysData {
	l := langOf(r)
	s := p.conf.Snapshot()
	views := map[string]pool.View{}
	for _, v := range p.keys() {
		views[v.ID] = v
	}
	health := map[string]string{}
	for _, e := range p.egresses() {
		health[e.ID] = e.Health
	}
	traffic := map[string]store.TopRow{}
	if p.st != nil {
		now := p.now()
		rows, err := p.st.Top(r.Context(), now.Add(-24*time.Hour), now.Add(time.Second), store.ByKey, 0)
		if err != nil {
			p.log.Warn("key traffic", "err", err)
		}
		for _, x := range rows {
			traffic[x.Value] = x
		}
	}
	d := keysData{Egress: p.egressOptions(l), CanSync: p.sync != nil, CanTest: p.test != nil,
		BatchEgress: append([]option{{egressNone, l.tr("选择出口…", "Choose an egress…")}, {"", l.tr("改回文件默认值", "Back to the file's default")}}, p.egressOptions(l)...)}
	for _, prov := range confstore.Providers {
		g := keyGroup{Provider: prov}
		var vs []pool.View
		for _, k := range s.KeysFor(prov) {
			v := views[k.ID]
			vs = append(vs, v)
			t := traffic[k.ID]
			g.Rows = append(g.Rows, keyRow{View: v, Key: k, Masked: confstore.MaskSecret(k.Secret), EgressHealth: health[k.Egress.ID],
				Req24: t.Requests, Err24: t.Errors})
		}
		if st := statsOf(l, vs, store.Totals{}); len(st) == 1 {
			g.Stats = st[0]
		}
		d.Groups = append(d.Groups, g)
	}
	return d
}

func (p *Panel) keysPage(w http.ResponseWriter, r *http.Request) {
	p.render(w, r, http.StatusOK, "keys", page{Title: "Keys", Nav: "keys", Data: p.keysData(r)})
}

func addKeyTitle(l lang) string { return l.tr("添加 key", "Add keys") }

func (p *Panel) keysError(w http.ResponseWriter, r *http.Request, status int, msg, lines string) {
	d := p.keysData(r)
	d.Lines = lines
	if htmx(r) {
		p.modal(w, r, status, "keys", page{Title: addKeyTitle(langOf(r)), Nav: "keys", Error: msg, Data: d})
		return
	}
	p.render(w, r, status, "keys", page{Title: "Keys", Nav: "keys", Error: msg, Data: d})
}

// keyNew is the create dialog: one key with fields, or a batch paste. It lives on
// the list page, so a direct visit shows that.
func (p *Panel) keyNew(w http.ResponseWriter, r *http.Request) {
	if !htmx(r) {
		p.keysPage(w, r)
		return
	}
	p.modal(w, r, http.StatusOK, "keys", page{Title: addKeyTitle(langOf(r)), Nav: "keys", Data: p.keysData(r)})
}

func (p *Panel) keysAdd(w http.ResponseWriter, r *http.Request) {
	l := langOf(r)
	lines := r.PostFormValue("lines")
	var items []confstore.KeyInput
	if strings.TrimSpace(lines) == "" {
		// The single-key form: one key plus the pickers.
		secret := strings.TrimSpace(r.PostFormValue("secret"))
		if secret == "" {
			p.keysError(w, r, http.StatusBadRequest, l.tr("请填写 key。", "Enter a key."), "")
			return
		}
		items = []confstore.KeyInput{{
			Secret: secret,
			Label:  strings.TrimSpace(r.PostFormValue("label")),
			Plan:   strings.TrimSpace(r.PostFormValue("plan")),
			Region: strings.TrimSpace(r.PostFormValue("region")),
			Note:   strings.TrimSpace(r.PostFormValue("note")),
			Egress: egressInput(r),
		}}
	} else {
		var errs []error
		items, errs = confstore.ParseKeyLines(lines)
		if len(errs) > 0 {
			msgs := make([]string, len(errs))
			for i, e := range errs {
				msgs[i] = e.Error()
			}
			p.keysError(w, r, http.StatusBadRequest, l.tr("格式错误：", "Bad format: ")+strings.Join(msgs, l.tr("；", "; ")), lines)
			return
		}
	}
	if len(items) == 0 {
		p.keysError(w, r, http.StatusBadRequest, l.tr("没有要添加的 key。", "There is no key to add."), lines)
		return
	}
	prov := r.PostFormValue("provider")
	eg := strings.TrimSpace(r.PostFormValue("egress"))
	for i := range items {
		if prov == "tavily" || prov == "exa" {
			items[i].Provider = prov
		}
		if items[i].Egress == "" {
			items[i].Egress = eg
		}
	}
	if e := strings.TrimSpace(r.PostFormValue("egress")); e != "" {
		for i := range items {
			if items[i].Egress == "" {
				items[i].Egress = e
			}
		}
	}
	added, skipped, err := p.conf.AddKeys(nil, items)
	if err != nil {
		p.keysError(w, r, http.StatusBadRequest, saveError(l, err), lines)
		return
	}
	if len(added) > 0 && p.sync != nil {
		var ids []string
		for _, k := range p.conf.Snapshot().Keys {
			if k.Provider == "tavily" {
				ids = append(ids, k.ID)
			}
		}
		p.sync(ids...) // new Tavily keys learn their balance soon (queued keys still respect the gates)
	}
	p.log.Info("keys added", "count", len(added), "skipped", len(skipped))
	p.done(w, r, "keys?ok=keys_added")
}

func (p *Panel) keysBatch(w http.ResponseWriter, r *http.Request) {
	l := langOf(r)
	if err := r.ParseForm(); err != nil {
		p.keysError(w, r, http.StatusBadRequest, l.tr("表单无效。", "The form is invalid."), "")
		return
	}
	ids := r.PostForm["id"]
	if len(ids) == 0 {
		p.keysError(w, r, http.StatusBadRequest, l.tr("请先勾选 key。", "Select at least one key first."), "")
		return
	}
	var err error
	ok := "keys_updated"
	switch r.PostFormValue("action") {
	case "enable", "disable":
		on := r.PostFormValue("action") == "enable"
		_, err = p.conf.UpdateKeys(nil, ids, confstore.KeyPatch{Enabled: &on})
	case "egress":
		v := egressInput(r)
		if v == egressNone || v == egressKeep {
			p.keysError(w, r, http.StatusBadRequest, l.tr("请选择出口，或粘贴代理 URL。", "Choose an egress, or paste a proxy URL."), "")
			return
		}
		_, err = p.conf.UpdateKeys(nil, ids, confstore.KeyPatch{Egress: &v})
	case "delete":
		_, err = p.conf.DeleteKeys(nil, ids)
		ok = "keys_deleted"
	case "test":
		if p.test == nil || p.test(ids...) == 0 {
			p.redirect(w, r, "keys?ok=sync_none")
			return
		}
		p.redirect(w, r, "keys?ok=testing_keys")
		return
	case "sync":
		if p.sync == nil || p.sync(ids...) == 0 {
			p.redirect(w, r, "keys?ok=sync_none")
			return
		}
		p.redirect(w, r, "keys?ok=sync")
		return
	default:
		p.keysError(w, r, http.StatusBadRequest, l.tr("未知的操作。", "Unknown action."), "")
		return
	}
	if err != nil {
		p.keysError(w, r, http.StatusBadRequest, saveError(l, err), "")
		return
	}
	p.done(w, r, "keys?ok="+ok)
}

// egressInput reads an egress picker: a typed URL wins over the selection.
func egressInput(r *http.Request) string {
	if v := strings.TrimSpace(r.PostFormValue("egress_url")); v != "" {
		return v
	}
	return strings.TrimSpace(r.PostFormValue("egress"))
}

type keyData struct {
	Row       keyRow
	Egress    []option
	EgressSel string // the option selected now
	Revealed  string // the full key, only right after "show"
	CanSync   bool
}

func (p *Panel) keyData(r *http.Request, id string) (keyData, bool) {
	k, ok := p.conf.Snapshot().KeyByID(id)
	if !ok {
		return keyData{}, false
	}
	row := keyRow{Key: k, Masked: confstore.MaskSecret(k.Secret)}
	for _, v := range p.keys() {
		if v.ID == id {
			row.View = v
		}
	}
	for _, e := range p.egresses() {
		if e.ID == k.Egress.ID {
			row.EgressHealth = e.Health
		}
	}
	l := langOf(r)
	d := keyData{Row: row, CanSync: p.sync != nil && k.Provider == "tavily", EgressSel: k.EgressRef,
		Egress: append([]option{{"", l.tr("使用文件默认值", "Use the file's default")}}, p.egressOptions(l)...)}
	if k.Egress.Inline {
		// The URL may hold a proxy password: never put it in the page.
		d.Egress = append([]option{{egressKeep, l.tr("保持当前手填的出口（", "Keep the proxy URL typed in (") + k.Egress.Label() + l.tr("）", ")")}}, d.Egress...)
		d.EgressSel = egressKeep
	}
	return d, true
}

func (p *Panel) keyPage(w http.ResponseWriter, r *http.Request) {
	d, ok := p.keyData(r, r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	pg := page{Title: d.Row.Key.DisplayName(), Nav: "keys", Data: d}
	p.dialog(w, r, http.StatusOK, "key", pg)
}

// optFloat reads an optional non-negative number; blank is 0. ok is false for
// anything else.
func optFloat(s string) (v *float64, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		zero := 0.0
		return &zero, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return nil, false
	}
	return &f, true
}

// optInt reads an optional integer; blank is nil. ok is false for anything else.
func optInt(s string) (v *int, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, true
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil, false
	}
	return &n, true
}

func (p *Panel) keySave(w http.ResponseWriter, r *http.Request) {
	l := langOf(r)
	id := r.PathValue("id")
	d, ok := p.keyData(r, id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	fail := func(msg string) {
		pg := page{Title: d.Row.Key.DisplayName(), Nav: "keys", Error: msg, Data: d}
		p.dialog(w, r, http.StatusBadRequest, "key", pg)
	}
	str := func(k string) *string { v := strings.TrimSpace(r.PostFormValue(k)); return &v }
	patch := confstore.KeyPatch{Label: str("label"), Plan: str("plan"), Region: str("region"), Note: str("note")}
	if eg := egressInput(r); eg != egressKeep {
		patch.Egress = &eg
	}
	if d.Row.Key.Provider == "exa" {
		patch.Team = str("team")
	}
	on := r.PostFormValue("enabled") == "on"
	patch.Enabled = &on
	if w := strings.TrimSpace(r.PostFormValue("weight")); w != "" {
		v, err := strconv.ParseFloat(w, 64)
		if err != nil || v <= 0 || v > 1000 {
			fail(l.tr("权重：必须是大于 0、不超过 1000 的数", "Weight: must be a number above 0 and at most 1000"))
			return
		}
		patch.Weight = &v
	}
	notNumber := l.tr("不是有效的非负数", "not a valid non-negative number")
	notInt := l.tr("不是有效的整数", "not a valid integer")
	if patch.RateLimit, ok = optFloat(r.PostFormValue("rate_limit")); !ok {
		fail(l.tr("限速：", "Rate limit: ") + notNumber)
		return
	}
	if patch.BudgetMonthly, ok = optFloat(r.PostFormValue("budget_monthly")); !ok {
		fail(l.tr("月度预算：", "Monthly budget: ") + notNumber)
		return
	}
	if patch.Priority, ok = optInt(r.PostFormValue("priority")); !ok {
		fail(l.tr("优先级：", "Priority: ") + notInt)
		return
	}
	if patch.ResetDay, ok = optInt(r.PostFormValue("reset_day")); !ok {
		fail(l.tr("重置日：", "Reset day: ") + notInt)
		return
	}
	if _, err := p.conf.UpdateKeys(nil, []string{id}, patch); err != nil {
		fail(saveError(l, err))
		return
	}
	p.done(w, r, "keys?ok=saved")
}

// keyReveal shows the full key once; every reveal is audited (§11.5).
func (p *Panel) keyReveal(w http.ResponseWriter, r *http.Request) {
	d, ok := p.keyData(r, r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	p.audit(r, "key.reveal", d.Row.Key.Provider+" "+d.Row.Key.DisplayName())
	d.Revealed = d.Row.Key.Secret
	pg := page{Title: d.Row.Key.DisplayName(), Nav: "keys", Data: d}
	p.dialog(w, r, http.StatusOK, "key", pg)
}
