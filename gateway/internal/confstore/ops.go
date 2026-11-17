package confstore

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/sodiseng/search-warden/gateway/internal/auth/password"
	"github.com/sodiseng/search-warden/gateway/internal/egress"
)

// Panel operations. Each runs as one Edit: validated as a whole, written with
// backups, rejected with *ConflictError if a file changed underneath.

// DetectProvider guesses a key's provider from its format (Tavily keys start with "tvly-").
func DetectProvider(secret string) string {
	if strings.HasPrefix(secret, "tvly-") {
		return "tavily"
	}
	return "exa"
}

func validProvider(p string) bool { return slices.Contains(Providers, p) }

// KeyInput is a key to add from the panel.
type KeyInput struct {
	Provider string // "" = DetectProvider
	Secret   string
	Label    string
	Plan     string // "" = file default
	Egress   string // "" = file default; node name, URL or any import format (§7.9.2 rule 3)
	Region   string
	Note     string
}

// ParseKeyLines parses the panel's batch format: key[,label][,egress][,plan] per line.
func ParseKeyLines(text string) ([]KeyInput, []error) {
	var out []KeyInput
	var errs []error
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ",")
		for j := range parts {
			parts[j] = strings.TrimSpace(parts[j])
		}
		if len(parts) > 4 {
			errs = append(errs, fmt.Errorf("line %d: expected key[,label][,egress][,plan]", i+1))
			continue
		}
		k := KeyInput{Secret: parts[0]}
		if len(parts) > 1 {
			k.Label = parts[1]
		}
		if len(parts) > 2 {
			k.Egress = parts[2]
		}
		if len(parts) > 3 {
			k.Plan = parts[3]
		}
		out = append(out, k)
	}
	return out, errs
}

// egressValue turns panel input into what is written to a key file: a node
// name when the input is or matches a node, otherwise the URL with its port.
func egressValue(reg *egress.Registry, input string) (string, error) {
	ref, err := reg.CanonicalRef(input)
	if err != nil {
		return "", err
	}
	if strings.Contains(ref, "://") {
		return egress.WithPort(ref), nil
	}
	return ref, nil
}

// AddKeys appends keys; keys that already exist are skipped and returned.
func (m *Manager) AddKeys(base map[string]string, items []KeyInput) (added, skipped []string, err error) {
	_, err = m.Edit(SourcePanel, base, func(tx *Tx) error {
		added, skipped = nil, nil
		existing := map[string]bool{}
		for _, k := range tx.Snapshot().Keys {
			existing[k.Secret] = true
		}
		for i, in := range items {
			secret := strings.TrimSpace(in.Secret)
			provider := in.Provider
			if provider == "" {
				provider = DetectProvider(secret)
			}
			if !validProvider(provider) {
				return fmt.Errorf("item %d: unknown provider %q", i+1, provider)
			}
			if existing[secret] {
				skipped = append(skipped, MaskSecret(secret))
				continue
			}
			existing[secret] = true
			node := mapNode("key", strNode(secret))
			addOpt := func(k, v string) {
				if v != "" {
					node.Content = append(node.Content, strNode(k), strNode(v))
				}
			}
			addOpt("label", in.Label)
			addOpt("plan", in.Plan)
			if in.Egress != "" {
				v, err := egressValue(tx.Snapshot().Egresses, in.Egress)
				if err != nil {
					return fmt.Errorf("item %d (%s): %w", i+1, MaskSecret(secret), err)
				}
				addOpt("egress", v)
			}
			addOpt("region", in.Region)
			addOpt("note", in.Note)
			d, err := tx.Doc(KeysFile(provider))
			if err != nil {
				return err
			}
			top, err := d.ensureTop()
			if err != nil {
				return err
			}
			d.AppendItem(top, "keys", node)
			added = append(added, MaskSecret(secret))
		}
		return nil
	})
	return added, skipped, err
}

// KeyPatch changes key fields. nil = unchanged; a pointer to "" removes the
// field so the file's defaults apply again.
type KeyPatch struct {
	Label, Plan, Egress, Region, Note, Team *string
	RateLimit                               *float64
	Weight                                  *float64
	Priority, ResetDay                      *int
	BudgetMonthly                           *float64
	Enabled                                 *bool
}

// keyItem locates a key's entry in its file and returns it as a mapping.
func keyItem(tx *Tx, k Key) (*Doc, *yaml.Node, error) {
	d, err := tx.Doc(KeysFile(k.Provider))
	if err != nil {
		return nil, nil, err
	}
	seq := mapGet(d.Top(), "keys")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil, nil, fmt.Errorf("%s: keys list not found", KeysFile(k.Provider))
	}
	for i, it := range seq.Content {
		v := it.Value
		if it.Kind == yaml.MappingNode {
			if kn := mapGet(it, "key"); kn != nil {
				v = kn.Value
			}
		}
		if v == k.Secret {
			return d, d.ItemMapping(seq, i, "key"), nil
		}
	}
	return nil, nil, fmt.Errorf("key %s not found in %s", k.DisplayName(), KeysFile(k.Provider))
}

// UpdateKeys applies one patch to several keys (e.g. batch "set egress").
func (m *Manager) UpdateKeys(base map[string]string, ids []string, patch KeyPatch) (*Snapshot, error) {
	return m.Edit(SourcePanel, base, func(tx *Tx) error {
		egressVal := ""
		if patch.Egress != nil && *patch.Egress != "" {
			v, err := egressValue(tx.Snapshot().Egresses, *patch.Egress)
			if err != nil {
				return err
			}
			egressVal = v
		}
		for _, id := range ids {
			k, ok := tx.Snapshot().KeyByID(id)
			if !ok {
				return fmt.Errorf("unknown key id %q", id)
			}
			d, item, err := keyItem(tx, k)
			if err != nil {
				return err
			}
			setStr := func(field string, v *string) {
				switch {
				case v == nil:
				case *v == "":
					d.DeleteField(item, field)
				default:
					d.SetField(item, field, strNode(*v))
				}
			}
			setStr("label", patch.Label)
			setStr("plan", patch.Plan)
			if patch.Egress != nil {
				setStr("egress", &egressVal)
			}
			setStr("region", patch.Region)
			setStr("note", patch.Note)
			setStr("team", patch.Team)
			if patch.RateLimit != nil {
				d.SetField(item, "rate_limit", floatNode(*patch.RateLimit))
			}
			if patch.Weight != nil {
				d.SetField(item, "weight", floatNode(*patch.Weight))
			}
			if patch.Priority != nil {
				d.SetField(item, "priority", intNode(int64(*patch.Priority)))
			}
			if patch.ResetDay != nil {
				d.SetField(item, "reset_day", intNode(int64(*patch.ResetDay)))
			}
			if patch.BudgetMonthly != nil {
				d.SetField(item, "budget_monthly", floatNode(*patch.BudgetMonthly))
			}
			if patch.Enabled != nil {
				d.SetField(item, "enabled", boolNode(*patch.Enabled))
			}
		}
		return nil
	})
}

// DeleteKeys removes keys from their files.
func (m *Manager) DeleteKeys(base map[string]string, ids []string) (*Snapshot, error) {
	return m.Edit(SourcePanel, base, func(tx *Tx) error {
		targets := map[string]map[string]bool{} // provider → secrets
		for _, id := range ids {
			k, ok := tx.Snapshot().KeyByID(id)
			if !ok {
				return fmt.Errorf("unknown key id %q", id)
			}
			if targets[k.Provider] == nil {
				targets[k.Provider] = map[string]bool{}
			}
			targets[k.Provider][k.Secret] = true
		}
		for provider, secrets := range targets {
			d, err := tx.Doc(KeysFile(provider))
			if err != nil {
				return err
			}
			seq := mapGet(d.Top(), "keys")
			if seq == nil || seq.Kind != yaml.SequenceNode {
				continue
			}
			for i := len(seq.Content) - 1; i >= 0; i-- {
				it := seq.Content[i]
				v := it.Value
				if it.Kind == yaml.MappingNode {
					if kn := mapGet(it, "key"); kn != nil {
						v = kn.Value
					}
				}
				if secrets[v] {
					d.DeleteItem(seq, i)
				}
			}
		}
		return nil
	})
}

// ---- egresses -----------------------------------------------------------

// ImportEgresses adds the new rows of an import plan as nodes (§7.9.1). With
// absorbInline, keys that wrote one of the new proxies inline are switched to
// the node name. It returns the full plan for display.
func (m *Manager) ImportEgresses(base map[string]string, text string, opt egress.ImportOptions, region string, tags []string, absorbInline bool) ([]egress.ImportRow, *Snapshot, error) {
	var rows []egress.ImportRow
	snap, err := m.Edit(SourcePanel, base, func(tx *Tx) error {
		s := tx.Snapshot()
		rows = egress.PlanImport(s.Egresses, text, s.EgressRefs(), opt)
		d, err := tx.Doc(FileEgresses)
		if err != nil {
			return err
		}
		top, err := d.ensureTop()
		if err != nil {
			return err
		}
		newByURL := map[string]string{}
		for _, r := range rows {
			if r.Status != egress.ImportNew && r.Status != egress.ImportAbsorbs {
				continue
			}
			node := mapNode("name", strNode(r.Name), "url", strNode(egress.WithPort(r.URL)))
			if region != "" {
				node.Content = append(node.Content, strNode("region"), strNode(region))
			}
			if len(tags) > 0 {
				node.Content = append(node.Content, strNode("tags"), flowList(tags))
			}
			d.AppendItem(top, "egresses", node)
			newByURL[r.URL] = r.Name
		}
		if !absorbInline {
			return nil
		}
		for _, k := range s.Keys {
			if !k.Egress.Inline || k.Egress.Name != "" {
				continue
			}
			if name, ok := newByURL[k.Egress.URL]; ok {
				kd, item, err := keyItem(tx, k)
				if err != nil {
					return err
				}
				kd.SetField(item, "egress", strNode(name))
			}
		}
		return nil
	})
	return rows, snap, err
}

// EgressPatch changes node fields; nil = unchanged.
type EgressPatch struct {
	URL          *string
	Enabled      *bool
	MaxKeys      *int
	Region, Note *string
	Tags         *[]string
}

func egressItem(tx *Tx, name string) (*Doc, *yaml.Node, int, error) {
	d, err := tx.Doc(FileEgresses)
	if err != nil {
		return nil, nil, 0, err
	}
	seq := mapGet(d.Top(), "egresses")
	if seq != nil && seq.Kind == yaml.SequenceNode {
		for i, it := range seq.Content {
			if n := mapGet(it, "name"); n != nil && strings.EqualFold(n.Value, name) {
				return d, it, i, nil
			}
		}
	}
	return nil, nil, 0, fmt.Errorf("egress %q not found", name)
}

// UpdateEgress edits a node. Keys referencing it by name follow automatically;
// keys that wrote the old URL inline are left as written (§7.9.2 rule 4).
func (m *Manager) UpdateEgress(base map[string]string, name string, patch EgressPatch) (*Snapshot, error) {
	return m.Edit(SourcePanel, base, func(tx *Tx) error {
		d, item, _, err := egressItem(tx, name)
		if err != nil {
			return err
		}
		if patch.URL != nil {
			c, err := egress.ParseImportLine(*patch.URL, "socks5")
			if err != nil || c.URL == "" {
				if err == nil {
					err = errors.New("empty URL")
				}
				return fmt.Errorf("url: %w", err)
			}
			d.SetField(item, "url", strNode(egress.WithPort(c.URL)))
		}
		if patch.Enabled != nil {
			d.SetField(item, "enabled", boolNode(*patch.Enabled))
		}
		if patch.MaxKeys != nil {
			d.SetField(item, "max_keys", intNode(int64(*patch.MaxKeys)))
		}
		for field, v := range map[string]*string{"region": patch.Region, "note": patch.Note} {
			if v == nil {
				continue
			}
			if *v == "" {
				d.DeleteField(item, field)
			} else {
				d.SetField(item, field, strNode(*v))
			}
		}
		if patch.Tags != nil {
			if len(*patch.Tags) == 0 {
				d.DeleteField(item, "tags")
			} else {
				d.SetField(item, "tags", flowList(*patch.Tags))
			}
		}
		return nil
	})
}

// replaceEgressRefs rewrites egress references in all key files (entries and defaults).
// match decides which raw values to replace; to is the new raw value.
func replaceEgressRefs(tx *Tx, match func(k Key) bool, matchDefault func(v string) bool, to string) error {
	for _, k := range tx.Snapshot().Keys {
		if !match(k) {
			continue
		}
		d, item, err := keyItem(tx, k)
		if err != nil {
			return err
		}
		if to == egress.Direct && findKey(item, "egress") < 0 {
			continue // inherits defaults; handled below
		}
		d.SetField(item, "egress", strNode(to))
	}
	for _, p := range Providers {
		d, err := tx.Doc(KeysFile(p))
		if err != nil {
			return err
		}
		defs := mapGet(d.Top(), "defaults")
		if v := mapGet(defs, "egress"); v != nil && v.Kind == yaml.ScalarNode && matchDefault(v.Value) {
			d.SetField(defs, "egress", strNode(to))
		}
	}
	return nil
}

// RenameEgress renames a node and every key reference to it, in one atomic edit.
func (m *Manager) RenameEgress(base map[string]string, oldName, newName string) (*Snapshot, error) {
	if err := egress.ValidateName(newName); err != nil {
		return nil, err
	}
	return m.Edit(SourcePanel, base, func(tx *Tx) error {
		if n, taken := tx.Snapshot().Egresses.Lookup(newName); taken && !strings.EqualFold(n.Name, oldName) {
			return fmt.Errorf("egress name %q is already used", newName)
		}
		d, item, _, err := egressItem(tx, oldName)
		if err != nil {
			return err
		}
		d.SetField(item, "name", strNode(newName))
		byName := func(v string) bool { return strings.EqualFold(strings.TrimSpace(v), oldName) }
		return replaceEgressRefs(tx, func(k Key) bool { return byName(k.EgressRef) }, byName, newName)
	})
}

// DeleteMode says what happens to keys that still use a node being deleted.
type DeleteMode int

const (
	DeleteRefuse   DeleteMode = iota // fail with *InUseError when referenced
	DeleteToDirect                   // switch those keys to direct
	DeleteToNode                     // switch them to another node
	DeleteToInline                   // write the node's URL into them (same proxy, unsaved)
)

// InUseError lists keys that still use an egress.
type InUseError struct {
	Egress string
	Keys   []string // display names
}

func (e *InUseError) Error() string {
	return fmt.Sprintf("egress %q is still used by %d key(s): %s", e.Egress, len(e.Keys), strings.Join(e.Keys, ", "))
}

// DeleteEgress removes a node. Keys using it (by name or by writing its URL)
// are handled per mode; nothing ever silently falls back to direct (§7.9.2 rule 4).
func (m *Manager) DeleteEgress(base map[string]string, name string, mode DeleteMode, target string) (*Snapshot, error) {
	return m.Edit(SourcePanel, base, func(tx *Tx) error {
		s := tx.Snapshot()
		node, ok := s.Egresses.Lookup(name)
		if !ok {
			return fmt.Errorf("egress %q not found", name)
		}
		id := egress.ID(node.URL)
		users := s.KeysUsingEgress(id)
		byName := func(v string) bool { return strings.EqualFold(strings.TrimSpace(v), node.Name) }
		defaultsUse := false
		for _, p := range Providers {
			d, err := tx.Doc(KeysFile(p))
			if err != nil {
				return err
			}
			if v := mapGet(mapGet(d.Top(), "defaults"), "egress"); v != nil && byName(v.Value) {
				defaultsUse = true
			}
		}
		if len(users) > 0 || defaultsUse {
			var to string
			switch mode {
			case DeleteRefuse:
				names := make([]string, 0, len(users))
				for _, k := range users {
					names = append(names, k.DisplayName())
				}
				if defaultsUse {
					names = append(names, "defaults.egress")
				}
				return &InUseError{Egress: node.Name, Keys: names}
			case DeleteToDirect:
				to = egress.Direct
			case DeleteToNode:
				t, ok := s.Egresses.Lookup(target)
				if !ok || strings.EqualFold(t.Name, node.Name) {
					return fmt.Errorf("target egress %q not found", target)
				}
				to = t.Name
			case DeleteToInline:
				to = egress.WithPort(node.URL)
			}
			match := func(k Key) bool {
				if mode == DeleteToInline {
					return byName(k.EgressRef) // inline users already carry the URL
				}
				return k.Egress.ID == id
			}
			if err := replaceEgressRefs(tx, match, byName, to); err != nil {
				return err
			}
		}
		d, _, idx, err := egressItem(tx, node.Name)
		if err != nil {
			return err
		}
		d.DeleteItem(mapGet(d.Top(), "egresses"), idx)
		return nil
	})
}

// ---- config -------------------------------------------------------------

var protectedConfig = []string{"admin.password", "admin.password_hash", "admin.totp_secret"}

// SetConfig sets a dotted config.yaml path ("logs.retention_days") to a
// string, bool, int, float64 or []string, creating missing sections.
func (m *Manager) SetConfig(base map[string]string, path string, value any) (*Snapshot, error) {
	return m.SetConfigs(base, []ConfigValue{{path, value}})
}

// ConfigValue is one config.yaml setting, by dotted path.
type ConfigValue struct {
	Path  string
	Value any // string, bool, int, int64, float64 or []string
}

// SetConfigs writes several settings in one edit: all are applied or none
// (the settings page saves a whole group at once).
func (m *Manager) SetConfigs(base map[string]string, vals []ConfigValue) (*Snapshot, error) {
	nodes := make([]*yaml.Node, len(vals))
	for i, v := range vals {
		if slices.Contains(protectedConfig, v.Path) {
			return nil, fmt.Errorf("%s cannot be set directly", v.Path)
		}
		vn, err := valueNode(v.Value)
		if err != nil {
			return nil, err
		}
		nodes[i] = vn
	}
	return m.Edit(SourcePanel, base, func(tx *Tx) error {
		d, err := tx.Doc(FileConfig)
		if err != nil {
			return err
		}
		for i, v := range vals {
			if err := setConfigPath(d, strings.Split(v.Path, "."), nodes[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

func setConfigPath(d *Doc, parts []string, vn *yaml.Node) error {
	cur, err := d.ensureTop()
	if err != nil {
		return err
	}
	for i, part := range parts[:len(parts)-1] {
		next := mapGet(cur, part)
		if next != nil && next.Kind == yaml.MappingNode {
			cur = next
			continue
		}
		// Build the missing tail as nested block mappings in one insert.
		leaf := vn
		for j := len(parts) - 1; j > i; j-- {
			leaf = mapNode(parts[j], leaf)
		}
		d.SetBlockField(cur, part, leaf)
		return nil
	}
	d.SetField(cur, parts[len(parts)-1], vn)
	return nil
}

func valueNode(v any) (*yaml.Node, error) {
	switch x := v.(type) {
	case string:
		return strNode(x), nil
	case bool:
		return boolNode(x), nil
	case int:
		return intNode(int64(x)), nil
	case int64:
		return intNode(x), nil
	case float64:
		return floatNode(x), nil
	case []string:
		return flowList(x), nil
	default:
		return nil, fmt.Errorf("unsupported config value type %T", v)
	}
}

// SetAdminPassword hashes plain into admin.password_hash. Callers should revoke
// sessions on the resulting change (the subscriber sees PasswordHash change).
func (m *Manager) SetAdminPassword(plain string) (*Snapshot, error) {
	hash, err := password.Hash(plain)
	if err != nil {
		return nil, err
	}
	return m.Edit(SourcePanel, nil, func(tx *Tx) error {
		d, err := tx.Doc(FileConfig)
		if err != nil {
			return err
		}
		return applyAdminHash(d, hash)
	})
}

// SetTOTPSecret stores (or with "" removes) admin.totp_secret.
func (m *Manager) SetTOTPSecret(secret string) (*Snapshot, error) {
	return m.Edit(SourcePanel, nil, func(tx *Tx) error {
		d, err := tx.Doc(FileConfig)
		if err != nil {
			return err
		}
		top, err := d.ensureTop()
		if err != nil {
			return err
		}
		admin := mapGet(top, "admin")
		if admin == nil || admin.Kind != yaml.MappingNode {
			return errors.New("config.yaml has no admin section")
		}
		if secret == "" {
			d.DeleteField(admin, "totp_secret")
		} else {
			d.SetField(admin, "totp_secret", strNode(secret))
		}
		return nil
	})
}
