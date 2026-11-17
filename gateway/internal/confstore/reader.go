package confstore

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
)

// FileError is one problem in a config file, with a 1-based line when known.
type FileError struct {
	File string `json:"file"`
	Line int    `json:"line,omitempty"`
	Msg  string `json:"msg"`
}

func (e FileError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Msg)
	}
	return e.File + ": " + e.Msg
}

// Errors is a list of FileError that is itself an error.
type Errors []FileError

func (es Errors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "\n")
}

func (es Errors) sorted() Errors {
	out := append(Errors(nil), es...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// reader turns yaml nodes into typed values, collecting positioned errors.
type reader struct {
	file string
	errs Errors
}

func (r *reader) addf(n *yaml.Node, format string, args ...any) {
	line := 0
	if n != nil {
		line = n.Line
	}
	r.errs = append(r.errs, FileError{File: r.file, Line: line, Msg: fmt.Sprintf(format, args...)})
}

// fields reads one mapping. A missing or null node reads as an empty mapping (all defaults).
type fields struct {
	r    *reader
	m    *yaml.Node
	path string
	used map[string]bool
}

func (r *reader) fieldsOf(n *yaml.Node, path string) *fields {
	f := &fields{r: r, path: path, used: map[string]bool{}}
	switch {
	case isNull(n):
	case n.Kind == yaml.MappingNode:
		f.m = n
		seen := map[string]int{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if prev, dup := seen[k.Value]; dup {
				r.addf(k, "duplicate field %q (first defined on line %d)", f.p(k.Value), prev)
			}
			seen[k.Value] = k.Line
		}
	default:
		where := path
		if where == "" {
			where = "top level"
		}
		r.addf(n, "%s must be a mapping", where)
	}
	return f
}

func (f *fields) p(key string) string {
	if f.path == "" {
		return key
	}
	return f.path + "." + key
}

func (f *fields) get(key string) *yaml.Node {
	f.used[key] = true
	return mapGet(f.m, key)
}

// has reports whether key is present with a non-null value.
func (f *fields) has(key string) bool {
	return !isNull(mapGet(f.m, key))
}

func (f *fields) scalar(key string) *yaml.Node {
	n := f.get(key)
	if isNull(n) {
		return nil
	}
	if n.Kind != yaml.ScalarNode {
		f.r.addf(n, "%s must be a single value", f.p(key))
		return nil
	}
	return n
}

func (f *fields) str(key, def string) string {
	n := f.scalar(key)
	if n == nil {
		return def
	}
	return n.Value
}

func (f *fields) boolean(key string, def bool) bool {
	n := f.scalar(key)
	if n == nil {
		return def
	}
	if n.Tag != "!!bool" {
		f.r.addf(n, "%s must be true or false, got %q", f.p(key), n.Value)
		return def
	}
	b, _ := strconv.ParseBool(strings.ToLower(n.Value))
	return b
}

func (f *fields) integer(key string, def, min, max int) int {
	n := f.scalar(key)
	if n == nil {
		return def
	}
	v, err := strconv.ParseInt(strings.ReplaceAll(n.Value, "_", ""), 0, 64)
	if n.Tag != "!!int" || err != nil {
		f.r.addf(n, "%s must be a whole number, got %q", f.p(key), n.Value)
		return def
	}
	if v < int64(min) || v > int64(max) {
		f.r.addf(n, "%s must be between %d and %d, got %d", f.p(key), min, max, v)
		return def
	}
	return int(v)
}

func (f *fields) number(key string, def, min, max float64) float64 {
	n := f.scalar(key)
	if n == nil {
		return def
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64)
	if (n.Tag != "!!int" && n.Tag != "!!float") || err != nil {
		f.r.addf(n, "%s must be a number, got %q", f.p(key), n.Value)
		return def
	}
	if v < min || v > max {
		f.r.addf(n, "%s must be between %g and %g, got %g", f.p(key), min, max, v)
		return def
	}
	return v
}

// strList accepts a sequence of scalars or a single scalar. ok=false when absent.
func (f *fields) strList(key string) (vals []string, ok bool) {
	n := f.get(key)
	switch {
	case isNull(n):
		return nil, false
	case n.Kind == yaml.ScalarNode:
		return []string{n.Value}, true
	case n.Kind == yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				f.r.addf(c, "%s must be a list of values", f.p(key))
				continue
			}
			out = append(out, c.Value)
		}
		return out, true
	default:
		f.r.addf(n, "%s must be a list", f.p(key))
		return nil, false
	}
}

// seq returns the items of a sequence field (null/absent → empty).
func (f *fields) seq(key string) []*yaml.Node {
	n := f.get(key)
	switch {
	case isNull(n):
		return nil
	case n.Kind == yaml.SequenceNode:
		return n.Content
	default:
		f.r.addf(n, "%s must be a list", f.p(key))
		return nil
	}
}

func (f *fields) sub(key string) *fields {
	return f.r.fieldsOf(f.get(key), f.p(key))
}

// done reports unknown fields, suggesting the closest known name.
func (f *fields) done() {
	if f.m == nil {
		return
	}
	known := make([]string, 0, len(f.used))
	for k := range f.used {
		known = append(known, k)
	}
	sort.Strings(known)
	for i := 0; i+1 < len(f.m.Content); i += 2 {
		k := f.m.Content[i]
		if f.used[k.Value] {
			continue
		}
		msg := fmt.Sprintf("unknown field %q", f.p(k.Value))
		if s := closest(k.Value, known); s != "" {
			msg += fmt.Sprintf(" (did you mean %q?)", s)
		}
		f.r.errs = append(f.r.errs, FileError{File: f.r.file, Line: k.Line, Msg: msg})
	}
}

func closest(s string, options []string) string {
	best, bestD := "", 3 // only suggest close matches
	for _, o := range options {
		if d := levenshtein(strings.ToLower(s), strings.ToLower(o)); d < bestD {
			best, bestD = o, d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
