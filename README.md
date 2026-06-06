# qoder2api

[中文文档](README_CN.md)

Go bridge that exposes Qoder through OpenAI-compatible and Anthropic-compatible local APIs.

Supported endpoints:

- `POST /v1/chat/completions` — OpenAI Chat Completions
- `POST /v1/responses` — OpenAI Responses (used by Codex CLI)
- `POST /v1/messages` — Anthropic Messages (used by Claude Code CLI)

## Quick Start

```bash
# 1. Login (only needed once or when token expires)
go run ./cmd/qoder2api-login

# 2. Start the bridge
go run ./cmd/qoder2api

# 3. Point your client at http://127.0.0.1:8963
```

## Authentication

### CLI Login (Recommended)

```bash
go run ./cmd/qoder2api-login
```

Opens the browser for Qoder authorization. Token is saved to `~/.config/qoder2api/auth.json`. After that, start the bridge directly without setting any environment variables.

Custom output path:

```bash
go run ./cmd/qoder2api-login -o /path/to/auth.json
```

### QODER_AUTH_JSON

Compatibility mode using auth JSON exported from the Qoder client:

```bash
QODER_AUTH_JSON=/absolute/path/to/auth.json go run ./cmd/qoder2api
```

Accepted JSON shapes:

- Root object containing `securityOauthToken` and `refreshToken`
- Root object containing `auth_user_info_raw`
- Array whose first item contains `auth_user_info_raw`

### QODER_PAT

Cloud Agents API mode:

```bash
QODER_PAT=your_pat go run ./cmd/qoder2api
```

## Run

Default bind:

```text
http://127.0.0.1:8963
```

Optional environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `QODER_HOST` | Listen address | `127.0.0.1` |
| `QODER_PORT` | Listen port | `8963` |
| `QODER_MODEL` | Default model | `auto` |
| `QODER_TIMEOUT_SEC` | Request timeout (seconds) | `300` |
| `QODER_BASE_URL` | Qoder API base URL | `https://api.qoder.com` |
| `QODER_WORKSPACE_ROOT` | Workspace path | empty |
| `QODER_PROXY_URL` | HTTP proxy | empty |
| `QODER_CA_FILE` | Custom CA certificate | empty |
| `QODER_INSECURE_SKIP_VERIFY` | Skip TLS verification | `false` |

## Docker

PAT mode:

```bash
QODER_PAT=your_pat docker compose up --build
```

Auth JSON mode:

```bash
docker build -t qoder2api:local .
docker run --rm -p 8963:8963 \
  -e QODER_HOST=0.0.0.0 \
  -e QODER_AUTH_JSON=/auth.json \
  -v /absolute/path/to/auth.json:/auth.json:ro \
  qoder2api:local
```

## Client Configuration

### Codex CLI

Codex uses the Responses API with a base URL ending in `/v1`.

Edit `~/.codex/config.toml`:

```toml
[model_providers.custom]
name = "custom"
wire_api = "responses"
base_url = "http://127.0.0.1:8963/v1"
```

### Claude Code CLI

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8963 ANTHROPIC_API_KEY=test-key claude
```

If settings override the base URL, use `--setting-sources local`.

## Compatibility Notes

- `QODER_AUTH_JSON` uses a compatibility backend derived from official Qoder client traffic. Upstream client/API changes can require updates.
- For `QODER_AUTH_JSON`, prefer `QODER_MODEL=auto` unless you have verified a specific model key.
- Anthropic `thinking` blocks are only exposed when the request explicitly enables Anthropic `thinking`.
- For legacy image streaming without explicit client tools, `/v1/messages` uses a compatibility fallback: it aggregates the upstream result and emits valid Anthropic SSE.

## Development

```bash
go test ./...
go build -o qoder2api ./cmd/qoder2api
```

## Disclaimer

This project is for **educational and research purposes only**. Use at your own risk. The author is not responsible for any consequences arising from the use of this software, including but not limited to account suspension, data loss, or legal disputes.

Do not use this project for any commercial purpose or in any way that violates the terms of service of Qoder or any third-party service.

Auth JSON files and token files contain live credentials. **Never commit them to version control.**

## License

[MIT](LICENSE)
