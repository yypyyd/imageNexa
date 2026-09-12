<div align="center">

<img src="frontend/public/favicon.svg" width="80" alt="2API" />

# 2API

**An OpenAI-compatible API gateway for text, image, and video generation**

[简体中文](README.md) | English | [Design](DESIGN.md)

</div>

## Scope

2API is a self-hosted API service with a singleton super-administrator console. Downstream clients use one OpenAI-style `Bearer sk-*` API key and canonical model IDs; the gateway handles provider routing, account selection, concurrency, quota reservation, failover, and quota reconciliation internally.

The product boundary is intentionally small:

- Public `/v1` endpoints cover text, images, image edits, asynchronous image tasks, and videos.
- There is exactly one super administrator and no public registration or end-user web application.
- The console manages model routes, upstream accounts, API keys, logs, artifacts, banned words, and system settings.
- The model catalog is closed. Clients cannot create model IDs.
- When the same product is offered by more than one provider, each channel has its own public ID. ChatGPT's GPT Image 2 uses the unprefixed `gpt-image-2`.

## Authentication

Every `/v1` request uses the standard OpenAI Bearer header:

```http
Authorization: Bearer sk-your-api-key
```

`x-api-key`, query parameters, cookies, and custom authentication headers are not accepted. The super administrator creates and rotates API keys in the console. Plaintext is shown once; the server persists only a hash and preview. Each key may have its own concurrency limit.

The administrator console uses an HttpOnly, SameSite=Strict session cookie. Mutations also require the session-bound `X-CSRF-Token`. Administrator credentials and session secrets are never stored in browser localStorage.

Creating the administrator for the first time also requires the deployment's `ADMIN_BOOTSTRAP_TOKEN`. The initialization request sends it only in the fixed `X-Admin-Bootstrap-Token` header. Once the administrator exists, the database singleton permanently closes initialization.

## Public API

Except for health probes, every endpoint below requires `Authorization: Bearer sk-*`.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/models` | List canonical models; the default is a strict five-field OpenAI object (`shutdown_date` is `null`), and `?extended=true` opts into capability metadata |
| `POST` | `/v1/chat/completions` | Chat Completions with ordinary JSON or `stream: true` SSE |
| `POST` | `/v1/images/generations` | Text-to-image; synchronous by default, opt into async with `Prefer: respond-async` |
| `POST` | `/v1/images/edits` | `multipart/form-data` image editing with one or more reference images |
| `GET` | `/v1/images/tasks?request_id=...` | Poll an asynchronous image task or recover a result by idempotency ID |
| `GET` | `/v1/images/:id/content` | Read image content |
| `POST` | `/v1/videos` | Create an asynchronous video task |
| `GET` | `/v1/videos/:id` | Read video task status |
| `GET` | `/v1/videos/:id/content` | Download the completed video |

Unauthenticated probes:

- `GET /health/live`: process liveness.
- `GET /health/ready`: PostgreSQL, Redis, the latest database migration, and the private RustFS bucket are ready.

Errors use the OpenAI shape:

```json
{"error":{"message":"...","type":"invalid_request_error","param":"model","code":"model_not_found"}}
```

## Closed Canonical Model Catalog

`GET /v1/models` returns only these 21 public IDs. Channels that offer the same product are not merged. Oreate, Runway, and Custom are not public channels. Provider route IDs and upstream model names are invalid as API `model` values.

### Text (4)

- `gpt-5-5-mini`
- `gpt-5-5-thinking`
- `grok-4.5`
- `grok-chat-fast`

### Image (10)

- `gpt-image-2`
- `byteplus-gpt-image-2`
- `adobe-gpt-image-2`
- `seedream-5.0-pro`
- `seedream-5.0-lite`
- `byteplus-nano-banana-2`
- `adobe-nano-banana-2`
- `byteplus-nano-banana-pro`
- `adobe-nano-banana-pro`
- `grok-imagine-image`

### Video (7)

- `kling-3`
- `kling-o3`
- `adobe-seedance-2.0`
- `adobe-seedance-2.0-fast`
- `dola-seedance-2.5` (Dola-only, 30 seconds, 720p, ratios `1:1` `3:4` `4:3` `9:16` `16:9` `21:9`, up to 10 reference images)
- `grok-imagine-video`
- `firefly-video`

## Unified Routing and Account Scheduling

Clients submit the canonical model ID for the channel they want. 2API then:

1. Filters routes by operation, aspect ratio, resolution, duration, and reference-media capabilities.
2. Removes route-bound accounts that are disabled, cooling down, authentication-invalid, or known to lack enough quota. Eligible accounts currently at their concurrency limit stay at the tail so they can be used when a slot is released.
3. Selects by route priority, account weight, available concurrency, and the quota bucket used by that model. Among equivalent candidates, best-fit balance ordering avoids spending large-balance accounts on cheap work.
4. Atomically reserves account concurrency and quota before upstream submission; a definitely unaccepted request releases its reservation.
5. Re-reads or reconciles the selected account's upstream quota after every generation, updating both the console and the next scheduling decision.

Authentication failures, exhausted quota, and safely retryable pre-acceptance errors may move to another account or route. Once an upstream accepts a task, or submission outcome is uncertain, 2API does not switch accounts and resubmit. The sole account-level exception is BytePlus GPT Image 2's exact known beta execution failure after that account's one live pre-submit and three post-failure balance snapshots all remain known and unchanged. Within the same BytePlus route, 2API may then walk a bounded chain of at most six distinct accounts. Every failed account must independently pass the four-snapshot no-charge proof; the chain never returns to an attempted account, enters the ordinary 300-second temporary retry loop, or switches routes. An unknown, changed, or unavailable balance, or an ambiguous submission outcome, stops the chain immediately. Send a stable, unique `Idempotency-Key` on image and video creation requests. If an image request omits it, the server creates one for that request and returns it in the `Idempotency-Key` and `Location` headers so accepted work can be recovered. That generated key is not automatically present on a later client retry and does not replace client-side reuse.

## Importing BytePlus Accounts

BytePlus uses only a complete Lumina website Cookie as its credential. You may paste a Cookie Header or browser-export JSON containing `cookie_string` / `cookies[]`; the backend extracts, normalizes, and stores only the Cookie. Exported email, avatar, tenant, and quota fields are never trusted.

The Cookie must contain valid session data and a non-empty `csrfToken`. A standalone CSRF value, Bearer token, password, or incomplete Cookie cannot be imported. The server derives `X-Csrf-Token` from the Cookie, immediately verifies identity and real upstream quota, and creates candidate bindings for the canonical BytePlus routes. If a later request returns affirmative evidence that the account lacks one model entitlement, only that account-route binding is disabled; the account's other models remain eligible.

Treat the Cookie as a high-value secret. Never place it in logs, screenshots, documentation, `.env`, or Git.

## Quick Deployment

Requirements: Docker Engine and Docker Compose v2. The stack starts PostgreSQL, Redis, RustFS, the Go API, and the administrator SPA. HTTP port `2000` binds only to the host loopback interface and is exposed as HTTPS by a reverse proxy on the same host.

1. Create the deployment environment file:

```bash
cp .env.example .env
```

PowerShell:

```powershell
Copy-Item .env.example .env
```

2. Edit `.env` and replace every `replace-with-*` value:

| Variable | Purpose |
|---|---|
| `APP_ENV` | Must be `production` online; use `development` explicitly only for local plain-HTTP testing |
| `APP_TITLE` | Console title |
| `PUBLIC_BASE_URL` | Canonical external API URL, for example `https://api.example.com` |
| `CORS_ORIGINS` | Origins allowed to access the administrator API; comma-separated |
| `COOKIE_SECURE` | `true` behind HTTPS; `false` only for local plain-HTTP development |
| `SESSION_COOKIE_NAME` | Administrator session cookie name |
| `ADMIN_BOOTSTRAP_TOKEN` | High-entropy secret required to create the singleton administrator; at least 32 bytes and never the template value |
| `TRUSTED_PROXY_CIDRS` | Exact reverse-proxy CIDRs allowed while walking `X-Forwarded-For` from right to left |
| `POSTGRES_DB` / `POSTGRES_USER` / `POSTGRES_PASSWORD` | PostgreSQL configuration; production passwords must be at least 16 bytes and non-default |
| `REDIS_PASSWORD` | High-entropy password shared by Redis and the backend; Compose enforces Redis authentication |
| `RUSTFS_BUCKET` / `RUSTFS_ACCESS_KEY` / `RUSTFS_SECRET_KEY` | Private object storage; production access keys must be at least 16 bytes and secret keys at least 32 bytes |

Generate the initialization token with:

```bash
openssl rand -hex 32
```

PowerShell:

```powershell
[Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLower()
```

3. Validate and start the stack:

```bash
docker compose config
docker compose up -d --build
docker compose ps
```

4. Check health:

```bash
curl http://localhost:2000/health/live
curl http://localhost:2000/health/ready
```

On first access, enter the initialization token from `.env` and create the singleton super administrator. The token is not stored in the browser or database. Then create a downstream key on the API Key page and import upstream credentials on the Accounts page.

For production, place your own HTTPS reverse proxy in front of port `2000`, preserve `Host`, and append `X-Forwarded-For`. Public links use only `PUBLIC_BASE_URL=https://...`; client-supplied forwarded schemes are not trusted. Production also requires HTTPS `CORS_ORIGINS`, `COOKIE_SECURE=true`, a non-default database password, and non-template RustFS keys, otherwise the backend refuses to start. Add only the exact network of any additional proxy to `TRUSTED_PROXY_CIDRS` when the backend must resolve the original client IP. This project does not issue TLS certificates.

Common operations:

```bash
docker compose logs -f backend web
docker compose pull
docker compose up -d --build
```

## API Examples

Create an API key in the administrator console first.

```bash
# Models
curl https://api.example.com/v1/models \
  -H "Authorization: Bearer sk-your-api-key"

# Text
curl https://api.example.com/v1/chat/completions \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5-5-mini","messages":[{"role":"user","content":"Hello"}]}'

# Image
curl https://api.example.com/v1/images/generations \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: image-order-001" \
  -d '{"model":"gpt-image-2","prompt":"Minimal product photography","size":"1024x1024"}'

# Image edit
curl https://api.example.com/v1/images/edits \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Idempotency-Key: edit-order-001" \
  -F "model=byteplus-nano-banana-pro" \
  -F "prompt=Replace the background with a studio backdrop" \
  -F "image=@reference.png"

# Create a video task
curl https://api.example.com/v1/videos \
  -H "Authorization: Bearer sk-your-api-key" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: video-order-001" \
  -d '{"model":"kling-3","prompt":"Slow dolly through a neon alley","seconds":8,"size":"1280x720"}'
```

For images, `Prefer: respond-async` returns `202`; poll `/v1/images/tasks` with the returned `request_id`. Even without that preference, an upstream-accepted image that is still processing returns `202` immediately so a reverse-proxy timeout cannot hide its recovery handle. Image edits accept PNG, JPEG, GIF, or WebP references; `mask`, `background`, and `output_format` are explicitly rejected instead of silently ignored. Videos are always task-based: poll `/v1/videos/:id` until `completed`, then request `/content`.

Image/video tasks, status, and `/content` are isolated to the API key that created them. Content downloads therefore require the same Bearer key.

## Local Development and Verification

Backend:

```bash
cd backend
go vet ./...
go build ./cmd/api
```

Frontend:

```bash
cd frontend
npm ci
npm run lint:unused
npm run build
```

## Repository Layout

```text
backend/                 Go API, provider adapters, routing, and scheduling
frontend/                Vue 3 singleton administrator console
docker-compose.yml       Complete container stack
.env.example             Deployment variable template
DESIGN.md                Architecture, data model, security, and scheduling semantics
```

## License

This project is released under the [MIT License](LICENSE).
