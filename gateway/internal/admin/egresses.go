package admin

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/egress"
	"github.com/sodiseng/search-warden/gateway/internal/outbound"
)

// Egress page (ARCHITECTURE.md §7.9). Nodes live in egresses.yaml; health and
// exit IPs come from the egress runtime. Proxy passwords are never shown.

type egressRow struct {
	Node     egress.Node
	ID       string
	URL      string // redacted
	View     outbound.View
	Keys     []string // labels of the keys bound to it
	DupWith  []string // other egresses with the same exit IP
	OverMax  bool
	IsDirect bool
}

type importForm struct {
	Text, Scheme, Prefix, Region, Tags string
	Absorb                             bool
}

type egressesData struct {
	Rows    []egressRow // direct first, then nodes
	Inline  []egressRow // inline URLs that match no node
	Form    importForm
	Preview []egress.ImportRow
	New     int // rows the preview would add
}

func (p *Panel) egressRows() (rows, inline []egressRow) {
	s := p.conf.Snapshot()
	views := map[string]outbound.View{}
	for _, v := range p.egresses() {
		views[v.ID] = v
	}
	keysOf := map[string][]string{}
	inlineURL := map[string]string{}
	for _, k := range s.Keys {
		keysOf[k.Egress.ID] = append(keysOf[k.Egress.ID], k.Provider+" · "+k.DisplayName())
		if k.Egress.Inline && k.Egress.Name == "" {
			inlineURL[k.Egress.ID] = k.Egress.URL
		}
	}
	// The direct row has no URL; the template says "the server's own IP" instead.
	direct := egressRow{ID: egress.DirectID, View: views[egress.DirectID], Keys: keysOf[egress.DirectID], IsDirect: true,
		Node: egress.Node{Name: egress.Direct, Enabled: true}}
	rows = append(rows, direct)
	for _, n := range s.Egresses.Nodes() {
		id := egress.ID(n.URL)
		r := egressRow{Node: n, ID: id, URL: egress.Redact(n.URL), View: views[id], Keys: keysOf[id]}
		r.OverMax = n.MaxKeys > 0 && len(r.Keys) > n.MaxKeys ||
			s.Config.Egress.MaxKeysPerEgress > 0 && len(r.Keys) > s.Config.Egress.MaxKeysPerEgress
		rows = append(rows, r)
	}
	for id, u := range inlineURL {
		inline = append(inline, egressRow{ID: id, URL: egress.Redact(u), View: views[id], Keys: keysOf[id]})
	}
	sort.Slice(inline, func(i, j int) bool { return inline[i].URL < inline[j].URL })

	// The same exit IP behind two egresses links their keys (§7.5).
	byIP := map[string][]string{}
	all := append(append([]egressRow{}, rows...), inline...)
	for _, r := range all {
		if ip := r.View.ExitIP; ip != "" {
			byIP[ip] = append(byIP[ip], r.Node.Name+r.URLIfInline())
		}
	}
	mark := func(rs []egressRow) {
		for i := range rs {
			for _, other := range byIP[rs[i].View.ExitIP] {
				if other != rs[i].Node.Name+rs[i].URLIfInline() {
					rs[i].DupWith = append(rs[i].DupWith, other)
				}
			}
		}
	}
	mark(rows)
	mark(inline)
	return rows, inline
}

// URLIfInline names an inline egress by its (redacted) URL.
func (r egressRow) URLIfInline() string {
	if r.Node.Name == "" {
		return r.URL
	}
	return ""
}

func (p *Panel) egressesPage(w http.ResponseWriter, r *http.Request) {
	rows, inline := p.egressRows()
	d := egressesData{Rows: rows, Inline: inline, Form: importForm{Scheme: "socks5", Absorb: true}}
	p.render(w, r, http.StatusOK, "egresses", page{Title: egressesTitle(langOf(r)), Nav: "egresses", Data: d})
}

func egressesTitle(l lang) string { return l.tr("出口", "Egresses") }
func importTitle(l lang) string   { return l.tr("导入节点", "Import nodes") }

// egressImportDialog is the import dialog: paste a node list, preview, confirm.
// It lives on the list page, so a direct visit shows that.
func (p *Panel) egressImportDialog(w http.ResponseWriter, r *http.Request) {
	if !htmx(r) {
		p.egressesPage(w, r)
		return
	}
	rows, inline := p.egressRows()
	d := egressesData{Rows: rows, Inline: inline, Form: importForm{Scheme: "socks5", Absorb: true}}
	p.modal(w, r, http.StatusOK, "egresses", page{Title: importTitle(langOf(r)), Nav: "egresses", Data: d})
}

func (p *Panel) egressesError(w http.ResponseWriter, r *http.Request, msg string, f importForm) {
	rows, inline := p.egressRows()
	pg := page{Title: egressesTitle(langOf(r)), Nav: "egresses", Error: msg, Data: egressesData{Rows: rows, Inline: inline, Form: f}}
	p.dialog(w, r, http.StatusBadRequest, "egresses", pg)
}

func importOptions(f importForm) (egress.ImportOptions, []string) {
	return egress.ImportOptions{DefaultScheme: f.Scheme, NamePrefix: f.Prefix}, confstore.ParseList(f.Tags)
}

// egressImport previews an import (step=preview) or applies it (step=confirm).
// "Save as node" on an inline egress imports its URL, looked up server-side so
// no proxy password ever travels through the page.
func (p *Panel) egressImport(w http.ResponseWriter, r *http.Request) {
	f := importForm{Text: r.PostFormValue("text"), Scheme: r.PostFormValue("scheme"), Prefix: strings.TrimSpace(r.PostFormValue("prefix")),
		Region: strings.TrimSpace(r.PostFormValue("region")), Tags: r.PostFormValue("tags"), Absorb: r.PostFormValue("absorb") == "on"}
	if f.Scheme != "http" {
		f.Scheme = "socks5"
	}
	l := langOf(r)
	s := p.conf.Snapshot()
	confirm := r.PostFormValue("step") == "confirm"
	if id := r.PostFormValue("inline_id"); id != "" {
		for _, k := range s.Keys {
			if k.Egress.ID == id && k.Egress.Inline && k.Egress.Name == "" {
				f.Text = k.Egress.URL
				break
			}
		}
		if f.Text == "" {
			p.egressesError(w, r, l.tr("找不到这个手填出口，可能已经被修改。", "That typed-in egress no longer exists; it may have been changed."), importForm{Scheme: "socks5", Absorb: true})
			return
		}
		f.Absorb = true
		if name := strings.TrimSpace(r.PostFormValue("name")); name != "" {
			f.Text += " #" + name
		}
		confirm = true
	}
	if strings.TrimSpace(f.Text) == "" {
		p.egressesError(w, r, l.tr("请粘贴要导入的节点。", "Paste the nodes to import."), f)
		return
	}
	opt, tags := importOptions(f)
	if !confirm {
		rows := egress.PlanImport(s.Egresses, f.Text, s.EgressRefs(), opt)
		d := egressesData{Form: f, Preview: rows}
		d.Rows, d.Inline = p.egressRows()
		for _, x := range rows {
			if x.Status == egress.ImportNew || x.Status == egress.ImportAbsorbs {
				d.New++
			}
		}
		if htmx(r) { // the preview replaces the dialog body
			p.modal(w, r, http.StatusOK, "egresses", page{Title: importTitle(l), Nav: "egresses", Data: d})
			return
		}
		p.done(w, r, "egresses")
		return
	}
	rows, _, err := p.conf.ImportEgresses(nil, f.Text, opt, f.Region, tags, f.Absorb)
	if err != nil {
		p.egressesError(w, r, saveError(l, err), f)
		return
	}
	var ids []string
	for _, x := range rows {
		if x.Status == egress.ImportNew || x.Status == egress.ImportAbsorbs {
			ids = append(ids, egress.ID(x.URL))
		}
	}
	if len(ids) > 0 {
		p.probe(ids...)
	}
	p.done(w, r, "egresses?ok=imported")
}

func (p *Panel) egressTest(w http.ResponseWriter, r *http.Request) {
	if id := r.PostFormValue("id"); id != "" {
		p.probe(id)
	} else {
		p.probe()
	}
	back := "egresses"
	if n := r.PostFormValue("name"); n != "" && egress.ValidateName(n) == nil {
		back += "/" + n
	}
	p.redirect(w, r, back+"?ok=testing")
}

// egressExport downloads the nodes as import lines. They include proxy
// passwords, so the export is audited and never cached.
func (p *Panel) egressExport(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString("# search-warden egress export: contains proxy credentials\n")
	for _, n := range p.conf.Snapshot().Egresses.Nodes() {
		fmt.Fprintf(&b, "%s#%s\n", egress.WithPort(n.URL), n.Name)
	}
	p.audit(r, "egress.export", strconv.Itoa(len(p.conf.Snapshot().Egresses.Nodes()))+" nodes")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="egresses.txt"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String()))
}

type egressData struct {
	Row    egressRow
	Others []option // nodes a deleted node's keys may move to
}

func (p *Panel) egressData(name string) (egressData, bool) {
	rows, _ := p.egressRows()
	for _, row := range rows {
		if !row.IsDirect && strings.EqualFold(row.Node.Name, name) {
			d := egressData{Row: row}
			for _, o := range rows {
				if !o.IsDirect && o.Node.Name != row.Node.Name {
					d.Others = append(d.Others, option{o.Node.Name, o.Node.Name})
				}
			}
			return d, true
		}
	}
	return egressData{}, false
}

func (p *Panel) egressPage(w http.ResponseWriter, r *http.Request) {
	d, ok := p.egressData(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	pg := page{Title: d.Row.Node.Name, Nav: "egresses", Data: d}
	p.dialog(w, r, http.StatusOK, "egress", pg)
}

func (p *Panel) egressFail(w http.ResponseWriter, r *http.Request, d egressData, msg string) {
	pg := page{Title: d.Row.Node.Name, Nav: "egresses", Error: msg, Data: d}
	p.dialog(w, r, http.StatusBadRequest, "egress", pg)
}

func (p *Panel) egressSave(w http.ResponseWriter, r *http.Request) {
	d, ok := p.egressData(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	str := func(k string) *string { v := strings.TrimSpace(r.PostFormValue(k)); return &v }
	patch := confstore.EgressPatch{Region: str("region"), Note: str("note")}
	if u := strings.TrimSpace(r.PostFormValue("url")); u != "" { // blank keeps the current URL (and its password)
		patch.URL = &u
	}
	on := r.PostFormValue("enabled") == "on"
	patch.Enabled = &on
	l := langOf(r)
	maxKeys, valid := optInt(r.PostFormValue("max_keys"))
	if !valid || (maxKeys != nil && *maxKeys < 0) {
		p.egressFail(w, r, d, l.tr("最多绑定的 key 数必须是非负整数。", "The key limit must be a non-negative integer."))
		return
	}
	if maxKeys == nil {
		zero := 0
		maxKeys = &zero
	}
	patch.MaxKeys = maxKeys
	tags := confstore.ParseList(r.PostFormValue("tags"))
	patch.Tags = &tags
	if _, err := p.conf.UpdateEgress(nil, d.Row.Node.Name, patch); err != nil {
		p.egressFail(w, r, d, saveError(l, err))
		return
	}
	if patch.URL != nil {
		if nd, ok := p.egressData(d.Row.Node.Name); ok {
			p.probe(nd.Row.ID)
		}
	}
	p.done(w, r, "egresses?ok=saved")
}

func (p *Panel) egressRename(w http.ResponseWriter, r *http.Request) {
	d, ok := p.egressData(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	to := strings.TrimSpace(r.PostFormValue("to"))
	if _, err := p.conf.RenameEgress(nil, d.Row.Node.Name, to); err != nil {
		p.egressFail(w, r, d, saveError(langOf(r), err))
		return
	}
	p.done(w, r, "egresses?ok=renamed")
}

func (p *Panel) egressDelete(w http.ResponseWriter, r *http.Request) {
	d, ok := p.egressData(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	mode := confstore.DeleteRefuse
	switch r.PostFormValue("mode") {
	case "direct":
		mode = confstore.DeleteToDirect
	case "node":
		mode = confstore.DeleteToNode
	case "inline":
		mode = confstore.DeleteToInline
	}
	_, err := p.conf.DeleteEgress(nil, d.Row.Node.Name, mode, r.PostFormValue("target"))
	var inUse *confstore.InUseError
	if errors.As(err, &inUse) {
		p.egressFail(w, r, d, fmt.Sprintf(langOf(r).tr("还有 %d 个 key 在使用这个出口，请选择删除后这些 key 怎么办。",
			"%d keys still use this egress; choose what happens to them when it is deleted."), len(inUse.Keys)))
		return
	}
	if err != nil {
		p.egressFail(w, r, d, saveError(langOf(r), err))
		return
	}
	p.done(w, r, "egresses?ok=deleted")
}
