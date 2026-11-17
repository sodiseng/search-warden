// Package examples embeds the commented configuration templates that are
// written to the data directory on first start (ARCHITECTURE.md §14.2).
package examples

import "embed"

// FS holds config.yaml, egresses.yaml, tokens.yaml and keys/*.yaml templates.
//
//go:embed config.yaml egresses.yaml tokens.yaml keys/tavily.yaml keys/exa.yaml
var FS embed.FS
