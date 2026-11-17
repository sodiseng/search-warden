package confstore

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v4"
)

// Doc is a YAML document edited surgically: bytes that an edit does not touch
// (comments, blank lines, quoting, indentation) are preserved exactly.
//
// Every edit mutates the node tree AND records a text splice against the
// original source. Bytes() applies the splices, re-parses the result and
// checks it is structurally identical to the mutated tree. If anything does
// not line up (exotic formatting, multi-line scalars, anchors...), it falls
// back to re-encoding the tree, which keeps comments but may drop blank lines.
type Doc struct {
	name     string
	src      []byte
	root     *yaml.Node // DocumentNode
	lineOff  []int      // byte offset of the start of each line; index 0 = line 1
	nl       string     // newline style of the source
	splices  []splice
	order    int
	reencode bool
	changed  bool
}

type splice struct {
	start, end int
	text       string
	order      int
}

var lineRe = regexp.MustCompile(`line (\d+)`)

// ParseDoc parses src. YAML syntax errors are returned as Errors with a line number.
func ParseDoc(name string, src []byte) (*Doc, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		return nil, Errors{yamlSyntaxError(name, err)}
	}
	d := &Doc{name: name, src: src, root: &root, nl: "\n"}
	if bytes.Contains(src, []byte("\r\n")) {
		d.nl = "\r\n"
	}
	d.lineOff = []int{0}
	for i, c := range src {
		if c == '\n' {
			d.lineOff = append(d.lineOff, i+1)
		}
	}
	if root.Kind == 0 { // empty or comment-only file
		d.root = &yaml.Node{Kind: yaml.DocumentNode}
	}
	if d.root.Kind != yaml.DocumentNode {
		return nil, Errors{{File: name, Msg: "expected a single YAML document"}}
	}
	return d, nil
}

func yamlSyntaxError(name string, err error) FileError {
	msg := err.Error()
	line := 0
	if m := lineRe.FindStringSubmatch(msg); m != nil {
		line, _ = strconv.Atoi(m[1])
	}
	msg = strings.TrimPrefix(msg, "yaml: ")
	if line > 0 {
		msg = strings.TrimSpace(lineRe.ReplaceAllString(msg, ""))
		msg = strings.TrimLeft(msg, ": ")
	}
	return FileError{File: name, Line: line, Msg: "YAML syntax error: " + msg}
}

// Top returns the top-level mapping, or nil if the document is empty or not a mapping.
func (d *Doc) Top() *yaml.Node {
	if len(d.root.Content) == 0 {
		return nil
	}
	if n := d.root.Content[0]; n.Kind == yaml.MappingNode {
		return n
	}
	return nil
}

// ensureTop returns the top-level mapping, creating it (and forcing a re-encode) if absent.
func (d *Doc) ensureTop() (*yaml.Node, error) {
	if t := d.Top(); t != nil {
		return t, nil
	}
	if len(d.root.Content) > 0 && !isNull(d.root.Content[0]) {
		return nil, fmt.Errorf("%s: top level is not a mapping", d.name)
	}
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	d.root.Content = []*yaml.Node{m}
	d.reencode, d.changed = true, true
	return m, nil
}

// Changed reports whether any edit was made.
func (d *Doc) Changed() bool { return d.changed }

// ---- node helpers -------------------------------------------------------

func isNull(n *yaml.Node) bool {
	return n == nil || (n.Kind == yaml.ScalarNode && n.Tag == "!!null")
}

func findKey(m *yaml.Node, key string) int {
	if m == nil || m.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// mapGet returns m[key] or nil.
func mapGet(m *yaml.Node, key string) *yaml.Node {
	if i := findKey(m, key); i >= 0 {
		return m.Content[i+1]
	}
	return nil
}

// Scalar constructors set the tag the parser would assign, so verification can compare trees.
func strNode(s string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
	if strings.ContainsAny(s, "\n\r") {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

func intNode(v int64) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(v, 10)}
}

func floatNode(v float64) *yaml.Node {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return intNode(int64(v))
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(v, 'f', -1, 64)}
}

func boolNode(v bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(v)}
}

// flowList renders as [a, b] so it fits on one line.
func flowList(items []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, s := range items {
		n.Content = append(n.Content, strNode(s))
	}
	return n
}

// mapNode builds a block mapping from alternating key/value pairs.
func mapNode(kv ...any) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(kv); i += 2 {
		n.Content = append(n.Content, strNode(kv[i].(string)), kv[i+1].(*yaml.Node))
	}
	return n
}

// ---- position helpers ---------------------------------------------------

func (d *Doc) offset(line, col int) (int, bool) {
	if line < 1 || line > len(d.lineOff) || col < 1 {
		return 0, false
	}
	i := d.lineOff[line-1]
	for c := 1; c < col; c++ { // columns count characters, not bytes
		if i >= len(d.src) || d.src[i] == '\n' {
			return 0, false
		}
		_, w := utf8.DecodeRune(d.src[i:])
		i += w
	}
	return i, true
}

// lineEnd is the offset of the line terminator of line (or len(src)).
func (d *Doc) lineEnd(line int) int {
	i := d.lineOff[line-1]
	for i < len(d.src) && d.src[i] != '\n' {
		i++
	}
	if i > 0 && i <= len(d.src) && i-1 >= d.lineOff[line-1] && d.src[i-1] == '\r' {
		i--
	}
	return i
}

// lineSpan covers whole lines a..b including the terminator of b.
func (d *Doc) lineSpan(a, b int) (int, int) {
	start := d.lineOff[a-1]
	end := len(d.src)
	if b < len(d.lineOff) {
		end = d.lineOff[b]
	}
	return start, end
}

func (d *Doc) onlySpacesBefore(n *yaml.Node) bool {
	off, ok := d.offset(n.Line, n.Column)
	if !ok {
		return false
	}
	for _, c := range d.src[d.lineOff[n.Line-1]:off] {
		if c != ' ' {
			return false
		}
	}
	return true
}

// scalarSpan returns the byte span of a single-line scalar token.
func (d *Doc) scalarSpan(n *yaml.Node, inFlow bool) (int, int, bool) {
	if n == nil || n.Kind != yaml.ScalarNode || n.Line == 0 || n.Anchor != "" ||
		n.Style&(yaml.LiteralStyle|yaml.FoldedStyle|yaml.TaggedStyle) != 0 {
		return 0, 0, false
	}
	start, ok := d.offset(n.Line, n.Column)
	if !ok {
		return 0, 0, false
	}
	src := d.src
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		if start >= len(src) || src[start] != '"' {
			return 0, 0, false
		}
		for i := start + 1; i < len(src); i++ {
			switch src[i] {
			case '\\':
				i++
			case '"':
				return start, i + 1, true
			case '\n', '\r':
				return 0, 0, false
			}
		}
	case n.Style&yaml.SingleQuotedStyle != 0:
		if start >= len(src) || src[start] != '\'' {
			return 0, 0, false
		}
		for i := start + 1; i < len(src); i++ {
			switch src[i] {
			case '\'':
				if i+1 < len(src) && src[i+1] == '\'' {
					i++
					continue
				}
				return start, i + 1, true
			case '\n', '\r':
				return 0, 0, false
			}
		}
	default: // plain
		if strings.ContainsAny(n.Value, "\n\r") {
			return 0, 0, false
		}
		end := start
		for end < len(src) {
			c := src[end]
			if c == '\n' || c == '\r' {
				break
			}
			if c == '#' && (end == start || src[end-1] == ' ' || src[end-1] == '\t') {
				break
			}
			if c == ':' && (end+1 >= len(src) || src[end+1] == ' ' || src[end+1] == '\t' || src[end+1] == '\n' || src[end+1] == '\r') {
				break // "key: value" — a plain scalar cannot contain ": "
			}
			if inFlow && (c == ',' || c == ']' || c == '}') {
				break
			}
			end++
		}
		for end > start && (src[end-1] == ' ' || src[end-1] == '\t') {
			end--
		}
		return start, end, true
	}
	return 0, 0, false
}

// endLine is the last line holding content of n (comments excluded).
func (d *Doc) endLine(n *yaml.Node) (int, bool) {
	if n == nil || n.Line == 0 {
		return 0, false
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			return 0, false
		}
		if _, _, ok := d.scalarSpan(n, false); !ok && n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
			return 0, false
		}
		return n.Line, true
	case yaml.AliasNode:
		return n.Line, true
	case yaml.MappingNode, yaml.SequenceNode:
		if n.Style&yaml.FlowStyle != 0 {
			if !allOnLine(n, n.Line) {
				return 0, false
			}
			return n.Line, true
		}
		if len(n.Content) == 0 {
			return n.Line, true
		}
		return d.endLine(n.Content[len(n.Content)-1])
	}
	return 0, false
}

func allOnLine(n *yaml.Node, line int) bool {
	if n.Line != line {
		return false
	}
	for _, c := range n.Content {
		if !allOnLine(c, line) {
			return false
		}
	}
	return true
}

// flowSpan returns the span of a single-line flow collection ("[...]" / "{...}").
func (d *Doc) flowSpan(n *yaml.Node) (int, int, bool) {
	if n.Style&yaml.FlowStyle == 0 || n.Line == 0 || !allOnLine(n, n.Line) {
		return 0, 0, false
	}
	start, ok := d.offset(n.Line, n.Column)
	if !ok || start >= len(d.src) {
		return 0, 0, false
	}
	open := d.src[start]
	closeCh := byte(']')
	if open == '{' {
		closeCh = '}'
	} else if open != '[' {
		return 0, 0, false
	}
	depth, quote := 0, byte(0)
	for i := start; i < len(d.src) && d.src[i] != '\n'; i++ {
		c := d.src[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
			if depth == 0 {
				if c != closeCh {
					return 0, 0, false
				}
				return start, i + 1, true
			}
		}
	}
	return 0, 0, false
}

// valueSpan locates a replaceable single-line value (scalar or flow collection).
func (d *Doc) valueSpan(v *yaml.Node, inFlow bool) (int, int, bool) {
	if v.Kind == yaml.ScalarNode {
		return d.scalarSpan(v, inFlow)
	}
	return d.flowSpan(v)
}

// ---- rendering ----------------------------------------------------------

// cloneNode deep-copies n: yaml.Dump rewrites tags of the nodes it encodes,
// which would corrupt the tree used for verification.
func cloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, ch := range n.Content {
		c.Content[i] = cloneNode(ch)
	}
	return &c
}

func dumpNode(n *yaml.Node) (string, error) {
	out, err := yaml.Dump(cloneNode(n), yaml.WithIndent(2), yaml.WithLineWidth(-1), yaml.WithCompactSeqIndent(false))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// renderInline renders a scalar or flow collection on one line.
func renderInline(n *yaml.Node) (string, bool) {
	s, err := dumpNode(n)
	if err != nil || strings.Contains(s, "\n") {
		return "", false
	}
	return s, true
}

// renderBlockItem renders a sequence item as "- ..." lines at the given indent.
func (d *Doc) renderBlockItem(item *yaml.Node, indent int) (string, bool) {
	s, err := dumpNode(item)
	if err != nil {
		return "", false
	}
	pad := strings.Repeat(" ", indent)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if i == 0 {
			lines[i] = pad + "- " + l
		} else {
			lines[i] = pad + "  " + l
		}
	}
	return strings.Join(lines, d.nl), true
}

// ---- splices ------------------------------------------------------------

func (d *Doc) addSplice(start, end int, text string) {
	d.order++
	d.splices = append(d.splices, splice{start: start, end: end, text: text, order: d.order})
}

func (d *Doc) insertAfterLine(line int, text string) {
	d.addSplice(d.lineEnd(line), d.lineEnd(line), d.nl+text)
}

// replaceValueText swaps the text of value node old for new's rendering.
func (d *Doc) replaceValueText(old, new *yaml.Node, inFlow bool) bool {
	start, end, ok := d.valueSpan(old, inFlow)
	if !ok {
		return false
	}
	text, ok := renderInline(new)
	if !ok {
		return false
	}
	if start == end && old.Kind == yaml.ScalarNode { // empty null value, e.g. "key:" or "key: # c"
		if start > 0 && d.src[start-1] == ':' {
			text = " " + text
		} else if start < len(d.src) && d.src[start] == '#' {
			text += " "
		}
	}
	d.addSplice(start, end, text)
	return true
}

// ---- edits --------------------------------------------------------------

// SetField sets m[key] = value (adding the key when missing). value should be a
// scalar or a flow collection to stay surgical.
func (d *Doc) SetField(m *yaml.Node, key string, value *yaml.Node) {
	d.changed = true
	if i := findKey(m, key); i >= 0 {
		old := m.Content[i+1]
		m.Content[i+1] = value
		if old.Line == 0 || !d.replaceValueText(old, value, m.Style&yaml.FlowStyle != 0) {
			d.reencode = true
		}
		return
	}
	if m.Line == 0 || m.Style&yaml.FlowStyle != 0 || len(m.Content) == 0 {
		m.Content = append(m.Content, strNode(key), value)
		d.reencode = true
		return
	}
	last := lastOriginalValue(m)
	first := m.Content[0]
	m.Content = append(m.Content, strNode(key), value)
	end, ok := d.endLine(last)
	if !ok {
		d.reencode = true
		return
	}
	k, ok1 := renderInline(strNode(key))
	v, ok2 := renderInline(value)
	if !ok1 || !ok2 {
		d.reencode = true
		return
	}
	d.insertAfterLine(end, strings.Repeat(" ", first.Column-1)+k+": "+v)
}

// lastOriginalValue is the value of the last mapping entry that exists in the
// source; entries appended earlier in the same edit have no position yet.
func lastOriginalValue(m *yaml.Node) *yaml.Node {
	for i := len(m.Content) - 2; i >= 0; i -= 2 {
		if m.Content[i].Line > 0 {
			return m.Content[i+1]
		}
	}
	return nil
}

func lastOriginalItem(seq *yaml.Node) *yaml.Node {
	for i := len(seq.Content) - 1; i >= 0; i-- {
		if seq.Content[i].Line > 0 {
			return seq.Content[i]
		}
	}
	return nil
}

// SetBlockField sets m[key] to a block collection (e.g. a new section mapping).
func (d *Doc) SetBlockField(m *yaml.Node, key string, value *yaml.Node) {
	d.changed = true
	if i := findKey(m, key); i >= 0 || m.Line == 0 || m.Style&yaml.FlowStyle != 0 || len(m.Content) == 0 {
		if i >= 0 {
			m.Content[i+1] = value
		} else {
			m.Content = append(m.Content, strNode(key), value)
		}
		d.reencode = true
		return
	}
	last, first := lastOriginalValue(m), m.Content[0]
	m.Content = append(m.Content, strNode(key), value)
	end, ok := d.endLine(last)
	body, err := dumpNode(mapNode(key, value))
	if !ok || err != nil {
		d.reencode = true
		return
	}
	pad := strings.Repeat(" ", first.Column-1)
	lines := strings.Split(body, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	d.insertAfterLine(end, strings.Join(lines, d.nl))
}

// RenameField renames key oldKey to newKey and sets its value, in place.
func (d *Doc) RenameField(m *yaml.Node, oldKey, newKey string, value *yaml.Node) {
	i := findKey(m, oldKey)
	if i < 0 {
		d.SetField(m, newKey, value)
		return
	}
	d.changed = true
	kNode, old := m.Content[i], m.Content[i+1]
	m.Content[i], m.Content[i+1] = strNode(newKey), value
	inFlow := m.Style&yaml.FlowStyle != 0
	ks, ke, ok1 := d.scalarSpan(kNode, inFlow)
	if !ok1 || old.Line == 0 {
		d.reencode = true
		return
	}
	kt, ok2 := renderInline(strNode(newKey))
	if !ok2 {
		d.reencode = true
		return
	}
	d.addSplice(ks, ke, kt)
	if !d.replaceValueText(old, value, inFlow) {
		d.reencode = true
	}
}

// DeleteField removes m[key] (and the comment lines directly above it).
func (d *Doc) DeleteField(m *yaml.Node, key string) {
	i := findKey(m, key)
	if i < 0 {
		return
	}
	d.changed = true
	k, v := m.Content[i], m.Content[i+1]
	m.Content = append(m.Content[:i:i], m.Content[i+2:]...)
	if m.Style&yaml.FlowStyle != 0 || k.Line == 0 || !d.onlySpacesBefore(k) {
		d.reencode = true
		return
	}
	end, ok := d.endLine(v)
	if !ok {
		d.reencode = true
		return
	}
	start := k.Line
	if k.HeadComment != "" {
		start = d.commentStart(k.Line)
	}
	s, e := d.lineSpan(start, end)
	d.addSplice(s, e, "")
}

// commentStart walks up over comment-only lines directly above line.
func (d *Doc) commentStart(line int) int {
	for line > 1 {
		s, e := d.lineSpan(line-1, line-1)
		t := strings.TrimSpace(string(d.src[s:e]))
		if !strings.HasPrefix(t, "#") {
			break
		}
		line--
	}
	return line
}

// AppendItem appends item to the sequence at m[key], converting a null or "[]" value to a block sequence.
func (d *Doc) AppendItem(m *yaml.Node, key string, item *yaml.Node) {
	d.changed = true
	i := findKey(m, key)
	if i < 0 {
		d.SetBlockField(m, key, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{item}})
		return
	}
	k, v := m.Content[i], m.Content[i+1]
	childIndent := k.Column - 1 + 2
	switch {
	case isNull(v) && v.Value == "" && v.Line == k.Line:
		m.Content[i+1] = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{item}}
		text, ok := d.renderBlockItem(item, childIndent)
		if !ok || k.Line == 0 {
			d.reencode = true
			return
		}
		d.insertAfterLine(k.Line, text)
	case v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle != 0 && len(v.Content) == 0 && v.Line > 0:
		m.Content[i+1] = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{item}}
		s, e, ok := d.flowSpan(v)
		text, ok2 := d.renderBlockItem(item, childIndent)
		if !ok || !ok2 {
			d.reencode = true
			return
		}
		for s > 0 && d.src[s-1] == ' ' { // "keys: []" -> "keys:" + newline + items
			s--
		}
		d.addSplice(s, e, d.nl+text)
	case v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle == 0 && len(v.Content) > 0 && v.Line > 0:
		last := lastOriginalItem(v)
		v.Content = append(v.Content, item)
		end, ok := d.endLine(last)
		text, ok2 := d.renderBlockItem(item, v.Column-1)
		if !ok || !ok2 {
			d.reencode = true
			return
		}
		d.insertAfterLine(end, text)
	default:
		if v.Kind == yaml.SequenceNode {
			v.Content = append(v.Content, item)
		} else {
			m.Content[i+1] = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{item}}
		}
		d.reencode = true
	}
}

// DeleteItem removes seq[i] (and the comment lines directly above it).
func (d *Doc) DeleteItem(seq *yaml.Node, i int) {
	item := seq.Content[i]
	seq.Content = append(seq.Content[:i:i], seq.Content[i+1:]...)
	d.changed = true
	if seq.Style&yaml.FlowStyle != 0 || item.Line == 0 {
		d.reencode = true
		return
	}
	// The "- " marker must sit on the item's first line, right before its content.
	off, ok := d.offset(item.Line, item.Column)
	if !ok || off < 2 || d.src[off-2] != '-' || d.src[off-1] != ' ' {
		d.reencode = true
		return
	}
	end, ok := d.endLine(item)
	if !ok {
		d.reencode = true
		return
	}
	start := item.Line
	head := item.HeadComment
	if item.Kind == yaml.MappingNode && len(item.Content) > 0 && head == "" {
		head = item.Content[0].HeadComment
	}
	if head != "" {
		start = d.commentStart(item.Line)
	}
	s, e := d.lineSpan(start, end)
	d.addSplice(s, e, "")
}

// ItemMapping makes seq[i] a mapping, expanding the shorthand "- value" into
// "- keyField: value" so fields can be added.
func (d *Doc) ItemMapping(seq *yaml.Node, i int, keyField string) *yaml.Node {
	it := seq.Content[i]
	if it.Kind == yaml.MappingNode {
		return it
	}
	k := strNode(keyField)
	k.Line, k.Column = it.Line, it.Column // new fields align with the old value
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Line: it.Line, Column: it.Column, Content: []*yaml.Node{k, it}}
	seq.Content[i] = m
	d.changed = true
	s, _, ok := d.scalarSpan(it, seq.Style&yaml.FlowStyle != 0)
	if !ok || seq.Style&yaml.FlowStyle != 0 {
		d.reencode = true
		return m
	}
	d.addSplice(s, s, keyField+": ")
	return m
}

// ---- output -------------------------------------------------------------

// Bytes returns the edited document: surgical when possible, re-encoded otherwise.
func (d *Doc) Bytes() ([]byte, error) {
	if !d.changed {
		return d.src, nil
	}
	if !d.reencode {
		if out, ok := d.applySplices(); ok {
			if verifyTree(out, d.root) {
				return out, nil
			}
		}
	}
	return d.reencoded()
}

// Surgical reports whether the last Bytes() call could preserve formatting (for tests/diagnostics).
func (d *Doc) Surgical() bool {
	if !d.changed {
		return true
	}
	if d.reencode {
		return false
	}
	out, ok := d.applySplices()
	return ok && verifyTree(out, d.root)
}

func (d *Doc) applySplices() ([]byte, bool) {
	sp := append([]splice(nil), d.splices...)
	sort.SliceStable(sp, func(a, b int) bool {
		if sp[a].start != sp[b].start {
			return sp[a].start < sp[b].start
		}
		za, zb := sp[a].start == sp[a].end, sp[b].start == sp[b].end
		if za != zb {
			return za // pure insertions before a replacement at the same offset
		}
		return sp[a].order < sp[b].order
	})
	var out bytes.Buffer
	pos := 0
	for _, s := range sp {
		if s.start < pos || s.end < s.start || s.end > len(d.src) {
			return nil, false // overlapping edits
		}
		out.Write(d.src[pos:s.start])
		out.WriteString(s.text)
		pos = s.end
	}
	out.Write(d.src[pos:])
	return out.Bytes(), true
}

func (d *Doc) reencoded() ([]byte, error) {
	if len(d.root.Content) == 0 {
		return []byte{}, nil
	}
	out, err := yaml.Dump(cloneNode(d.root), yaml.WithIndent(2), yaml.WithLineWidth(-1), yaml.WithCompactSeqIndent(false))
	if err != nil {
		return nil, fmt.Errorf("%s: re-encode: %w", d.name, err)
	}
	if d.nl == "\r\n" {
		out = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
	}
	return out, nil
}

// verifyTree checks that out parses to the same structure as want (comments ignored).
func verifyTree(out []byte, want *yaml.Node) bool {
	var got yaml.Node
	if err := yaml.Unmarshal(out, &got); err != nil {
		return false
	}
	var g, w *yaml.Node
	if len(got.Content) > 0 {
		g = got.Content[0]
	}
	if len(want.Content) > 0 {
		w = want.Content[0]
	}
	return sameNode(g, w)
}

func emptyish(n *yaml.Node) bool {
	return n == nil || isNull(n) ||
		((n.Kind == yaml.SequenceNode || n.Kind == yaml.MappingNode) && len(n.Content) == 0)
}

func sameNode(a, b *yaml.Node) bool {
	if emptyish(a) || emptyish(b) {
		return emptyish(a) && emptyish(b)
	}
	if a.Kind == yaml.AliasNode || b.Kind == yaml.AliasNode {
		return false // never claim equivalence across aliases; re-encode instead
	}
	if a.Kind != b.Kind || len(a.Content) != len(b.Content) {
		return false
	}
	if a.Kind == yaml.ScalarNode {
		return scalarKey(a) == scalarKey(b)
	}
	for i := range a.Content {
		if !sameNode(a.Content[i], b.Content[i]) {
			return false
		}
	}
	return true
}

func scalarKey(n *yaml.Node) string {
	switch n.Tag {
	case "!!int", "!!float":
		if f, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64); err == nil {
			return "n:" + strconv.FormatFloat(f, 'g', -1, 64)
		}
	case "!!bool":
		return "b:" + strings.ToLower(n.Value)
	case "!!null":
		return "z:"
	}
	return "s:" + n.Value
}
