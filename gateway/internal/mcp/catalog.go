package mcp

import (
	"embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/sodiseng/search-warden/gateway/internal/buildinfo"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
)

// Snapshots of the official remote servers' initialize and tools/list results,
// fetched by scripts/fetch-mcp-schemas.py (ARCHITECTURE.md §8.4). Served verbatim.
//
//go:embed schemas/*.json
var schemaFS embed.FS

type toolDef struct {
	name     string
	provider string
	raw      json.RawMessage // the tool object exactly as the official server lists it
}

type providerSnapshot struct {
	serverInfo   json.RawMessage
	instructions string
	tools        []toolDef
}

// Exa's default tool set when ?tools= is absent (official: search and fetch,
// plus agent_run for authenticated connections — gateway callers always are).
var exaDefaults = []string{"web_search_exa", "web_fetch_exa", "agent_run"}

// toolAliases are accepted in ?tools= like the official Exa server does.
var toolAliases = map[string][]string{
	"agent_tools":          {"agent_run"},
	"agent_create_run":     {"agent_run"},
	"agent_run_stream":     {"agent_run"},
	"agent_wait_for_run":   {"agent_run"},
	"agent_get_run_output": {"agent_run"},
	"agent_cancel_run":     {"agent_run"},
}

func loadSnapshots(implemented map[string]bool) (map[string]*providerSnapshot, []string, error) {
	out := map[string]*providerSnapshot{}
	var skipped []string
	for _, p := range confstore.Providers {
		b, err := schemaFS.ReadFile("schemas/" + p + ".json")
		if err != nil {
			return nil, nil, fmt.Errorf("mcp: snapshot for %s: %w", p, err)
		}
		if !gjson.ValidBytes(b) {
			return nil, nil, fmt.Errorf("mcp: snapshot for %s is not valid JSON", p)
		}
		root := gjson.ParseBytes(b)
		ps := &providerSnapshot{serverInfo: json.RawMessage(root.Get("serverInfo").Raw), instructions: root.Get("instructions").String()}
		for _, t := range root.Get("tools").Array() {
			name := t.Get("name").String()
			if !implemented[name] {
				skipped = append(skipped, name) // listed upstream, not yet implemented here
				continue
			}
			var raw json.RawMessage = []byte(t.Raw)
			ps.tools = append(ps.tools, toolDef{name: name, provider: p, raw: raw})
		}
		if len(ps.tools) == 0 || len(ps.serverInfo) == 0 {
			return nil, nil, fmt.Errorf("mcp: snapshot for %s has no usable tools or server info", p)
		}
		out[p] = ps
	}
	return out, skipped, nil
}

// view is what one request may see: the providers and tools, in a stable order.
type view struct {
	scope     string // "tavily", "exa", or "" for the merged endpoint
	providers []string
	tools     []toolDef
}

func (v *view) tool(name string) (toolDef, bool) {
	for _, t := range v.tools {
		if t.name == name {
			return t, true
		}
	}
	return toolDef{}, false
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// selectTools applies the endpoint, ?providers=, ?tools= and the token's scope
// (ARCHITECTURE.md §8.1–8.2).
func (s *Server) selectTools(scope string, tok *confstore.Token, providersParam, toolsParam string) *view {
	v := &view{scope: scope}
	if scope != "" {
		v.providers = []string{scope}
	} else {
		want := splitList(providersParam)
		for _, p := range confstore.Providers {
			if slices.Contains(tok.Providers, p) && (len(want) == 0 || slices.Contains(want, p)) {
				v.providers = append(v.providers, p)
			}
		}
	}

	var requested []string
	for _, name := range splitList(toolsParam) {
		if alias, ok := toolAliases[name]; ok {
			requested = append(requested, alias...)
		} else {
			requested = append(requested, name)
		}
	}
	known := func(name string) bool {
		for _, p := range v.providers {
			for _, t := range s.snaps[p].tools {
				if t.name == name {
					return true
				}
			}
		}
		return false
	}
	requested = slices.DeleteFunc(requested, func(n string) bool { return !known(n) })

	for _, p := range v.providers {
		for _, t := range s.snaps[p].tools {
			var on bool
			switch {
			case len(requested) > 0: // an explicit list replaces the defaults (as with the official Exa server)
				on = slices.Contains(requested, t.name)
			case p == "exa":
				on = slices.Contains(exaDefaults, t.name)
			default:
				on = true
			}
			if on && (len(tok.Tools) == 0 || slices.Contains(tok.Tools, t.name)) {
				v.tools = append(v.tools, t)
			}
		}
	}
	return v
}

// serverInfo is the official server's identity on its own endpoint, and the
// gateway's on the merged one.
func (s *Server) serverInfo(scope string) json.RawMessage {
	if scope != "" {
		return s.snaps[scope].serverInfo
	}
	return marshal(map[string]string{"name": "search-warden", "version": buildinfo.Version})
}

func (s *Server) instructions(v *view) string {
	if v.scope != "" {
		return s.snaps[v.scope].instructions
	}
	var parts []string
	for _, p := range v.providers {
		if in := s.snaps[p].instructions; in != "" {
			parts = append(parts, in)
		}
	}
	return strings.Join(parts, "\n\n")
}
