// Package web embeds the admin panel's templates and static assets
// (ARCHITECTURE.md §2: html/template, no build step, strict CSP — no inline scripts or styles).
package web

import "embed"

// FS holds templates/*.html and static/*.
//
//go:embed templates/*.html static/*
var FS embed.FS
