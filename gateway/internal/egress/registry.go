package egress

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Node is a named egress from egresses.yaml.
type Node struct {
	Name    string
	URL     string // as written; normalized by NewRegistry
	Enabled bool
	MaxKeys int
	Region  string
	Tags    []string
	Note    string
}

// Resolved is the effective egress of one key.
type Resolved struct {
	ID      string // stable; DirectID for direct
	URL     string // normalized proxy URL; "" for direct
	Name    string // node name when the URL belongs to a node ("" for direct / unsaved inline)
	Inline  bool   // the key wrote a URL instead of a name
	Enabled bool   // false when the matching node is disabled
	MaxKeys int
	Region  string
}

// Direct reports whether r is the server's own IP.
func (r Resolved) Direct() bool { return r.ID == DirectID }

// Label is a short human description (password-free).
func (r Resolved) Label() string {
	switch {
	case r.Direct():
		return Direct
	case r.Name != "" && r.Inline:
		return r.Name + " (inline)"
	case r.Name != "":
		return r.Name
	default:
		return Redact(r.URL)
	}
}

// Registry indexes nodes by name and by normalized URL.
type Registry struct {
	nodes  []Node // normalized, in file order
	byName map[string]int
	byURL  map[string]int
}

// NewRegistry validates the node list as a whole (§14.4: a file applies fully or not at all).
// It rejects invalid names, invalid URLs, duplicate names and two names for the same proxy.
func NewRegistry(nodes []Node) (*Registry, []error) {
	r := &Registry{byName: map[string]int{}, byURL: map[string]int{}}
	var errs []error
	for i, n := range nodes {
		where := fmt.Sprintf("egresses[%d]", i)
		if n.Name != "" {
			where = fmt.Sprintf("egresses[%d] (%s)", i, n.Name)
		}
		if err := ValidateName(n.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
			continue
		}
		norm, err := NormalizeURL(n.URL)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
			continue
		}
		if n.MaxKeys < 0 {
			errs = append(errs, fmt.Errorf("%s: max_keys must be >= 0", where))
			continue
		}
		key := strings.ToLower(n.Name)
		if j, dup := r.byName[key]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate name (already used by egresses[%d])", where, j))
			continue
		}
		if j, dup := r.byURL[norm]; dup {
			errs = append(errs, fmt.Errorf("%s: same proxy as %q; one proxy may only have one name", where, r.nodes[j].Name))
			continue
		}
		n.URL = norm
		r.byName[key] = len(r.nodes)
		r.byURL[norm] = len(r.nodes)
		r.nodes = append(r.nodes, n)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return r, nil
}

// Nodes returns the normalized nodes in file order.
func (r *Registry) Nodes() []Node { return append([]Node(nil), r.nodes...) }

// Lookup finds a node by name (case-insensitive).
func (r *Registry) Lookup(name string) (Node, bool) {
	i, ok := r.byName[strings.ToLower(name)]
	if !ok {
		return Node{}, false
	}
	return r.nodes[i], true
}

// NodeForURL finds the node owning a normalized URL.
func (r *Registry) NodeForURL(normalized string) (Node, bool) {
	i, ok := r.byURL[normalized]
	if !ok {
		return Node{}, false
	}
	return r.nodes[i], true
}

func fromNode(n Node, inline bool) Resolved {
	return Resolved{ID: ID(n.URL), URL: n.URL, Name: n.Name, Inline: inline, Enabled: n.Enabled, MaxKeys: n.MaxKeys, Region: n.Region}
}

// Resolve turns a key's egress field into its effective egress (§7.9.2 rule 1).
func (r *Registry) Resolve(value string) (Resolved, error) {
	ref, err := ParseRef(value)
	if err != nil {
		return Resolved{}, err
	}
	switch ref.Kind {
	case RefDirect:
		return Resolved{ID: DirectID, Enabled: true}, nil
	case RefNamed:
		n, ok := r.Lookup(ref.Name)
		if !ok {
			return Resolved{}, fmt.Errorf("egress %q is not defined in egresses.yaml", ref.Name)
		}
		return fromNode(n, false), nil
	default: // inline
		if n, ok := r.NodeForURL(ref.URL); ok {
			return fromNode(n, true), nil // same proxy as a node: one egress
		}
		return Resolved{ID: ID(ref.URL), URL: ref.URL, Inline: true, Enabled: true}, nil
	}
}

// CanonicalRef is what the panel writes back for a user input (§7.9.2 rule 3):
// a node name when the input is or matches a node, the normalized URL otherwise.
// Hand-edited files are never rewritten with this (rule 5).
func (r *Registry) CanonicalRef(input string) (string, error) {
	res, err := r.Resolve(input)
	if err != nil {
		// Accept the import formats in the panel field too (e.g. host:port:user:pass).
		c, perr := ParseImportLine(input, "socks5")
		if perr != nil || c.URL == "" {
			return "", err
		}
		res, err = r.Resolve(c.URL)
		if err != nil {
			return "", err
		}
	}
	switch {
	case res.Direct():
		return Direct, nil
	case res.Name != "":
		return res.Name, nil
	default:
		return res.URL, nil
	}
}

// NextName returns prefix+N with N one higher than any existing prefix+number name.
func (r *Registry) NextName(prefix string, taken map[string]bool) string {
	max := 0
	consider := func(name string) {
		if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
			return
		}
		if n, err := strconv.Atoi(name[len(prefix):]); err == nil && n > max {
			max = n
		}
	}
	for _, n := range r.nodes {
		consider(n.Name)
	}
	for name := range taken {
		consider(name)
	}
	for {
		max++
		cand := prefix + strconv.Itoa(max)
		if _, used := r.byName[strings.ToLower(cand)]; !used && !taken[strings.ToLower(cand)] {
			return cand
		}
	}
}

// Usage counts keys per egress ID; used for max_keys warnings and the egress list.
type Usage map[string]int

// Count resolves every key's egress field and tallies them. Unresolvable values are reported.
func (r *Registry) Count(values []string) (Usage, []error) {
	u := Usage{}
	var errs []error
	for i, v := range values {
		res, err := r.Resolve(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("key[%d]: %w", i, err))
			continue
		}
		u[res.ID]++
	}
	return u, errs
}

// InlineOnly lists inline egresses that match no node: the "unsaved inline egresses" group (§7.9.1).
func (r *Registry) InlineOnly(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		res, err := r.Resolve(v)
		if err != nil || !res.Inline || res.Name != "" || seen[res.URL] {
			continue
		}
		seen[res.URL] = true
		out = append(out, res.URL)
	}
	sort.Strings(out)
	return out
}
