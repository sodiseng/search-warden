package mcp

import (
	"bytes"
	"encoding/json"
	"slices"
)

// Protocol versions (ARCHITECTURE.md §8.2). Modern versions carry their
// version and client capabilities in every request's _meta; legacy versions
// open with an initialize handshake. The gateway serves both, statelessly.
const latestVersion = "2026-07-28"

var (
	modernVersions = []string{"2026-07-28"}
	legacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}
	allVersions    = append(slices.Clone(modernVersions), legacyVersions...)
)

// _meta keys defined by the 2026-07-28 revision.
const (
	metaProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	metaServerInfo      = "io.modelcontextprotocol/serverInfo"
)

// JSON-RPC and MCP error codes.
const (
	codeParseError         = -32700
	codeInvalidRequest     = -32600
	codeMethodNotFound     = -32601
	codeInvalidParams      = -32602
	codeInternal           = -32603
	codeHeaderMismatch     = -32020
	codeUnsupportedVersion = -32022
)

// listTTLMillis is the cache hint on tools/list and server/discover: the tool
// set only changes with a new snapshot (a restart) or a token edit.
const listTTLMillis = 300_000

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// message is one incoming JSON-RPC message.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

func (m *message) isNotification() bool { return m.Method != "" && len(m.ID) == 0 }
func (m *message) isResponse() bool     { return m.Method == "" && (len(m.Result) > 0 || len(m.Error) > 0) }

func responseJSON(id json.RawMessage, result json.RawMessage, e *rpcError) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	v := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *rpcError       `json:"error,omitempty"`
	}{"2.0", id, result, e}
	return marshal(v)
}

func notificationJSON(method string, params any) []byte {
	return marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", method, params})
}

// marshal encodes like JavaScript's JSON.stringify: no HTML escaping, compact.
func marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// ordered is a JSON object that keeps insertion order, so text built from
// upstream data reads in the same order as the official servers' output.
type ordered struct {
	keys []string
	vals map[string]any
}

func newOrdered() *ordered { return &ordered{vals: map[string]any{}} }

func (o *ordered) set(k string, v any) *ordered {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
	return o
}

func (o *ordered) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *ordered) has(k string) bool { _, ok := o.vals[k]; return ok }
func (o *ordered) len() int          { return len(o.keys) }

func (o *ordered) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(marshal(k))
		buf.WriteByte(':')
		buf.Write(marshal(o.vals[k]))
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
