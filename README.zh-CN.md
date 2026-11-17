<div align="center">

# Search Warden

**自托管的 Tavily + Exa API key 网关**

把多个 key 收拢到一个入口后面，照常使用官方的 MCP 工具和 SDK，
让每个 key 走各自的代理出口，并在管理面板里看清每一次请求。

[![CI](https://github.com/sodiseng/search-warden/actions/workflows/ci.yml/badge.svg)](https://github.com/sodiseng/search-warden/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
![Go 1.27](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)
![Docker](https://img.shields.io/badge/Docker-compose-2496ED?logo=docker&logoColor=white)

[English](README.md) | **简体中文**

</div>

---

## 目录

- [功能](#功能)
- [工作方式](#工作方式)
- [快速开始](#快速开始)
- [Docker Compose](#docker-compose)
- [配置](#配置)
- [接入客户端](#接入客户端)
- [管理面板](#管理面板)
- [反向代理与 HTTPS](#反向代理与-https)
- [数据、备份与升级](#数据备份与升级)
- [监控](#监控)
- [预编译二进制](#预编译二进制)
- [从源码构建](#从源码构建)
- [安全说明](#安全说明)
- [许可证](#许可证)
- [免责声明](#免责声明)

## 功能

- **直接替换官方 MCP。** `/tavily/mcp`、`/exa/mcp` 和合并的 `/mcp` 提供与 Tavily、Exa 官方 MCP 服务相同的工具、名称和参数。客户端里只需要改地址和 key。
- **REST 透传。** 把 SDK 的 base URL 指向 `/tavily` 或 `/exa`，其余代码不用动。
- **key 池。** 免费 key 优先、付费 key 兜底，按剩余额度均衡，可以设置权重。某个 key 出错时自动跳过，换一个 key 重试。
- **每个 key 一个出口。** 可以给 key 绑定 SOCKS5 或 HTTP 代理。代理不可用时这个 key 会暂停，流量绝不回退为直连。
- **额度跟踪。** Tavily 的用量通过官方 `/usage` 接口同步；Exa 按每次响应里报告的费用累计。
- **网关令牌。** 给每个 agent 或同事单独发令牌，可以限制服务、入口、工具、key 分组、来源 IP 和有效期，并设置每分钟、并发、每天和每月的限额。
- **管理面板。** 仪表盘、实时请求监控（能看到来源 IP 和实际使用的 key）、Keys、出口、令牌、设置和审计日志。支持中文和英文、浅色和深色。
- **以文件为准。** 所有配置都是数据目录里的 YAML 文件，与面板双向同步。
- **占用小。** 一个静态 Go 二进制，默认 SQLite，可选 PostgreSQL。

## 工作方式

```text
 Agent 和 SDK                       Search Warden                         上游
┌──────────────────┐  网关令牌  ┌──────────────────────────┐  你的 key   ┌────────────────┐
│ MCP 客户端       │──────────▶│ 鉴权与限额               │────────────▶│ api.tavily.com │
│ Tavily / Exa SDK │           │ key 池、重试、额度       │  按 key 的  │ api.exa.ai     │
└──────────────────┘           │ 请求日志、管理面板       │  出口       └────────────────┘
                               └──────────────────────────┘
```

客户端用**网关令牌**认证。网关从你的**上游 key** 里选一个，换上它的凭据，经这个 key 的出口发出请求，并记录这次请求的花费。

## 快速开始

需要：Docker 和 Compose 插件。

```bash
git clone https://github.com/sodiseng/search-warden.git
cd search-warden

cp .env.example .env            # 然后设置 WARDEN_ADMIN_PASSWORD（至少 12 个字符）
mkdir -p data && sudo chown -R 65532:65532 data

docker compose up -d --build
```

打开 `http://127.0.0.1:18683/admin/`，用 `admin` 和你设置的密码登录。然后：

1. **出口**（可选）：导入代理节点。不导入时，key 直接连接上游。
2. **Keys**：添加 Tavily 和 Exa 的 key，每行一个，格式为 `key[,名称][,出口][,套餐]`。点**测试**确认可用。
3. **令牌**：创建网关令牌。令牌只显示一次。
4. 把客户端指向网关：见[接入客户端](#接入客户端)。

## Docker Compose

仓库里与 Compose 有关的文件有三个：

| 文件 | 用途 |
|---|---|
| [`docker-compose.yml`](docker-compose.yml) | 服务定义。每一项设置都列出并写了注释，可选项标有 `[optional]`。 |
| [`.env.example`](.env.example) | `.env` 的模板。只有 `WARDEN_ADMIN_PASSWORD` 必须填写；其余每一行都是注释掉的默认值。 |
| [`Dockerfile`](Dockerfile) | 构建镜像：静态二进制加 distroless 基础镜像，以非 root 用户运行。 |

这两个模板文件里的注释是英文的，每个变量的中文说明见下面的[配置](#配置)。

### 常用命令

| 操作 | 命令 |
|---|---|
| 启动（默认 SQLite） | `docker compose up -d --build` |
| 用 PostgreSQL 启动 | `docker compose --profile postgres up -d --build` |
| 查看日志 | `docker compose logs -f warden` |
| 查看健康状态 | `docker compose ps` |
| 停止 | `docker compose down` |
| 升级 | `git pull && docker compose up -d --build` |

### 使用 PostgreSQL

SQLite 不需要任何准备，小团队足够用。要改用 PostgreSQL，在 `.env` 里设置下面两行（两处密码相同），并用 `postgres` profile 启动：

```dotenv
PG_PASSWORD=choose-a-password
WARDEN_DB=postgres://warden:choose-a-password@postgres:5432/warden?sslmode=disable
```

### 两个容易踩的点

- **`.env` 里的注释要单独占一行。** Compose 会把 `NAME=   # 说明` 读成值 `# 说明`。
- **数据目录必须能被容器用户（uid 65532）写入。** 执行快速开始里的 `chown`，或者在 `.env` 里把 `WARDEN_UID` 和 `WARDEN_GID` 设成你自己的 `id -u` 和 `id -g`。

### 预构建镜像

CI 在每次推送到 `main` 时发布 `ghcr.io/sodiseng/search-warden:edge`，打发布标签时发布 `:<版本号>` 和 `:latest`。要直接运行镜像而不是自己构建，在 `.env` 里设置 `WARDEN_VERSION`，启动时去掉 `--build`。

## 配置

配置分两层：

- **启动参数**是环境变量，写在 `.env` 里，见下表。
- **其余所有配置**（key、出口、令牌、路由、限额、日志保留期）都在数据目录的 YAML 文件里，可以在面板修改，也可以直接编辑。见[数据、备份与升级](#数据备份与升级)。

### 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `WARDEN_ADMIN_PASSWORD` | *（无，首次必填）* | 面板用户 `admin` 的密码，至少 12 个字符。只在 `config.yaml` 里还没有管理员密码时读取，保存为 argon2id 哈希。 |
| `WARDEN_DB` | `sqlite:///data/db` | `sqlite://<目录>` 或 `postgres://user:pass@host:5432/db`。 |
| `WARDEN_ADMIN_LISTEN` | `shared` | `shared` 表示面板与网关共用端口，整个服务只有一个端口。面板的独立端口**默认不开启**；设为 `:18684` 才会启用。 |
| `WARDEN_ADMIN_BASE_PATH` | `/admin/` | 面板挂载的路径。面板与网关共用端口时不能是 `/`。 |
| `WARDEN_PUBLIC_URL` | *（空）* | 网关的对外地址。只用于面板里展示的客户端配置示例。 |
| `WARDEN_ADMIN_PUBLIC_URL` | *（空）* | 面板的对外地址。只接受来自这个来源的登录和修改。 |
| `WARDEN_COOKIE_SECURE` | `false` | 面板通过 HTTPS 访问时设为 `true`。 |
| `WARDEN_METRICS_TOKEN` | *（空）* | 设置后开启 `GET /metrics`。至少 16 个字符。 |
| `WARDEN_LOG_LEVEL` | `info` | `debug`、`info`、`warn` 或 `error`。 |
| `WARDEN_LOG_FORMAT` | `json` | `json` 或 `text`。 |
| `WARDEN_LISTEN` | `:18683` | 网关的监听地址。容器内固定，要改端口请改宿主机一侧。 |
| `WARDEN_DATA_DIR` | 镜像内为 `/data`，否则为 `./data` | 数据目录。 |

只给 Compose 用的变量（由 `docker-compose.yml` 使用，网关本身不读取）：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `WARDEN_IMAGE`、`WARDEN_VERSION` | `ghcr.io/sodiseng/search-warden`、`latest` | 镜像名和标签。 |
| `WARDEN_UID`、`WARDEN_GID` | `65532` | 容器的运行用户，也是 `./data` 里文件的属主。 |
| `WARDEN_GATEWAY_BIND`、`WARDEN_GATEWAY_PORT` | `0.0.0.0`、`18683` | 网关在宿主机上的地址和端口。 |
| `WARDEN_ADMIN_BIND`、`WARDEN_ADMIN_PORT` | `127.0.0.1`、`18684` | 面板独立端口在宿主机上的地址和端口。**默认不开启**：只有设置了 `WARDEN_ADMIN_LISTEN=:18684`，并取消 `docker-compose.yml` 里第二条端口映射的注释后才起作用。 |
| `PG_PASSWORD` | `change-me` | 自带 PostgreSQL 的密码。 |

## 接入客户端

把 `gw.example.com` 换成你的网关地址，把 `<令牌>` 换成网关令牌。

### MCP

| 用途 | 地址 | 令牌放在 |
|---|---|---|
| Tavily 工具 | `https://gw.example.com/tavily/mcp` | `Authorization: Bearer <令牌>`，或 `?tavilyApiKey=<令牌>` |
| Exa 工具 | `https://gw.example.com/exa/mcp` | `x-api-key: <令牌>`，或 `?exaApiKey=<令牌>` |
| 两家合并为一个服务 | `https://gw.example.com/mcp` | `Authorization: Bearer <令牌>` |

传输方式是 Streamable HTTP。以 Claude Code 为例：

```bash
claude mcp add --transport http search https://gw.example.com/mcp \
  --header "Authorization: Bearer <令牌>"
```

或者写成 JSON 配置：

```json
{
  "mcpServers": {
    "search": {
      "type": "http",
      "url": "https://gw.example.com/mcp",
      "headers": { "Authorization": "Bearer <令牌>" }
    }
  }
}
```

可用的工具：

| 服务 | 工具 |
|---|---|
| Tavily | `tavily_search`、`tavily_extract`、`tavily_crawl`、`tavily_map`、`tavily_research` |
| Exa | `web_search_exa`、`web_fetch_exa`、`agent_run`；`web_search_advanced_exa` 需要在 `?tools=` 里列出 |

在 `/mcp` 上，可以用 `?providers=tavily,exa` 和 `?tools=<名称>,<名称>` 缩小工具列表。令牌自身的范围会再筛一次。

### REST

把 SDK 的 base URL 设为 `https://gw.example.com/tavily` 或 `https://gw.example.com/exa`，API key 填网关令牌。

```bash
# Tavily
curl https://gw.example.com/tavily/search \
  -H "Authorization: Bearer <令牌>" -H "Content-Type: application/json" \
  -d '{"query": "what is MCP", "max_results": 3}'

# Exa
curl https://gw.example.com/exa/search \
  -H "x-api-key: <令牌>" -H "Content-Type: application/json" \
  -d '{"query": "what is MCP", "numResults": 3}'
```

只转发下面这些路径，其他路径返回 404：

| 服务 | 路径 |
|---|---|
| Tavily | `POST /search`、`/extract`、`/crawl`、`/map`、`/research`；`GET /research/{id}`；`POST /feedback`；`GET /usage`（由网关按自己的统计回答） |
| Exa | `POST /search`、`/contents`、`/findSimilar`、`/answer`、`/research/v1`；`GET /research/v1/{id}`；`POST /agent/runs`；`GET /agent/runs/{id}`；`POST /agent/runs/{id}/cancel` |

## 管理面板

面板在网关端口的 `/admin/` 下。

| 页面 | 用途 |
|---|---|
| 概览 | 请求量、花费、错误，以及每个 key 的状态和剩余额度。 |
| 请求 | 每个请求的来源 IP、令牌、key、出口、状态和花费。可以实时刷新。 |
| Keys | 添加、编辑、测试、暂停和删除上游 key，给它们绑定出口。 |
| 出口 | 导入、测试、重命名和删除代理节点，查看每个节点的健康状态和出口 IP。 |
| 令牌 | 创建网关令牌，设置范围和限额，轮换或停用。 |
| 设置 | 路由策略、重试、额度同步、日志保留期、可信代理。 |
| 文件 | 直接查看和编辑 YAML 文件，或打包下载。 |
| 安全 | 修改密码、开启两步验证（TOTP）、管理会话、查看审计日志。 |

忘记密码时，在 `data/config.yaml` 的 `admin:` 下面加一行 `password: <新密码>`。网关会在一秒内把它改写成哈希，并让所有已登录的会话失效。

## 反向代理与 HTTPS

只要不是纯本机使用，就应该把网关放在负责 TLS 的反向代理后面，并在 `.env` 里设置：

```dotenv
WARDEN_GATEWAY_BIND=127.0.0.1
WARDEN_PUBLIC_URL=https://gw.example.com
WARDEN_ADMIN_PUBLIC_URL=https://gw.example.com
WARDEN_COOKIE_SECURE=true
```

`WARDEN_GATEWAY_BIND=127.0.0.1` 很重要：不设的话，别人可以绕过反向代理，用服务器 IP 加端口通过明文 HTTP 直接访问。

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

需要知道的几点：

- **长请求。** MCP 工具调用以流的方式返回，`tavily_research` 最长会运行 15 分钟。请像上面那样关闭响应缓冲，并把读超时设到 20 分钟。Cloudflare 免费版会在 100 秒时切断经它代理的请求。
- **客户端 IP。** 网关默认信任来自本机和私有网段的 `X-Forwarded-For`，所以同机或同一 Docker 网络里的反向代理不需要额外配置。其他情况请修改 `config.yaml` 里的 `network.trusted_proxies`。
- **URL 里的令牌。** `?tavilyApiKey=` 和 `?exaApiKey=` 的值就是令牌。网关自己的日志会把它们脱敏；请确认反向代理的访问日志也做了脱敏，或者改用请求头传令牌。
- **直接打开域名返回 403。** 访问 `https://gw.example.com/` 不显示任何内容，这是有意的。确认服务是否正常请用 `/healthz`。

## 数据、备份与升级

所有内容都在数据目录里（用 Compose 时是 `./data`）：

| 路径 | 内容 |
|---|---|
| `config.yaml` | 管理员账号、路由、额度同步、日志保留期、可信代理。 |
| `keys/tavily.yaml`、`keys/exa.yaml` | 上游 key，**明文保存**。 |
| `egresses.yaml` | 代理节点。 |
| `tokens.yaml` | 网关令牌，只保存 SHA-256 哈希。 |
| `db/` | SQLite 文件：key 状态、会话、审计日志、请求日志。 |

这些文件在首次启动时由模板生成（模板里的注释是中文）。可以直接编辑，也可以在面板里修改：保存后约一秒生效；文件写错时会被忽略，网关继续使用上一个正确的版本，面板会显示出错的行号。

- **备份。** 停止容器后复制 `data/`；不停机时执行 `sqlite3 data/db/state.db ".backup state.bak"`。使用 PostgreSQL 时用 `pg_dump`，并复制 YAML 文件。
- **升级。** `git pull && docker compose up -d --build`。数据库迁移在启动时自动执行。

## 监控

- `GET /healthz` 返回 `{"status":"ok"}`。某个配置文件有无法生效的修改时返回 `degraded`，状态码仍是 200。
- 设置 `WARDEN_METRICS_TOKEN` 后，`GET /metrics` 提供 Prometheus 指标，用 `Authorization: Bearer <token>` 读取。内容包括按服务区分的请求数、耗时、花费、重试和错误，各状态的 key 数量，每个 key 的剩余额度，以及出口是否可用。

## 预编译二进制

不想用 Docker 时，可以从[发布页](https://github.com/sodiseng/search-warden/releases)下载网关。它是一个独立的可执行文件，没有任何运行时依赖。

| 系统 | x86-64（amd64） | ARM64 |
|---|---|---|
| Linux | `search-warden_linux_amd64.tar.gz` | `search-warden_linux_arm64.tar.gz` |
| macOS | `search-warden_darwin_amd64.tar.gz`（Intel） | `search-warden_darwin_arm64.tar.gz`（Apple 芯片） |
| Windows | `search-warden_windows_amd64.zip` | `search-warden_windows_arm64.zip` |

每个压缩包解开后是一个目录，里面有 `warden`（Windows 上是 `warden.exe`）、许可证文件和 README。同一个发布里的 `SHA256SUMS.txt` 列出了校验值。

Linux 和 macOS：

```bash
# x86-64 的 Linux；其他平台请换成对应的文件名
curl -LO https://github.com/sodiseng/search-warden/releases/latest/download/search-warden_linux_amd64.tar.gz
tar -xzf search-warden_linux_amd64.tar.gz
cd search-warden_linux_amd64

WARDEN_ADMIN_PASSWORD='choose-a-password' ./warden serve
```

Windows（PowerShell），解压 zip 之后：

```powershell
cd search-warden_windows_amd64
$env:WARDEN_ADMIN_PASSWORD = 'choose-a-password'
.\warden.exe serve
```

然后打开 `http://127.0.0.1:18683/admin/`。需要知道的几点：

- 网关把文件放在 `./data` 里，相对于启动它时所在的目录。要放到别处，设置 `WARDEN_DATA_DIR`。
- 它监听所有网卡的 18683 端口。只接受本机连接时，设置 `WARDEN_LISTEN=127.0.0.1:18683`。
- 其余设置与 Docker 方式相同，都是那些[环境变量](#环境变量)。`WARDEN_ADMIN_PASSWORD` 只在首次启动时需要。
- 这些二进制没有签名。macOS 可能拒绝运行用浏览器下载的文件，用 `xattr -d com.apple.quarantine ./warden` 去掉隔离标记即可。

## 从源码构建

需要 Git 和 Go 1.27 或更新的版本，除此之外什么都不用装：构建不需要 C 编译器，面板的资源已经嵌在二进制里，没有前端构建步骤。

**1. 获取源码**

```bash
git clone https://github.com/sodiseng/search-warden.git
cd search-warden/gateway
```

**2. 运行检查**（可选）

```bash
go vet ./... && go test ./...
```

PostgreSQL 的测试默认跳过，除非用 `WARDEN_TEST_PG_DSN` 指定一个测试数据库。

**3. 编译**

```bash
go build -o bin/warden ./cmd/warden        # Windows 上用 -o bin/warden.exe
```

第一次编译会下载 Go 模块。这样编出来的二进制，版本号显示为 `dev`。

**4. 按发布版的方式编译**（可选）

写入版本号、去掉调试信息，并生成静态链接的二进制：

```bash
PKG=github.com/sodiseng/search-warden/gateway/internal/buildinfo
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X $PKG.Version=$(git describe --tags --always) -X $PKG.Commit=$(git rev-parse HEAD)" \
  -o bin/warden ./cmd/warden

./bin/warden version
```

要为其他平台编译，设置 `GOOS`（`linux`、`darwin` 或 `windows`）和 `GOARCH`（`amd64` 或 `arm64`）：

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bin/warden-linux-arm64 ./cmd/warden
```

**5. 运行**

```bash
WARDEN_ADMIN_PASSWORD='choose-a-password' WARDEN_DATA_DIR=./data ./bin/warden serve
```

`warden healthcheck` 检查本机的 `/healthz`；`warden version` 输出版本号。

**Docker 镜像。** 在仓库根目录执行：

```bash
docker build -t search-warden .
# 同时构建两个架构：
docker buildx build --platform linux/amd64,linux/arm64 -t search-warden .
```

## 安全说明

- 上游 key 以明文保存在 `data/keys/*.yaml` 里。请保护好数据目录（网关会把它的权限设为 0700），不要把它提交到版本库，也不要放进对外分享的备份或问题报告里。
- 管理员密码保存为 argon2id 哈希，会话和网关令牌保存为 SHA-256 哈希。
- 容器以非 root 用户运行，根文件系统只读，不带任何 capability。
- 绑定了出口的 key，它的请求只会从这个出口发出。
- 对公网开放时，请务必放在 HTTPS 后面，并为面板开启两步验证。

## 许可证

[MIT](LICENSE)。用到的第三方代码和资源列在 [NOTICE](NOTICE) 里。

## 免责声明

Search Warden 用于管理你合法持有的 API key。请确认你的用法符合 Tavily 和 Exa 的服务条款。本项目不鼓励批量注册账号，也不能保证账号不会被封禁。
