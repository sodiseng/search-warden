package confstore

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/sodiseng/search-warden/gateway/internal/auth/token"
)

// Token operations for the panel (ARCHITECTURE.md §11.2). A token created or
// rotated here is returned in plaintext exactly once; only its hash and
// display prefix are written to tokens.yaml.

// TokenSpec is the editable part of a token. Empty lists mean "all" and are
// left out of the file; zero limits mean "unlimited".
type TokenSpec struct {
	Name      string
	Providers []string
	Surfaces  []string
	Tools     []string
	KeyGroup  string // "", all, free or paid
	IPAllow   []string
	Expires   string // "" = never; 2027-01-01 or RFC 3339
	Enabled   bool
	Limits    TokenLimits
	Note      string
}

// SpecOf returns the editable fields of a token.
func SpecOf(t Token) TokenSpec {
	s := TokenSpec{Name: t.Name, Tools: t.Tools, KeyGroup: t.KeyGroup, Enabled: t.Enabled, Limits: t.Limits, Note: t.Note}
	if len(t.Providers) < len(Providers) {
		s.Providers = t.Providers
	}
	if len(t.Surfaces) < 2 {
		s.Surfaces = t.Surfaces
	}
	for _, p := range t.IPAllow {
		s.IPAllow = append(s.IPAllow, p.String())
	}
	if !t.Expires.IsZero() {
		s.Expires = t.Expires.Format("2006-01-02T15:04:05Z07:00")
	}
	return s
}

// TokenByID finds a token by its stable ID.
func (s *Snapshot) TokenByID(id string) (Token, bool) {
	for _, t := range s.Tokens {
		if t.ID == id {
			return t, true
		}
	}
	return Token{}, false
}

func tokenItem(tx *Tx, id string) (*Doc, *yaml.Node, *yaml.Node, int, error) {
	t, ok := tx.Snapshot().TokenByID(id)
	if !ok {
		return nil, nil, nil, 0, fmt.Errorf("unknown token id %q", id)
	}
	d, err := tx.Doc(FileTokens)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	seq := mapGet(d.Top(), "tokens")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil, nil, nil, 0, errors.New("tokens.yaml: tokens list not found")
	}
	for i, it := range seq.Content {
		if h := mapGet(it, "token_hash"); h != nil && strings.EqualFold(h.Value, t.Hash) {
			return d, it, seq, i, nil
		}
	}
	return nil, nil, nil, 0, fmt.Errorf("token %q not found in tokens.yaml", t.Name)
}

type field struct {
	key string
	val *yaml.Node // nil: the field is left out (default applies)
}

// specFields renders a spec as tokens.yaml fields, in file order.
func specFields(s TokenSpec) []field {
	opt := func(k string, v *yaml.Node, keep bool) field {
		if !keep {
			return field{key: k}
		}
		return field{k, v}
	}
	l := s.Limits
	limits := mapNode()
	add := func(k string, v int64) {
		if v > 0 {
			limits.Content = append(limits.Content, strNode(k), intNode(v))
		}
	}
	add("rpm", int64(l.RPM))
	add("concurrency", int64(l.Concurrency))
	add("daily_requests", l.DailyRequests)
	add("monthly_requests", l.MonthlyRequests)
	add("monthly_credits", l.MonthlyCredits)
	if l.MonthlyUSD > 0 {
		limits.Content = append(limits.Content, strNode("monthly_usd"), floatNode(l.MonthlyUSD))
	}
	return []field{
		opt("providers", flowList(s.Providers), len(s.Providers) > 0),
		opt("surfaces", flowList(s.Surfaces), len(s.Surfaces) > 0),
		opt("tools", flowList(s.Tools), len(s.Tools) > 0),
		opt("key_group", strNode(s.KeyGroup), s.KeyGroup != "" && s.KeyGroup != "all"),
		opt("ip_allowlist", flowList(s.IPAllow), len(s.IPAllow) > 0),
		opt("expires", strNode(strings.TrimSpace(s.Expires)), strings.TrimSpace(s.Expires) != ""),
		opt("enabled", boolNode(false), !s.Enabled),
		opt("limits", limits, len(limits.Content) > 0),
		opt("note", strNode(s.Note), s.Note != ""),
	}
}

// writeSpec updates an existing entry in place (comments elsewhere survive).
func writeSpec(d *Doc, item *yaml.Node, s TokenSpec) {
	d.SetField(item, "name", strNode(strings.TrimSpace(s.Name)))
	for _, f := range specFields(s) {
		switch {
		case f.val == nil:
			d.DeleteField(item, f.key)
		case f.key == "limits":
			d.SetBlockField(item, f.key, f.val)
		default:
			d.SetField(item, f.key, f.val)
		}
	}
}

// AddToken creates a token and returns its plaintext (shown once).
func (m *Manager) AddToken(base map[string]string, s TokenSpec) (string, *Snapshot, error) {
	plain, err := token.Generate()
	if err != nil {
		return "", nil, err
	}
	snap, err := m.Edit(SourcePanel, base, func(tx *Tx) error {
		d, err := tx.Doc(FileTokens)
		if err != nil {
			return err
		}
		top, err := d.ensureTop()
		if err != nil {
			return err
		}
		item := mapNode("name", strNode(strings.TrimSpace(s.Name)), "token_hash", strNode(token.Hash(plain)), "token_prefix", strNode(token.DisplayPrefix(plain)))
		for _, f := range specFields(s) {
			if f.val != nil {
				item.Content = append(item.Content, strNode(f.key), f.val)
			}
		}
		d.AppendItem(top, "tokens", item)
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return plain, snap, nil
}

// UpdateToken replaces a token's editable fields; its secret is unchanged.
func (m *Manager) UpdateToken(base map[string]string, id string, s TokenSpec) (*Snapshot, error) {
	return m.Edit(SourcePanel, base, func(tx *Tx) error {
		d, item, _, _, err := tokenItem(tx, id)
		if err != nil {
			return err
		}
		writeSpec(d, item, s)
		return nil
	})
}

// SetTokenEnabled switches a token on or off.
func (m *Manager) SetTokenEnabled(base map[string]string, id string, on bool) (*Snapshot, error) {
	return m.Edit(SourcePanel, base, func(tx *Tx) error {
		d, item, _, _, err := tokenItem(tx, id)
		if err != nil {
			return err
		}
		if on {
			d.DeleteField(item, "enabled")
		} else {
			d.SetField(item, "enabled", boolNode(false))
		}
		return nil
	})
}

// RotateToken gives a token a new secret and returns it (shown once). The old
// secret stops working as soon as the edit is applied. The token's ID changes
// with its hash.
func (m *Manager) RotateToken(base map[string]string, id string) (string, *Snapshot, error) {
	plain, err := token.Generate()
	if err != nil {
		return "", nil, err
	}
	snap, err := m.Edit(SourcePanel, base, func(tx *Tx) error {
		d, item, _, _, err := tokenItem(tx, id)
		if err != nil {
			return err
		}
		d.SetField(item, "token_hash", strNode(token.Hash(plain)))
		d.SetField(item, "token_prefix", strNode(token.DisplayPrefix(plain)))
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return plain, snap, nil
}

// DeleteToken removes a token.
func (m *Manager) DeleteToken(base map[string]string, id string) (*Snapshot, error) {
	return m.Edit(SourcePanel, base, func(tx *Tx) error {
		d, _, seq, i, err := tokenItem(tx, id)
		if err != nil {
			return err
		}
		d.DeleteItem(seq, i)
		return nil
	})
}

// ParseList splits a comma- or newline-separated list, dropping blanks.
func ParseList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// ParseLimit reads a non-negative integer form value ("" = 0).
func ParseLimit(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%q is not a non-negative whole number", s)
	}
	return v, nil
}
