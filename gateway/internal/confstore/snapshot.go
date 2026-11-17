package confstore

import (
	"fmt"
	"strings"

	"github.com/sodiseng/search-warden/gateway/internal/egress"
)

// Key is an upstream key with its egress resolved.
type Key struct {
	ID            string // KeyID(provider, secret)
	Provider      string
	Secret        string
	Label         string
	Plan          string
	EgressRef     string // as written in the file
	Egress        egress.Resolved
	Region        string
	Priority      int
	BudgetMonthly float64
	Team          string  // Exa: keys of one team share its rate limit
	RateLimit     float64 // overrides the main endpoint limit; 0 = documented default
	Weight        float64 // share of traffic within its priority tier (default 1)
	ResetDay      int
	Enabled       bool
	Note          string
	Line          int
}

// DisplayName is the label, or a masked key when there is no label.
func (k Key) DisplayName() string {
	if k.Label != "" {
		return k.Label
	}
	return MaskSecret(k.Secret)
}

// MaskSecret shows only the head and tail of a key: "tvly-dev-…a1b2".
func MaskSecret(s string) string {
	if len(s) <= 12 {
		return strings.Repeat("*", len(s))
	}
	head := 4
	if i := strings.LastIndex(s[:min(len(s)-4, 12)], "-"); i >= 0 {
		head = i + 1
	}
	return s[:head] + "…" + s[len(s)-4:]
}

// Snapshot is an immutable, fully validated view of all config files.
type Snapshot struct {
	Config   Config
	Egresses *egress.Registry
	Keys     []Key // tavily then exa, file order
	Tokens   []Token
	Versions map[string]string // file → content version of the applied file
}

// KeysFor returns the keys of one provider in file order.
func (s *Snapshot) KeysFor(provider string) []Key {
	var out []Key
	for _, k := range s.Keys {
		if k.Provider == provider {
			out = append(out, k)
		}
	}
	return out
}

// KeyByID finds a key.
func (s *Snapshot) KeyByID(id string) (Key, bool) {
	for _, k := range s.Keys {
		if k.ID == id {
			return k, true
		}
	}
	return Key{}, false
}

// EgressRefs returns every key's raw egress value (used for usage counts and the inline group).
func (s *Snapshot) EgressRefs() []string {
	out := make([]string, len(s.Keys))
	for i, k := range s.Keys {
		out[i] = k.EgressRef
	}
	return out
}

// KeysUsingEgress lists keys whose resolved egress has the given ID.
func (s *Snapshot) KeysUsingEgress(id string) []Key {
	var out []Key
	for _, k := range s.Keys {
		if k.Egress.ID == id {
			out = append(out, k)
		}
	}
	return out
}

// TokenByHash finds a token by its SHA-256 hash.
func (s *Snapshot) TokenByHash(hash string) (Token, bool) {
	for _, t := range s.Tokens {
		if t.Hash == hash {
			return t, true
		}
	}
	return Token{}, false
}

// models is one candidate set of parsed files.
type models struct {
	cfg  *Config
	eg   *EgressFile
	keys map[string]*KeyFile
	toks *TokenFile
}

// assemble resolves cross-file references. Errors are attributed to key files.
func assemble(m models, versions map[string]string) (*Snapshot, Errors) {
	var errs Errors
	s := &Snapshot{Config: *m.cfg, Egresses: m.eg.Registry, Tokens: m.toks.Tokens, Versions: versions}
	owner := map[string]string{} // secret → file:line, across providers
	for _, p := range Providers {
		kf := m.keys[p]
		if kf == nil {
			continue
		}
		file := KeysFile(p)
		for _, it := range kf.Items {
			res, err := m.eg.Registry.Resolve(it.EgressRef)
			if err != nil {
				errs = append(errs, FileError{File: file, Line: it.EgressLine, Msg: fmt.Sprintf("%s: %v", labelOr(it), err)})
				continue
			}
			if prev, dup := owner[it.Secret]; dup {
				errs = append(errs, FileError{File: file, Line: it.Line, Msg: "the same key is also listed in " + prev})
				continue
			}
			owner[it.Secret] = fmt.Sprintf("%s:%d", file, it.Line)
			s.Keys = append(s.Keys, Key{
				ID: KeyID(p, it.Secret), Provider: p, Secret: it.Secret, Label: it.Label, Plan: it.Plan,
				EgressRef: it.EgressRef, Egress: res, Region: it.Region, Priority: it.Priority,
				BudgetMonthly: it.BudgetMonthly, Team: it.Team, RateLimit: it.RateLimit, Weight: it.Weight, ResetDay: it.ResetDay, Enabled: it.Enabled, Note: it.Note, Line: it.Line,
			})
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return s, nil
}

func labelOr(it KeyItem) string {
	if it.Label != "" {
		return it.Label
	}
	return MaskSecret(it.Secret)
}
