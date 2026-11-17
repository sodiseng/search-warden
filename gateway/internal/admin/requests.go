package admin

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sodiseng/search-warden/gateway/internal/store"
)

// Request monitor (ARCHITECTURE.md §16): filters, paging and a live mode that
// htmx refreshes every few seconds.

const requestsPerPage = 50

type requestFilter struct {
	Provider, Token, Key, IP, Op string
	Status                       int
	Errors, Live                 bool
	Before                       int64
}

func (f requestFilter) query() store.LogQuery {
	return store.LogQuery{Provider: f.Provider, TokenID: f.Token, KeyID: f.Key, ClientIP: f.IP, Operation: f.Op,
		Status: f.Status, ErrorOnly: f.Errors, BeforeID: f.Before, Limit: requestsPerPage}
}

// encode renders the filter as a query string (without paging).
func (f requestFilter) encode() string {
	v := url.Values{}
	set := func(k, s string) {
		if s != "" {
			v.Set(k, s)
		}
	}
	set("provider", f.Provider)
	set("token", f.Token)
	set("key", f.Key)
	set("ip", f.IP)
	set("op", f.Op)
	if f.Status > 0 {
		v.Set("status", strconv.Itoa(f.Status))
	}
	if f.Errors {
		v.Set("errors", "1")
	}
	return v.Encode()
}

func parseFilter(q url.Values) requestFilter {
	f := requestFilter{Provider: q.Get("provider"), Token: q.Get("token"), Key: q.Get("key"), IP: strings.TrimSpace(q.Get("ip")),
		Op: strings.TrimSpace(q.Get("op")), Errors: q.Get("errors") == "1", Live: q.Get("live") == "1"}
	if f.Provider != "tavily" && f.Provider != "exa" {
		f.Provider = ""
	}
	f.Status, _ = strconv.Atoi(q.Get("status"))
	f.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	if f.Live {
		f.Before = 0 // live mode always shows the newest rows
	}
	return f
}

type logRow struct {
	store.RequestLog
	TokenName, KeyLabel, EgressLabel string
	Error                            bool
}

type option struct{ Value, Label string }

type requestsData struct {
	F            requestFilter
	Query        string
	Rows         []logRow
	Next         int64 // ID to page from; 0 = no more
	Tokens, Keys []option
	NoStore      bool
}

type labels struct{ token, key, egress map[string]string }

func (p *Panel) labels() labels {
	s := p.conf.Snapshot()
	l := labels{token: map[string]string{}, key: map[string]string{}, egress: map[string]string{}}
	for _, t := range s.Tokens {
		l.token[t.ID] = t.Name
	}
	for _, k := range s.Keys {
		l.key[k.ID] = k.Provider + " · " + k.DisplayName()
		l.egress[k.Egress.ID] = k.Egress.Label()
	}
	for _, e := range p.egresses() {
		if _, ok := l.egress[e.ID]; !ok {
			l.egress[e.ID] = e.Label
		}
	}
	return l
}

func (l labels) row(r store.RequestLog) logRow {
	row := logRow{RequestLog: r, TokenName: l.token[r.TokenID], KeyLabel: l.key[r.KeyID], EgressLabel: l.egress[r.EgressID],
		Error: r.Status >= 400 || r.ErrorKind != ""}
	if row.TokenName == "" && r.TokenID != "" {
		row.TokenName = r.TokenID
	}
	if row.KeyLabel == "" && r.KeyID != "" {
		row.KeyLabel = r.KeyID
	}
	return row
}

func (p *Panel) requestsData(r *http.Request) requestsData {
	f := parseFilter(r.URL.Query())
	d := requestsData{F: f, Query: f.encode(), NoStore: p.st == nil}
	s := p.conf.Snapshot()
	for _, t := range s.Tokens {
		d.Tokens = append(d.Tokens, option{t.ID, t.Name})
	}
	for _, k := range s.Keys {
		d.Keys = append(d.Keys, option{k.ID, k.Provider + " · " + k.DisplayName()})
	}
	if p.st == nil {
		return d
	}
	rows, err := p.st.QueryRequestLogs(r.Context(), f.query())
	if err != nil {
		p.log.Warn("query request logs", "err", err)
	}
	l := p.labels()
	for _, x := range rows {
		d.Rows = append(d.Rows, l.row(x))
	}
	if len(rows) == requestsPerPage && !f.Live {
		d.Next = rows[len(rows)-1].ID
	}
	return d
}

func (p *Panel) requests(w http.ResponseWriter, r *http.Request) {
	p.render(w, r, http.StatusOK, "requests", page{Title: langOf(r).tr("请求", "Requests"), Nav: "requests", Data: p.requestsData(r)})
}

// requestRows is the table body alone, polled by htmx in live mode.
func (p *Panel) requestRows(w http.ResponseWriter, r *http.Request) {
	p.renderPart(w, r, "requests", "rows", page{Data: p.requestsData(r)})
}

type requestDetail struct {
	Row      logRow
	Attempts []attemptRow
}

type attemptRow struct {
	store.Attempt
	KeyLabel, EgressLabel string
}

func (p *Panel) requestDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || p.st == nil {
		http.NotFound(w, r)
		return
	}
	rec, err := p.st.GetRequestLog(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	l := p.labels()
	d := requestDetail{Row: l.row(rec)}
	atts, err := p.st.ListAttempts(r.Context(), rec.RequestID)
	if err != nil {
		p.log.Warn("list attempts", "err", err)
	}
	for _, a := range atts {
		d.Attempts = append(d.Attempts, attemptRow{Attempt: a, KeyLabel: l.key[a.KeyID], EgressLabel: l.egress[a.EgressID]})
	}
	pg := page{Title: langOf(r).tr("请求详情", "Request details"), Nav: "requests", Data: d}
	p.dialog(w, r, http.StatusOK, "request", pg) // the list opens the detail in the shared dialog
}
