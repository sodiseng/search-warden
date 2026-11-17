<div align="center">

# Search Warden

**A self-hosted gateway for your Tavily and Exa API keys.**

Pool many keys behind one endpoint, keep using the official MCP tools and SDKs,
send each key through its own proxy, and watch everything from an admin panel.

[![CI](https://github.com/sodiseng/search-warden/actions/workflows/ci.yml/badge.svg)](https://github.com/sodiseng/search-warden/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
![Go 1.27](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)
![Docker](https://img.shields.io/badge/Docker-compose-2496ED?logo=docker&logoColor=white)

**English** | [简体中文](README.zh-CN.md)

</div>

---

## Table of contents

- [Features](#features)
- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Docker Compose](#docker-compose)
- [Configuration](#configuration)
- [Connecting clients](#connecting-clients)
- [Admin panel](#admin-panel)
- [Reverse proxy and HTTPS](#reverse-proxy-and-https)
- [Data, backup and upgrade](#data-backup-and-upgrade)
- [Monitoring](#monitoring)
- [Build from source](#build-from-source)
- [Security notes](#security-notes)
- [License](#license)
- [Disclaimer](#disclaimer)

## Features

- **Drop-in MCP.** `/tavily/mcp`, `/exa/mcp` and a merged `/mcp` expose the same tools, names and parameters as the official Tavily and Exa MCP servers. In a client you change only the URL and the key.
- **REST passthrough.** Point an SDK's base URL at `/tavily` or `/exa` and keep the rest of your code.
- **Key pool.** Free keys first, paid keys as fallback, balanced by remaining quota, with optional weights. A failing key is skipped and the request is retried on another one.
- **One egress per key.** Bind a key to a SOCKS5 or HTTP proxy. If that proxy is down the key is paused; traffic never falls back to a direct connection.
- **Quota tracking.** Tavily usage is synced from the official `/usage` endpoint; Exa spend is counted from the cost each response reports.
- **Gateway tokens.** Give each agent or teammate its own token, limited by provider, interface, tools, key group, source IP and expiry, with per-minute, concurrency, daily and monthly limits.
- **Admin panel.** Dashboard, live request monitor (source IP and the key that served each request), keys, egresses, tokens, settings and an audit log. English and Chinese, light and dark.
- **Files are the source of truth.** Everything is plain YAML in one data directory, synced both ways with the panel.
- **Small footprint.** One static Go binary, SQLite by default, PostgreSQL optional.

## How it works

```text
 Agents and SDKs                    Search Warden                         Upstream
┌──────────────────┐  gateway  ┌──────────────────────────┐  your keys  ┌────────────────┐
│ MCP clients      │──token───▶│ auth and limits          │────────────▶│ api.tavily.com │
│ Tavily / Exa SDK │           │ key pool, retry, quota   │  per-key    │ api.exa.ai     │
└──────────────────┘           │ request log, admin panel │  egress     └────────────────┘
                               └──────────────────────────┘
```

Clients authenticate with a **gateway token**. The gateway picks one of your **upstream keys**, swaps the credential, sends the request through that key's egress, and records what it cost.

## Quick start

Requirements: Docker with the Compose plugin.

```bash
git clone https://github.com/sodiseng/search-warden.git
cd search-warden

cp .env.example .env            # then set WARDEN_ADMIN_PASSWORD (12+ characters)
mkdir -p data && sudo chown -R 65532:65532 data

docker compose up -d --build
```

Open `http://127.0.0.1:18683/admin/` and log in as `admin` with the password you set. Then:

1. **Egresses** (optional): import proxy nodes. Without any, keys connect directly.
2. **Keys**: add your Tavily and Exa keys, one per line as `key[,name][,egress][,plan]`. Press **Test** to check them.
3. **Tokens**: create a gateway token. It is shown once.
4. Point your client at the gateway: see [Connecting clients](#connecting-clients).

## Docker Compose

The repository ships three files for Compose:

| File | Purpose |
|---|---|
| [`docker-compose.yml`](docker-compose.yml) | The service definition. Every setting is listed and commented; optional ones are marked `[optional]`. |
| [`.env.example`](.env.example) | Template for `.env`. Only `WARDEN_ADMIN_PASSWORD` is required; every other line shows its default, commented out. |
| [`Dockerfile`](Dockerfile) | Builds the image: a static binary on a distroless base, running as a non-root user. |

### Common commands

| Task | Command |
|---|---|
| Start (SQLite, the default) | `docker compose up -d --build` |
| Start with PostgreSQL | `docker compose --profile postgres up -d --build` |
| Follow the logs | `docker compose logs -f warden` |
| Check health | `docker compose ps` |
| Stop | `docker compose down` |
| Upgrade | `git pull && docker compose up -d --build` |

### Using PostgreSQL

SQLite needs no setup and is enough for a small team. To use PostgreSQL instead, set both lines in `.env` (the same password in both) and start with the `postgres` profile:

```dotenv
PG_PASSWORD=choose-a-password
WARDEN_DB=postgres://warden:choose-a-password@postgres:5432/warden?sslmode=disable
```

### Two things that trip people up

- **Keep comments in `.env` on their own lines.** Compose reads `NAME=   # text` as the value `# text`.
- **The data directory must be writable by the container user** (uid 65532). Either run the `chown` from the quick start, or set `WARDEN_UID` and `WARDEN_GID` in `.env` to your own `id -u` and `id -g`.

### Prebuilt images

CI publishes `ghcr.io/sodiseng/search-warden:edge` on every push to `main`, and `:<version>` plus `:latest` for release tags. To run one instead of building, set `WARDEN_VERSION` in `.env` and start without `--build`.

## Configuration

There are two layers:

- **Startup settings** are environment variables, set in `.env`. They are listed below.
- **Everything else** (keys, egresses, tokens, routing, limits, log retention) lives in YAML files in the data directory and can be edited in the panel or by hand. See [Data, backup and upgrade](#data-backup-and-upgrade).

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `WARDEN_ADMIN_PASSWORD` | *(none, required once)* | Password of the panel user `admin`, at least 12 characters. Read only while `config.yaml` has no admin password yet; stored as an argon2id hash. |
| `WARDEN_DB` | `sqlite:///data/db` | `sqlite://<directory>` or `postgres://user:pass@host:5432/db`. |
| `WARDEN_ADMIN_LISTEN` | `shared` | `shared` serves the panel on the gateway port. `:18684` gives it a separate port. |
| `WARDEN_ADMIN_BASE_PATH` | `/admin/` | Path the panel is mounted at. Must not be `/` while the panel shares the gateway port. |
| `WARDEN_PUBLIC_URL` | *(empty)* | Public address of the gateway. Used only in the client configuration examples the panel shows. |
| `WARDEN_ADMIN_PUBLIC_URL` | *(empty)* | Public address of the panel. Logins and changes are accepted only from this origin. |
| `WARDEN_COOKIE_SECURE` | `false` | Set `true` when the panel is served over HTTPS. |
| `WARDEN_METRICS_TOKEN` | *(empty)* | Enables `GET /metrics`. At least 16 characters. |
| `WARDEN_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `WARDEN_LOG_FORMAT` | `json` | `json` or `text`. |
| `WARDEN_LISTEN` | `:18683` | Listen address of the gateway. Fixed inside the container; change the host port instead. |
| `WARDEN_DATA_DIR` | `/data` in the image, `./data` otherwise | The data directory. |

Compose-only variables, used by `docker-compose.yml` and not read by the gateway itself:

| Variable | Default | Description |
|---|---|---|
| `WARDEN_IMAGE`, `WARDEN_VERSION` | `ghcr.io/sodiseng/search-warden`, `latest` | Image name and tag. |
| `WARDEN_UID`, `WARDEN_GID` | `65532` | User the container runs as; owner of the files in `./data`. |
| `WARDEN_GATEWAY_BIND`, `WARDEN_GATEWAY_PORT` | `0.0.0.0`, `18683` | Host address and port of the gateway. |
| `WARDEN_ADMIN_BIND`, `WARDEN_ADMIN_PORT` | `127.0.0.1`, `18684` | Host address and port of a separate panel port. |
| `PG_PASSWORD` | `change-me` | Password of the bundled PostgreSQL. |

## Connecting clients

Replace `gw.example.com` with your gateway and `<token>` with a gateway token.

### MCP

| Use | URL | Where the token goes |
|---|---|---|
| Tavily tools | `https://gw.example.com/tavily/mcp` | `Authorization: Bearer <token>`, or `?tavilyApiKey=<token>` |
| Exa tools | `https://gw.example.com/exa/mcp` | `x-api-key: <token>`, or `?exaApiKey=<token>` |
| Both in one server | `https://gw.example.com/mcp` | `Authorization: Bearer <token>` |

The transport is Streamable HTTP. For example, in Claude Code:

```bash
claude mcp add --transport http search https://gw.example.com/mcp \
  --header "Authorization: Bearer <token>"
```

Or as JSON configuration:

```json
{
  "mcpServers": {
    "search": {
      "type": "http",
      "url": "https://gw.example.com/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

Available tools:

| Provider | Tools |
|---|---|
| Tavily | `tavily_search`, `tavily_extract`, `tavily_crawl`, `tavily_map`, `tavily_research` |
| Exa | `web_search_exa`, `web_fetch_exa`, `agent_run`; `web_search_advanced_exa` when listed in `?tools=` |

On `/mcp`, `?providers=tavily,exa` and `?tools=<name>,<name>` narrow the list. A token's own scope narrows it further.

### REST

Set the SDK's base URL to `https://gw.example.com/tavily` or `https://gw.example.com/exa` and use a gateway token as the API key.

```bash
# Tavily
curl https://gw.example.com/tavily/search \
  -H "Authorization: Bearer <token>" -H "Content-Type: application/json" \
  -d '{"query": "what is MCP", "max_results": 3}'

# Exa
curl https://gw.example.com/exa/search \
  -H "x-api-key: <token>" -H "Content-Type: application/json" \
  -d '{"query": "what is MCP", "numResults": 3}'
```

Only these paths are forwarded; anything else returns 404:

| Provider | Paths |
|---|---|
| Tavily | `POST /search`, `/extract`, `/crawl`, `/map`, `/research`; `GET /research/{id}`; `POST /feedback`; `GET /usage` (answered by the gateway with its own view) |
| Exa | `POST /search`, `/contents`, `/findSimilar`, `/answer`, `/research/v1`; `GET /research/v1/{id}`; `POST /agent/runs`; `GET /agent/runs/{id}`; `POST /agent/runs/{id}/cancel` |

## Admin panel

The panel is at `/admin/` on the gateway port.

| Page | What it is for |
|---|---|
| Overview | Request volume, spend, errors, and the state and remaining quota of every key. |
| Requests | Every request with its source IP, token, key, egress, status and cost. Can refresh live. |
| Keys | Add, edit, test, pause and remove upstream keys; bind them to egresses. |
| Egresses | Import, test, rename and remove proxy nodes; see each one's health and exit IP. |
| Tokens | Create, scope, limit, rotate and disable gateway tokens. |
| Settings | Routing strategy, retries, quota sync, log retention, trusted proxies. |
| Files | View and edit the YAML files directly; download them as a zip. |
| Security | Change the password, enable two-step verification (TOTP), manage sessions, read the audit log. |

Forgot the password? Add `password: <new password>` under `admin:` in `data/config.yaml`. The gateway replaces it with a hash within a second and signs out every session.

## Reverse proxy and HTTPS

For anything beyond local use, put the gateway behind a reverse proxy that terminates TLS, and set in `.env`:

```dotenv
WARDEN_GATEWAY_BIND=127.0.0.1
WARDEN_PUBLIC_URL=https://gw.example.com
WARDEN_ADMIN_PUBLIC_URL=https://gw.example.com
WARDEN_COOKIE_SECURE=true
```

`WARDEN_GATEWAY_BIND=127.0.0.1` matters: otherwise the port stays reachable over plain HTTP at the server's IP address, around the proxy.

**Caddy**

```caddyfile
gw.example.com {
	reverse_proxy 127.0.0.1:18683 {
		flush_interval -1
	}
}
```

**Nginx**

```nginx
location / {
    proxy_pass http://127.0.0.1:18683;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_buffering off;
    proxy_read_timeout 1200s;
}
```

Things to know:

- **Long requests.** MCP tool calls answer as a stream, and `tavily_research` can run for up to 15 minutes. Turn response buffering off and allow a read timeout of 20 minutes, as above. Cloudflare's free plan cuts proxied requests at 100 seconds.
- **Client IP.** The gateway trusts `X-Forwarded-For` from loopback and private addresses by default, so a proxy on the same host or Docker network works without configuration. Adjust `network.trusted_proxies` in `config.yaml` otherwise.
- **Tokens in URLs.** `?tavilyApiKey=` and `?exaApiKey=` carry the token. The gateway redacts them in its own logs; make sure your proxy's access log does too, or send the token in a header.
- **The bare domain returns 403.** Opening `https://gw.example.com/` shows nothing on purpose. Use `/healthz` to check that the service is up.

## Data, backup and upgrade

Everything lives in the data directory (`./data` with Compose):

| Path | Contents |
|---|---|
| `config.yaml` | Admin account, routing, quota sync, log retention, trusted proxies. |
| `keys/tavily.yaml`, `keys/exa.yaml` | Upstream keys, **in plaintext**. |
| `egresses.yaml` | Proxy nodes. |
| `tokens.yaml` | Gateway tokens, stored as SHA-256 hashes. |
| `db/` | SQLite files: key state, sessions, audit log, request log. |

The files are created from templates on first start (their comments are currently in Chinese). Edit them by hand or in the panel: a saved file takes effect in about a second, and a file with a mistake is ignored, with the previous version kept in use and the line number shown in the panel.

- **Backup.** Stop the container and copy `data/`, or run `sqlite3 data/db/state.db ".backup state.bak"` while it is running. With PostgreSQL, use `pg_dump` and copy the YAML files.
- **Upgrade.** `git pull && docker compose up -d --build`. Database migrations run at startup.

## Monitoring

- `GET /healthz` returns `{"status":"ok"}`. It reports `degraded`, still with status 200, when a configuration file has an edit that could not be applied.
- `GET /metrics` serves Prometheus metrics once `WARDEN_METRICS_TOKEN` is set, read with `Authorization: Bearer <token>`. It covers requests, latency, spend, retries and errors per provider, the number of keys in each state, remaining quota per key, and egress health.

## Build from source

Requires Go 1.27.

```bash
cd gateway
go vet ./... && go test ./...
go build -o bin/warden ./cmd/warden

WARDEN_ADMIN_PASSWORD='choose-a-password' WARDEN_DATA_DIR=./data ./bin/warden serve
```

`warden healthcheck` probes the local `/healthz`; `warden version` prints the version.

## Security notes

- Upstream keys are stored in plaintext in `data/keys/*.yaml`. Keep the data directory private (the gateway sets it to mode 0700) and out of version control, backups you share, and bug reports.
- The admin password is stored as an argon2id hash; sessions and gateway tokens as SHA-256 hashes.
- The container runs as a non-root user with a read-only root filesystem and no capabilities.
- Requests for a key bound to an egress only ever leave through that egress.
- Expose the gateway to the internet only behind HTTPS, and enable two-step verification for the panel.

## License

[MIT](LICENSE). Third-party code and assets are listed in [NOTICE](NOTICE).

## Disclaimer

Search Warden is for managing API keys that you hold legitimately. Make sure your use complies with the Tavily and Exa terms of service. The project does not encourage registering accounts in bulk and cannot guarantee that accounts will not be suspended.
