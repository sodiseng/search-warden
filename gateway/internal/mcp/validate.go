package mcp

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// Argument validation for the Tavily tools.
//
// The official remote server (tavily-mcp 4.1.5, checked 2026-11-10) validates
// tool arguments itself, before it calls the API, and answers a violation with
// the JSON-RPC error -32603 carrying pydantic's message. It validates in
// pydantic's lax mode: "5" is an integer, "no" is a boolean, and what it
// accepts reaches the API in its declared type. Its answers are recorded in
// testdata/golden/tavily-10-validation.json.
//
// The rules below reproduce that from the tool's inputSchema in the snapshot,
// so new parameters arrive with a refreshed snapshot. Only violations the
// official server is known to reject are rejected here: anything unclear is
// left for the API to judge, as the gateway did before.

const pydanticDocs = "https://errors.pydantic.dev/2.14/v/"

// argError is one pydantic-style validation error.
type argError struct {
	path, msg, kind string
	input           gjson.Result
}

var (
	intLiteralRe  = regexp.MustCompile(`^-?[0-9]+$`)
	intStringRe   = regexp.MustCompile(`^[+-]?[0-9]+$`)
	intDotZeroRe  = regexp.MustCompile(`^([+-]?[0-9]+)\.0*$`)
	fractionRe    = regexp.MustCompile(`^[+-]?[0-9]*\.[0-9]*[1-9][0-9]*$`)
	intCharsRe    = regexp.MustCompile(`^[0-9+\-_.eE]+$`)
	laxTrueFalse  = map[string]bool{"1": true, "on": true, "t": true, "true": true, "y": true, "yes": true, "0": false, "off": false, "f": false, "false": false, "n": false, "no": false}
	jsonTrue      = json.RawMessage("true")
	jsonFalse     = json.RawMessage("false")
	pyTypeOfInput = map[gjson.Type]string{gjson.Null: "NoneType", gjson.True: "bool", gjson.False: "bool", gjson.String: "str"}
)

// validateArgs checks the arguments of a tool call against the tool's
// inputSchema. It returns them with lax values turned into their declared type
// (in the order received), or the message of the official error.
func validateArgs(tool string, schema, args gjson.Result) (*ordered, string) {
	given := map[string]gjson.Result{}
	var order []string
	if args.IsObject() {
		args.ForEach(func(k, v gjson.Result) bool {
			if _, dup := given[k.String()]; !dup {
				order = append(order, k.String())
			}
			given[k.String()] = v
			return true
		})
	} else {
		args = gjson.Parse("{}")
	}
	required := map[string]bool{}
	for _, r := range schema.Get("required").Array() {
		required[r.String()] = true
	}

	var errs []argError
	coerced := map[string]json.RawMessage{}
	declared := map[string]bool{}
	schema.Get("properties").ForEach(func(k, prop gjson.Result) bool { // declared parameters first, in their order
		name := k.String()
		declared[name] = true
		v, ok := given[name]
		if !ok {
			if required[name] {
				errs = append(errs, argError{name, "Missing required argument", "missing_argument", args})
			}
			return true
		}
		raw, es := checkValue(name, prop, v)
		coerced[name], errs = raw, append(errs, es...)
		return true
	})
	strict := schema.Get("additionalProperties").Type == gjson.False
	for _, name := range order { // then what the tool does not know, as received
		if !declared[name] && strict {
			errs = append(errs, argError{name, "Unexpected keyword argument", "unexpected_keyword_argument", given[name]})
		}
	}
	if len(errs) > 0 {
		return nil, formatArgErrors(tool, errs)
	}
	out := newOrdered()
	for _, name := range order {
		if raw, ok := coerced[name]; ok {
			out.set(name, raw)
		} else {
			out.set(name, json.RawMessage(given[name].Raw))
		}
	}
	return out, ""
}

func formatArgErrors(tool string, errs []argError) string {
	var b strings.Builder
	b.WriteString("Internal error: " + strconv.Itoa(len(errs)) + " validation error")
	if len(errs) != 1 {
		b.WriteByte('s')
	}
	b.WriteString(" for call[" + tool + "]")
	for _, e := range errs {
		b.WriteString("\n" + e.path + "\n  " + e.msg + " [type=" + e.kind + ", input_value=" + pyTruncate(pyRepr(e.input)) +
			", input_type=" + pyType(e.input) + "]\n    For further information visit " + pydanticDocs + e.kind)
	}
	return b.String()
}

// checkValue validates v against one property schema and returns the value in
// its declared type.
func checkValue(path string, sch, v gjson.Result) (json.RawMessage, []argError) {
	raw := json.RawMessage(v.Raw)
	fail := func(msg, kind string) (json.RawMessage, []argError) {
		return raw, []argError{{path, msg, kind, v}}
	}
	if alts := sch.Get("anyOf"); alts.IsArray() {
		var rest []gjson.Result
		nullable := false
		for _, alt := range alts.Array() {
			if alt.Get("type").String() == "null" {
				nullable = true
			} else {
				rest = append(rest, alt)
			}
		}
		if nullable && v.Type == gjson.Null {
			return raw, nil
		}
		if len(rest) != 1 {
			return raw, nil // a real union: left to the API
		}
		sch = rest[0]
	}
	if c := sch.Get("const"); c.Exists() {
		if v.Type != c.Type || v.Raw != c.Raw {
			return fail("Input should be "+pyRepr(c), "literal_error")
		}
		return raw, nil
	}
	if enum := sch.Get("enum"); enum.IsArray() {
		var names []string
		for _, e := range enum.Array() {
			if e.Type == v.Type && e.Raw == v.Raw {
				return raw, nil
			}
			names = append(names, pyRepr(e))
		}
		return fail("Input should be "+orList(names), "literal_error")
	}

	switch sch.Get("type").String() {
	case "string":
		if v.Type != gjson.String {
			return fail("Input should be a valid string", "string_type")
		}
	case "integer":
		return checkInteger(path, sch, v)
	case "boolean":
		switch v.Type {
		case gjson.True, gjson.False:
		case gjson.Number:
			switch f := v.Float(); {
			case f == 0:
				return jsonFalse, nil
			case f == 1:
				return jsonTrue, nil
			case intLiteralRe.MatchString(v.Raw):
				return fail("Input should be a valid boolean, unable to interpret input", "bool_parsing")
			default:
				return fail("Input should be a valid boolean", "bool_type")
			}
		case gjson.String:
			b, ok := laxTrueFalse[strings.ToLower(v.String())]
			if !ok {
				return fail("Input should be a valid boolean, unable to interpret input", "bool_parsing")
			}
			if b {
				return jsonTrue, nil
			}
			return jsonFalse, nil
		default:
			return fail("Input should be a valid boolean", "bool_type")
		}
	case "array":
		if !v.IsArray() {
			return fail("Input should be a valid list", "list_type")
		}
		items := sch.Get("items")
		if !items.Exists() {
			return raw, nil
		}
		var errs []argError
		out := make([]json.RawMessage, 0, 8)
		for i, item := range v.Array() {
			r, es := checkValue(path+"."+strconv.Itoa(i), items, item)
			out, errs = append(out, r), append(errs, es...)
		}
		return marshal(out), errs
	case "object":
		if !v.IsObject() || !sch.Get("properties").Exists() {
			return raw, nil // not modelled: left to the API
		}
		var errs []argError
		fields := map[string]json.RawMessage{}
		required := map[string]bool{}
		for _, r := range sch.Get("required").Array() {
			required[r.String()] = true
		}
		sch.Get("properties").ForEach(func(k, prop gjson.Result) bool {
			name := k.String()
			f := v.Get(gjsonKey(name))
			if !f.Exists() {
				if required[name] {
					errs = append(errs, argError{path + "." + name, "Field required", "missing", v})
				}
				return true
			}
			r, es := checkValue(path+"."+name, prop, f)
			fields[name], errs = r, append(errs, es...)
			return true
		})
		out := newOrdered()
		v.ForEach(func(k, f gjson.Result) bool {
			if r, ok := fields[k.String()]; ok {
				out.set(k.String(), r)
			} else {
				out.set(k.String(), json.RawMessage(f.Raw))
			}
			return true
		})
		return marshal(out), errs
	}
	return raw, nil
}

// checkInteger follows pydantic's lax integer: whole floats, booleans and
// integer-looking strings count.
func checkInteger(path string, sch, v gjson.Result) (json.RawMessage, []argError) {
	raw := json.RawMessage(v.Raw)
	fail := func(msg, kind string) (json.RawMessage, []argError) {
		return raw, []argError{{path, msg, kind, v}}
	}
	lit := ""
	switch v.Type {
	case gjson.Number:
		if intLiteralRe.MatchString(v.Raw) {
			lit = v.Raw
			break
		}
		f := v.Float()
		if math.IsInf(f, 0) || math.IsNaN(f) || f != math.Trunc(f) {
			return fail("Input should be a valid integer, got a number with a fractional part", "int_from_float")
		}
		lit = strconv.FormatFloat(f, 'f', 0, 64)
	case gjson.True:
		lit = "1"
	case gjson.False:
		lit = "0"
	case gjson.String:
		s := strings.TrimSpace(v.String())
		switch m := intDotZeroRe.FindStringSubmatch(s); {
		case intStringRe.MatchString(s):
			lit = strings.TrimPrefix(s, "+")
		case m != nil:
			lit = strings.TrimPrefix(m[1], "+")
		case s == "" || !intCharsRe.MatchString(s) || fractionRe.MatchString(s):
			return fail("Input should be a valid integer, unable to parse string as an integer", "int_parsing")
		default:
			return raw, nil // e.g. "1_000" or "1e3": left to the API
		}
	default:
		return fail("Input should be a valid integer", "int_type")
	}
	if n, err := strconv.ParseFloat(lit, 64); err == nil {
		if lo := sch.Get("minimum"); lo.Type == gjson.Number && n < lo.Float() {
			return fail("Input should be greater than or equal to "+lo.Raw, "greater_than_equal")
		}
		if hi := sch.Get("maximum"); hi.Type == gjson.Number && n > hi.Float() {
			return fail("Input should be less than or equal to "+hi.Raw, "less_than_equal")
		}
	}
	return json.RawMessage(lit), nil
}

// orList joins like pydantic: 'a', 'b' or 'c'.
func orList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// ---- Python's view of a JSON value, as pydantic prints it ----------------------

func pyType(v gjson.Result) string {
	switch {
	case v.IsArray():
		return "list"
	case v.IsObject():
		return "dict"
	case v.Type == gjson.Number:
		if intLiteralRe.MatchString(v.Raw) {
			return "int"
		}
		return "float"
	}
	return pyTypeOfInput[v.Type]
}

// pyRepr is repr() of the value json.loads would produce.
func pyRepr(v gjson.Result) string {
	switch {
	case v.IsArray():
		var parts []string
		for _, x := range v.Array() {
			parts = append(parts, pyRepr(x))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case v.IsObject():
		var parts []string
		v.ForEach(func(k, x gjson.Result) bool {
			parts = append(parts, pyString(k.String())+": "+pyRepr(x))
			return true
		})
		return "{" + strings.Join(parts, ", ") + "}"
	}
	switch v.Type {
	case gjson.Null:
		return "None"
	case gjson.True:
		return "True"
	case gjson.False:
		return "False"
	case gjson.String:
		return pyString(v.String())
	case gjson.Number:
		if intLiteralRe.MatchString(v.Raw) {
			return v.Raw
		}
		return pyFloat(v.Float())
	}
	return v.Raw
}

func pyString(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x`)
			b.WriteString(strconv.FormatInt(int64(r)+0x100, 16)[1:])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// pyFloat is repr(float): the shortest form that reads back, never without a
// decimal point or an exponent.
func pyFloat(f float64) string {
	if a := math.Abs(f); a != 0 && (a < 1e-4 || a >= 1e16) {
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// pyTruncate shortens a long input_value the way pydantic does: 50 characters
// at most, the middle replaced by "...".
func pyTruncate(s string) string {
	if utf8.RuneCountInString(s) <= 50 {
		return s
	}
	r := []rune(s)
	return string(r[:25]) + "..." + string(r[len(r)-24:])
}
