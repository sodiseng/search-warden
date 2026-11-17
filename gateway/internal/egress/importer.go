package egress

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Candidate is one parsed import line.
type Candidate struct {
	URL  string // normalized
	Name string // optional, from "#name" or "|name"
}

// ParseImportLine accepts the formats of §7.9.1:
//
//	socks5://user:pass@host:port   (any supported scheme; socks5h accepted)
//	host:port
//	host:port:user:pass            (common provider export format)
//	user:pass@host:port
//
// optionally followed by " #name" or "|name". defaultScheme ("socks5" or
// "http") is used when no scheme is given. Blank and "#" lines return a zero
// Candidate and nil error.
func ParseImportLine(line, defaultScheme string) (Candidate, error) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return Candidate{}, nil
	}
	var name string
	if i := strings.LastIndex(s, "|"); i >= 0 {
		s, name = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	} else if i := strings.Index(s, " #"); i >= 0 {
		s, name = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+2:])
	} else if i := strings.Index(s, "#"); i >= 0 && strings.Contains(s, "://") {
		// URL fragment used as a name: socks5://h:1#hk-1
		s, name = s[:i], strings.TrimSpace(s[i+1:])
	}
	if name != "" {
		if err := ValidateName(name); err != nil {
			return Candidate{}, err
		}
	}

	raw, err := toURL(s, defaultScheme)
	if err != nil {
		return Candidate{}, err
	}
	norm, err := NormalizeURL(raw)
	if err != nil {
		return Candidate{}, err
	}
	return Candidate{URL: norm, Name: name}, nil
}

func toURL(s, defaultScheme string) (string, error) {
	if strings.Contains(s, "://") {
		return s, nil
	}
	if defaultScheme == "" {
		defaultScheme = "socks5"
	}
	if strings.Contains(s, "@") { // user:pass@host:port
		return defaultScheme + "://" + s, nil
	}
	// IPv6 "[::1]:1080" or "host:port"
	if host, port, err := net.SplitHostPort(s); err == nil {
		if _, perr := strconv.Atoi(port); perr == nil && host != "" {
			return defaultScheme + "://" + s, nil
		}
	}
	// host:port:user:pass — the password itself may contain ':'
	parts := strings.SplitN(s, ":", 4)
	if len(parts) == 4 {
		host, port, user, pass := parts[0], parts[1], parts[2], parts[3]
		if _, err := strconv.Atoi(port); err == nil && host != "" && user != "" {
			u := url.URL{Scheme: defaultScheme, User: url.UserPassword(user, pass), Host: net.JoinHostPort(host, port)}
			return u.String(), nil
		}
	}
	return "", errors.New("unrecognized proxy format (expected scheme://..., host:port, host:port:user:pass or user:pass@host:port)")
}

// ImportStatus is the preview verdict for one line (§7.9.1).
type ImportStatus string

const (
	ImportNew       ImportStatus = "new"
	ImportDuplicate ImportStatus = "duplicate"      // proxy already a node (or repeated in the paste)
	ImportAbsorbs   ImportStatus = "absorbs_inline" // new node that also adopts keys' inline URLs
	ImportInvalid   ImportStatus = "invalid"
)

// ImportRow is one previewed line.
type ImportRow struct {
	Line     int // 1-based
	Status   ImportStatus
	Name     string // final name (generated when absent)
	URL      string // normalized; never shown unredacted in the UI list
	Existing string // for duplicates: the node it duplicates
	Error    string // for invalid lines (contains no credentials)
}

// ImportOptions controls PlanImport.
type ImportOptions struct {
	DefaultScheme string // socks5 (default) or http
	NamePrefix    string // default "node-"
}

// PlanImport previews an import against the current registry without changing it.
// inlineValues are the egress fields of all keys, used to detect inline URLs that
// the new nodes will adopt.
func PlanImport(r *Registry, text string, inlineValues []string, opt ImportOptions) []ImportRow {
	if opt.NamePrefix == "" {
		opt.NamePrefix = "node-"
	}
	inline := map[string]bool{}
	for _, u := range r.InlineOnly(inlineValues) {
		inline[u] = true
	}
	taken := map[string]bool{}  // lower-case names allocated in this batch
	seenURL := map[string]int{} // url → line in this batch
	var rows []ImportRow
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		c, err := ParseImportLine(line, opt.DefaultScheme)
		if err == nil && c.URL == "" {
			continue // blank or comment
		}
		row := ImportRow{Line: i + 1}
		if err != nil {
			row.Status, row.Error = ImportInvalid, err.Error()
			rows = append(rows, row)
			continue
		}
		row.URL = c.URL
		if n, ok := r.NodeForURL(c.URL); ok {
			row.Status, row.Existing, row.Name = ImportDuplicate, n.Name, n.Name
			rows = append(rows, row)
			continue
		}
		if prev, ok := seenURL[c.URL]; ok {
			row.Status, row.Error = ImportDuplicate, fmt.Sprintf("same proxy as line %d", prev)
			rows = append(rows, row)
			continue
		}
		name := c.Name
		if name != "" {
			if _, used := r.Lookup(name); used || taken[strings.ToLower(name)] {
				row.Status, row.Error = ImportInvalid, fmt.Sprintf("name %q is already used", name)
				rows = append(rows, row)
				continue
			}
		} else {
			name = r.NextName(opt.NamePrefix, taken)
		}
		taken[strings.ToLower(name)] = true
		seenURL[c.URL] = row.Line
		row.Name, row.Status = name, ImportNew
		if inline[c.URL] {
			row.Status = ImportAbsorbs
		}
		rows = append(rows, row)
	}
	return rows
}
