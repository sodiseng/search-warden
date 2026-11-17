# vendored front-end libraries

Served from the binary (go:embed); no CDN at runtime, so the admin CSP stays `default-src 'self'`.

| File | Source | Version | SHA-256 | License |
|---|---|---|---|---|
| `htmx-2.0.4.min.js` | https://cdn.jsdelivr.net/npm/htmx.org@2.0.4/dist/htmx.min.js, unmodified | 2.0.4 | `e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447` | Zero-Clause BSD (see NOTICE) |
| `pico-2.1.1.indigo.min.css` | npm `@picocss/pico@2.1.1`, `css/pico.indigo.min.css`, unmodified | 2.1.1 | `3ff75cde84c76491549e1a7c64294c2c83cb2f92231ea44692f9ffc69897a811` | MIT (see NOTICE) |
| `inter-latin-wght-normal.woff2` | npm `@fontsource-variable/inter@5.3.0`, `files/inter-latin-wght-normal.woff2`, unmodified (Inter variable font, Latin subset) | 5.3.0 | `3100e775e8616cd2611beecfa23a4263d7037586789b43f035236a2e6fbd4c62` | SIL Open Font License 1.1 (see NOTICE) |
| `lucide-1.72.0.svg` | npm `lucide-static@1.72.0`: the icons the templates use, combined into one `<symbol>` sprite | 1.72.0 | `aef984f2826cac307becaaa078aa4af5969771e88a19c9a20f97a4d8df49514f` | ISC, some icons MIT (see NOTICE) |

htmx is configured in `templates/layout.html` with `allowEval: false` and
`includeIndicatorStyles: false`, so it needs neither `unsafe-eval` nor inline styles.

Pico is the base layer (reset, form controls, `<dialog>`, light and dark colour
schemes via `data-theme`); the panel's own look is `../app.css`, loaded after it.
Chinese text uses the system fonts: only the Latin subset of Inter is shipped.
