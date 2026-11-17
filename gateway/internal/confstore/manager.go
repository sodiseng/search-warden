// Package confstore makes YAML files the single source of configuration and
// keeps them in two-way sync with the admin panel (ARCHITECTURE.md §14):
//
//   - Files are loaded, validated as a whole and published as an immutable Snapshot.
//   - Hand edits are picked up by a watcher; a broken file never replaces the
//     last good one, and its errors (with line numbers) are exposed via Status.
//   - Panel edits are written back surgically (comments and layout preserved),
//     with version checks against concurrent hand edits, backups and atomic renames.
//   - Plaintext secrets (admin.password, tokens[].token) are hashed in place.
package confstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/sodiseng/search-warden/gateway/examples"
	"github.com/sodiseng/search-warden/gateway/internal/auth/password"
	"github.com/sodiseng/search-warden/gateway/internal/auth/token"
)

// Change sources.
const (
	SourceFile   = "file"   // hand edit picked up from disk
	SourcePanel  = "panel"  // admin panel
	SourceSystem = "system" // automatic (secret hashing, bootstrap)
)

// Options tunes a Manager.
type Options struct {
	AdminPasswordEnv string        // WARDEN_ADMIN_PASSWORD; only used when config.yaml has no password yet
	Logger           *slog.Logger  // default: discard
	BackupKeep       int           // backups kept per file (default 20)
	PollInterval     time.Duration // hash polling fallback for missed fs events (default 5s)
	Debounce         time.Duration // delay after a file event before reloading (default 500ms)
	Now              func() time.Time
}

// Change describes an applied configuration change.
type Change struct {
	Source  string
	Files   []string
	Old     *Snapshot // nil for the initial load
	New     *Snapshot
	Summary []string // human-readable, secrets redacted
}

// FileStatus is the sync state of one file (shown in the panel's raw-file page).
type FileStatus struct {
	File        string    `json:"file"`
	Version     string    `json:"version"`      // applied version
	DiskVersion string    `json:"disk_version"` // version currently on disk
	Applied     bool      `json:"applied"`      // the disk content is what is in effect
	Errors      Errors    `json:"errors,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
}

// ErrConflict matches *ConflictError.
var ErrConflict = errors.New("confstore: conflict")

// ConflictError means a file changed since the caller loaded it.
type ConflictError struct {
	File   string
	Reason string
}

func (e *ConflictError) Error() string     { return e.File + ": " + e.Reason }
func (e *ConflictError) Is(err error) bool { return err == ErrConflict }

type fileState struct {
	applied   []byte
	version   string
	model     any
	diskVer   string
	errs      Errors
	checkedAt time.Time
}

// Manager owns the configuration files of one data directory. Safe for concurrent use.
type Manager struct {
	dir  string
	opts Options
	log  *slog.Logger

	mu    sync.Mutex // serializes reloads and writes
	files map[string]*fileState
	cur   atomic.Pointer[Snapshot]

	subMu   sync.Mutex
	subs    map[int]func(Change)
	nextSub int
}

func version(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func (m *Manager) path(name string) string { return filepath.Join(m.dir, filepath.FromSlash(name)) }

func (m *Manager) now() time.Time { return m.opts.Now() }

// Open creates missing files from templates, loads and validates everything.
// Any invalid file is a startup error listing file:line problems.
func Open(dir string, opts Options) (*Manager, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.BackupKeep <= 0 {
		opts.BackupKeep = 20
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 500 * time.Millisecond
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	m := &Manager{dir: dir, opts: opts, log: opts.Logger, files: map[string]*fileState{}, subs: map[int]func(Change){}}
	for _, sub := range []string{"keys", "backups"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("confstore: %w", err)
		}
	}
	for _, name := range AllFiles() {
		if _, err := os.Stat(m.path(name)); errors.Is(err, fs.ErrNotExist) {
			tpl, err := examples.FS.ReadFile(name)
			if err != nil {
				return nil, fmt.Errorf("confstore: template %s: %w", name, err)
			}
			if err := m.writeFile(name, tpl); err != nil {
				return nil, err
			}
			m.log.Info("created config file from template", "file", name)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	parsed := map[string]*parsed{}
	var all Errors
	for _, name := range AllFiles() {
		data, err := os.ReadFile(m.path(name))
		if err != nil {
			return nil, fmt.Errorf("confstore: %w", err)
		}
		p := m.loadAndRewrite(name, data, true)
		parsed[name] = p
		all = append(all, p.errs...)
	}
	if len(all) > 0 {
		return nil, all.sorted()
	}
	snap, errs := assemble(m.models(parsed), versionsOf(parsed, nil))
	if errs != nil {
		return nil, errs.sorted()
	}
	now := m.now()
	for name, p := range parsed {
		m.files[name] = &fileState{applied: p.data, version: p.version, model: p.model, diskVer: p.version, checkedAt: now}
	}
	m.cur.Store(snap)
	if opts.AdminPasswordEnv != "" && !parsed[FileConfig].fromEnv {
		m.log.Info("WARDEN_ADMIN_PASSWORD is ignored because config.yaml already has an admin password; you may remove it from the environment")
	}
	return m, nil
}

// parsed is one file's content run through the YAML parser and schema.
type parsed struct {
	name    string
	data    []byte
	version string
	doc     *Doc
	model   any
	errs    Errors
	fromEnv bool // admin password bootstrapped from WARDEN_ADMIN_PASSWORD
}

func parseFile(name string, data []byte) *parsed {
	p := &parsed{name: name, data: data, version: version(data)}
	doc, err := ParseDoc(name, data)
	if err != nil {
		p.errs = err.(Errors)
		return p
	}
	p.doc = doc
	switch name {
	case FileConfig:
		p.model, p.errs = parseConfig(name, doc)
	case FileEgresses:
		p.model, p.errs = parseEgresses(name, doc)
	case FileTokens:
		p.model, p.errs = parseTokens(name, doc)
	default:
		provider := strings.TrimSuffix(strings.TrimPrefix(name, "keys/"), ".yaml")
		p.model, p.errs = parseKeys(name, provider, doc)
	}
	return p
}

// loadAndRewrite parses a file, hashes plaintext secrets in place (writing the
// file without a backup, so no copy of the plaintext survives), and on startup
// bootstraps the admin password from the environment.
func (m *Manager) loadAndRewrite(name string, data []byte, startup bool) *parsed {
	p := parseFile(name, data)
	if out, changed, err := hashSecrets(p); err != nil {
		m.log.Error("could not hash plaintext secret", "file", name, "err", err)
	} else if changed {
		if err := m.writeFile(name, out); err != nil {
			m.log.Error("could not rewrite plaintext secret; it is still on disk", "file", name, "err", err)
		} else {
			m.log.Info("hashed plaintext secret in place", "file", name)
		}
		p = parseFile(name, out)
	}
	if name != FileConfig || len(p.errs) > 0 {
		return p
	}
	cfg := p.model.(*Config)
	if cfg.Admin.PasswordHash != "" {
		return p
	}
	if startup && m.opts.AdminPasswordEnv != "" {
		out, err := setAdminHash(p.doc, m.opts.AdminPasswordEnv)
		if err == nil {
			err = m.writeFile(name, out)
		}
		if err != nil {
			p.errs = append(p.errs, FileError{File: name, Msg: "WARDEN_ADMIN_PASSWORD: " + err.Error()})
			return p
		}
		m.log.Info("admin password initialized from WARDEN_ADMIN_PASSWORD")
		p = parseFile(name, out)
		p.fromEnv = true
		return p
	}
	p.errs = append(p.errs, FileError{File: name, Msg: "admin password is not set: write `password: <at least 12 characters>` under admin in " + name + " (it is hashed automatically), or set WARDEN_ADMIN_PASSWORD on first start"})
	return p
}

// hashSecrets rewrites admin.password → password_hash and tokens[].token → token_hash/token_prefix.
func hashSecrets(p *parsed) ([]byte, bool, error) {
	if p.doc == nil || p.model == nil {
		return nil, false, nil
	}
	switch p.name {
	case FileConfig:
		cfg := p.model.(*Config)
		if cfg.Admin.plainPassword == "" || password.Validate(cfg.Admin.plainPassword) != nil {
			return nil, false, nil
		}
		out, err := setAdminHash(p.doc, cfg.Admin.plainPassword)
		return out, err == nil, err
	case FileTokens:
		seq := mapGet(p.doc.Top(), "tokens")
		if seq == nil || seq.Kind != yaml.SequenceNode {
			return nil, false, nil
		}
		for _, item := range seq.Content {
			tn := mapGet(item, "token")
			if tn == nil || tn.Kind != yaml.ScalarNode || isNull(tn) || token.Validate(tn.Value) != nil {
				continue
			}
			hash, prefix := token.Hash(tn.Value), token.DisplayPrefix(tn.Value)
			p.doc.DeleteField(item, "token_hash")
			p.doc.RenameField(item, "token", "token_hash", strNode(hash))
			p.doc.SetField(item, "token_prefix", strNode(prefix))
		}
		if !p.doc.Changed() {
			return nil, false, nil
		}
		out, err := p.doc.Bytes()
		return out, err == nil, err
	}
	return nil, false, nil
}

// setAdminHash hashes plain and stores it as admin.password_hash, removing admin.password.
func setAdminHash(d *Doc, plain string) ([]byte, error) {
	hash, err := password.Hash(plain)
	if err != nil {
		return nil, err
	}
	if err := applyAdminHash(d, hash); err != nil {
		return nil, err
	}
	return d.Bytes()
}

func applyAdminHash(d *Doc, hash string) error {
	top, err := d.ensureTop()
	if err != nil {
		return err
	}
	admin := mapGet(top, "admin")
	switch {
	case admin == nil || isNull(admin):
		d.SetBlockField(top, "admin", mapNode("password_hash", strNode(hash)))
	case admin.Kind != yaml.MappingNode:
		return errors.New("admin must be a mapping")
	case findKey(admin, "password") >= 0:
		d.DeleteField(admin, "password_hash")
		d.RenameField(admin, "password", "password_hash", strNode(hash))
	default:
		d.SetField(admin, "password_hash", strNode(hash))
	}
	return nil
}

func (m *Manager) models(use map[string]*parsed) models {
	get := func(name string) any {
		if p, ok := use[name]; ok {
			return p.model
		}
		return m.files[name].model
	}
	md := models{cfg: get(FileConfig).(*Config), eg: get(FileEgresses).(*EgressFile), toks: get(FileTokens).(*TokenFile), keys: map[string]*KeyFile{}}
	for _, p := range Providers {
		md.keys[p] = get(KeysFile(p)).(*KeyFile)
	}
	return md
}

func versionsOf(use map[string]*parsed, files map[string]*fileState) map[string]string {
	v := map[string]string{}
	for name, st := range files {
		v[name] = st.version
	}
	for name, p := range use {
		v[name] = p.version
	}
	return v
}

// Snapshot returns the configuration in effect. Never nil after Open.
func (m *Manager) Snapshot() *Snapshot { return m.cur.Load() }

// Status reports every file's sync state.
func (m *Manager) Status() []FileStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]FileStatus, 0, len(m.files))
	for _, name := range AllFiles() {
		st := m.files[name]
		out = append(out, FileStatus{File: name, Version: st.version, DiskVersion: st.diskVer,
			Applied: st.diskVer == st.version, Errors: st.errs, CheckedAt: st.checkedAt})
	}
	return out
}

// Healthy reports whether every file on disk is the one in effect.
func (m *Manager) Healthy() bool {
	for _, s := range m.Status() {
		if !s.Applied {
			return false
		}
	}
	return true
}

// Subscribe registers fn for applied changes; it runs after the change is
// visible via Snapshot. The returned func unsubscribes.
func (m *Manager) Subscribe(fn func(Change)) (cancel func()) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	id := m.nextSub
	m.nextSub++
	m.subs[id] = fn
	return func() {
		m.subMu.Lock()
		delete(m.subs, id)
		m.subMu.Unlock()
	}
}

func (m *Manager) notify(changes ...*Change) {
	for _, c := range changes {
		if c == nil {
			continue
		}
		m.subMu.Lock()
		ids := make([]int, 0, len(m.subs))
		for id := range m.subs {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		fns := make([]func(Change), 0, len(ids))
		for _, id := range ids {
			fns = append(fns, m.subs[id])
		}
		m.subMu.Unlock()
		for _, fn := range fns {
			func() {
				defer func() {
					if v := recover(); v != nil {
						m.log.Error("config subscriber panicked", "panic", v)
					}
				}()
				fn(*c)
			}()
		}
	}
}

// Reload picks up hand edits. It is called by Watch; tests call it directly.
func (m *Manager) Reload() {
	m.mu.Lock()
	c := m.reloadLocked(SourceFile)
	m.mu.Unlock()
	m.notify(c)
}

func (m *Manager) reloadLocked(source string) *Change {
	now := m.now()
	use := map[string]*parsed{}
	rejected := map[string]Errors{}

	// Pass 1: read everything and see whether anything is new since the last check.
	disk := map[string][]byte{}
	fresh := false
	for _, name := range AllFiles() {
		st := m.files[name]
		st.checkedAt = now
		data, err := os.ReadFile(m.path(name))
		if err != nil {
			state, msg := "missing", "file is missing; the last applied version stays in effect (restore the file, or restart to recreate it from the template)"
			if !errors.Is(err, fs.ErrNotExist) {
				// e.g. replaced by a root-owned copy (scp, sudo cp): say what to fix.
				state = "unreadable"
				msg = fmt.Sprintf("file cannot be read (%v); the last applied version stays in effect. Make it readable and writable for uid %d, e.g. chown it back", errors.Unwrap(err), os.Getuid())
				if errors.Unwrap(err) == nil {
					msg = "file cannot be read; the last applied version stays in effect"
				}
			}
			if st.diskVer != state {
				st.diskVer = state
				st.errs = Errors{{File: name, Msg: msg}}
				m.log.Warn("config file "+state, "file", name)
			}
			continue
		}
		v := version(data)
		switch {
		case v == st.version:
			st.diskVer, st.errs = v, nil // in sync (also clears errors after a revert)
		case v != st.diskVer:
			fresh = true
			disk[name] = data
		default:
			disk[name] = data // previously rejected; re-evaluated together with fresh edits
		}
	}
	if !fresh {
		return nil
	}

	// Pass 2: parse every file that differs from what is applied. Previously
	// rejected files are included: a rename split across two files only becomes
	// consistent once both are saved.
	for _, name := range AllFiles() {
		data, ok := disk[name]
		if !ok {
			continue
		}
		st := m.files[name]
		p := m.loadAndRewrite(name, data, false)
		st.diskVer = p.version
		if len(p.errs) > 0 {
			rejected[name] = p.errs
			continue
		}
		if p.version == st.version { // the secret rewrite turned it back into the applied content
			st.errs = nil
			continue
		}
		use[name] = p
	}

	// Drop new file versions until the cross-file view is consistent. The applied
	// versions are always consistent, so this terminates.
	var snap *Snapshot
	for {
		s, errs := assemble(m.models(use), versionsOf(use, m.files))
		if errs == nil {
			snap = s
			break
		}
		progress := false
		for _, e := range errs {
			if _, isNew := use[e.File]; isNew {
				delete(use, e.File)
				rejected[e.File] = append(rejected[e.File], e)
				progress = true
			} else if _, egNew := use[FileEgresses]; egNew {
				delete(use, FileEgresses)
				rejected[FileEgresses] = append(rejected[FileEgresses], FileError{File: FileEgresses,
					Msg: "not applied because it breaks a reference: " + e.Error()})
				progress = true
			}
		}
		if !progress {
			for name := range use {
				rejected[name] = append(rejected[name], errs...)
			}
			use = map[string]*parsed{}
		}
	}

	for name, errs := range rejected {
		st := m.files[name]
		errs = errs.sorted()
		if st.errs.Error() != errs.Error() { // log only what is new, not every re-evaluation
			for _, e := range errs {
				m.log.Warn("config change rejected", "file", name, "line", e.Line, "error", e.Msg)
			}
		}
		st.errs = errs
	}
	if len(use) == 0 {
		return nil
	}
	files := make([]string, 0, len(use))
	for name, p := range use {
		st := m.files[name]
		st.applied, st.version, st.model, st.diskVer, st.errs = p.data, p.version, p.model, p.version, nil
		files = append(files, name)
	}
	sort.Strings(files)
	old := m.cur.Load()
	m.cur.Store(snap)
	c := &Change{Source: source, Files: files, Old: old, New: snap, Summary: summarize(old, snap, files)}
	m.log.Info("config applied", "source", source, "files", files, "summary", c.Summary)
	return c
}

// ---- writes -------------------------------------------------------------

// ReadRaw returns a file's current disk content and version (for the raw editor).
func (m *Manager) ReadRaw(name string) ([]byte, string, error) {
	if !isManaged(name) {
		return nil, "", fmt.Errorf("confstore: unknown file %q", name)
	}
	data, err := os.ReadFile(m.path(name))
	if err != nil {
		return nil, "", err
	}
	return data, version(data), nil
}

func isManaged(name string) bool {
	for _, f := range AllFiles() {
		if f == name {
			return true
		}
	}
	return false
}

// Tx is a panel edit across one or more files.
type Tx struct {
	m    *Manager
	snap *Snapshot
	base map[string]string
	docs map[string]*Doc
	raw  map[string][]byte
}

// Snapshot is the configuration the edit starts from.
func (tx *Tx) Snapshot() *Snapshot { return tx.snap }

// Doc opens a file for surgical editing. It fails with *ConflictError when the
// file on disk is not the applied version or differs from the caller's base version.
func (tx *Tx) Doc(name string) (*Doc, error) {
	if d, ok := tx.docs[name]; ok {
		return d, nil
	}
	data, err := tx.current(name)
	if err != nil {
		return nil, err
	}
	d, err := ParseDoc(name, data)
	if err != nil {
		return nil, err
	}
	tx.docs[name] = d
	return d, nil
}

// SetRaw replaces a whole file (raw editor).
func (tx *Tx) SetRaw(name string, data []byte) error {
	if _, err := tx.current(name); err != nil {
		return err
	}
	tx.raw[name] = data
	return nil
}

func (tx *Tx) current(name string) ([]byte, error) {
	if !isManaged(name) {
		return nil, fmt.Errorf("confstore: unknown file %q", name)
	}
	data, err := os.ReadFile(tx.m.path(name))
	if err != nil {
		return nil, err
	}
	v := version(data)
	if v != tx.m.files[name].version {
		return nil, &ConflictError{File: name, Reason: "the file on disk has changes that are not in effect (fix them in the raw editor first)"}
	}
	if b, ok := tx.base[name]; ok && b != "" && b != v {
		return nil, &ConflictError{File: name, Reason: "the file was modified after you opened it; reload and try again"}
	}
	return data, nil
}

// Edit runs fn and, if it changed anything, validates the result as a whole,
// backs up and atomically rewrites the touched files, and applies them.
// base maps file → version the caller saw (may be nil). Validation failures
// return Errors; concurrent modifications return *ConflictError.
func (m *Manager) Edit(source string, base map[string]string, fn func(tx *Tx) error) (*Snapshot, error) {
	m.mu.Lock()
	pre := m.reloadLocked(SourceFile)
	snap, c, err := m.editLocked(source, base, fn)
	m.mu.Unlock()
	m.notify(pre, c)
	return snap, err
}

func (m *Manager) editLocked(source string, base map[string]string, fn func(tx *Tx) error) (*Snapshot, *Change, error) {
	tx := &Tx{m: m, snap: m.cur.Load(), base: base, docs: map[string]*Doc{}, raw: map[string][]byte{}}
	if err := fn(tx); err != nil {
		return nil, nil, err
	}
	out := map[string][]byte{}
	for name, data := range tx.raw {
		out[name] = data
	}
	for name, d := range tx.docs {
		if _, isRaw := tx.raw[name]; isRaw || !d.Changed() {
			continue
		}
		b, err := d.Bytes()
		if err != nil {
			return nil, nil, err
		}
		out[name] = b
	}
	if len(out) == 0 {
		return tx.snap, nil, nil
	}

	use := map[string]*parsed{}
	var verrs Errors
	for name, data := range out {
		p := parseFile(name, data)
		if o, changed, err := hashSecrets(p); err != nil {
			return nil, nil, err
		} else if changed {
			out[name] = o
			p = parseFile(name, o)
		}
		if name == FileConfig && len(p.errs) == 0 && p.model.(*Config).Admin.PasswordHash == "" {
			p.errs = append(p.errs, FileError{File: name, Msg: "admin.password_hash is required"})
		}
		verrs = append(verrs, p.errs...)
		use[name] = p
	}
	if len(verrs) > 0 {
		return nil, nil, verrs.sorted()
	}
	snap, errs := assemble(m.models(use), versionsOf(use, m.files))
	if errs != nil {
		return nil, nil, errs.sorted()
	}

	var written []string
	for _, name := range AllFiles() {
		data, ok := out[name]
		if !ok || version(data) == m.files[name].version {
			continue
		}
		if err := m.backup(name, m.files[name].applied); err != nil {
			m.log.Warn("backup failed", "file", name, "err", err)
		}
		if err := m.writeFile(name, data); err != nil {
			c := m.reloadLocked(SourceFile) // reconcile with whatever made it to disk
			return nil, c, fmt.Errorf("confstore: write %s: %w", name, err)
		}
		written = append(written, name)
	}
	if len(written) == 0 {
		return tx.snap, nil, nil
	}
	now := m.now()
	for _, name := range written {
		p := use[name]
		m.files[name] = &fileState{applied: p.data, version: p.version, model: p.model, diskVer: p.version, checkedAt: now}
	}
	old := m.cur.Load()
	m.cur.Store(snap)
	c := &Change{Source: source, Files: written, Old: old, New: snap, Summary: summarize(old, snap, written)}
	m.log.Info("config applied", "source", source, "files", written, "summary", c.Summary)
	return snap, c, nil
}

// SaveRaw replaces a file from the raw editor after validating it.
func (m *Manager) SaveRaw(name, baseVersion string, data []byte) (*Snapshot, error) {
	return m.Edit(SourcePanel, map[string]string{name: baseVersion}, func(tx *Tx) error {
		return tx.SetRaw(name, data)
	})
}

// writeFile atomically replaces name with data (mode 0600).
func (m *Manager) writeFile(name string, data []byte) error {
	target := m.path(name)
	dir := filepath.Dir(target)
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(dir, "."+filepath.Base(target)+".tmp-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	if runtime.GOOS != "windows" {
		if d, err := os.Open(dir); err == nil {
			_ = d.Sync()
			d.Close()
		}
	}
	return nil
}

// backup stores the previous content in backups/, keeping the newest BackupKeep per file.
func (m *Manager) backup(name string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	flat := strings.ReplaceAll(name, "/", "__")
	bdir := filepath.Join(m.dir, "backups")
	stamp := m.now().Format("20060102-150405.000000")
	if err := os.WriteFile(filepath.Join(bdir, flat+"."+stamp+".bak"), data, 0o600); err != nil {
		return err
	}
	entries, err := os.ReadDir(bdir)
	if err != nil {
		return err
	}
	var mine []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), flat+".") && strings.HasSuffix(e.Name(), ".bak") {
			mine = append(mine, e.Name())
		}
	}
	sort.Strings(mine)
	for len(mine) > m.opts.BackupKeep {
		_ = os.Remove(filepath.Join(bdir, mine[0]))
		mine = mine[1:]
	}
	return nil
}
