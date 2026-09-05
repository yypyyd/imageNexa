# 2API Backend

Go data plane and singleton-administrator control plane for 2API. The public API exposes only the canonical text, image, image-edit, and video endpoints documented in the repository [README](../README.md). Provider names, upstream model IDs, account credentials, and account selection stay internal.

## Security contract

- Every `/v1` request requires `Authorization: Bearer sk-*`; alternate headers, cookies, and query credentials are rejected.
- Image and video task/content reads are scoped to the API credential that created the event.
- The control plane has exactly one administrator and uses an HttpOnly, SameSite=Strict session cookie plus CSRF protection.
- First initialization also requires `X-Admin-Bootstrap-Token`, matching the deployment-only `ADMIN_BOOTSTRAP_TOKEN`.
- PostgreSQL stores downstream API-key hashes and durable routing/quota state. Redis stores short-lived concurrency/session state. RustFS stores private generated artifacts.
- The production backend container runs as an unprivileged user. Docker's default seccomp profile prevents Chromium user namespaces, so the Oreate signer uses `--no-sandbox`; its empty environment, ephemeral profile, parent-death/process-group teardown, and restricted provider-only use are mandatory compensating controls.

## Local dependencies

The supported full-stack deployment is `docker compose` from the repository root. To run only the backend during development, start PostgreSQL, Redis, and RustFS, then copy and edit the local template:

```powershell
Copy-Item .env.example .env
go run ./cmd/api
```

Important variables are:

- `POSTGRES_DSN`, `REDIS_ADDR`
- `RUSTFS_ENDPOINT`, `RUSTFS_BUCKET`, `RUSTFS_ACCESS_KEY`, `RUSTFS_SECRET_KEY`
- `ADMIN_BOOTSTRAP_TOKEN`
- `PUBLIC_BASE_URL`, `CORS_ORIGINS`, `COOKIE_SECURE`
- `TRUSTED_PROXY_CIDRS` for reverse proxies that append `X-Forwarded-For`

Never commit the real `.env` or any provider credential. The backend imports BytePlus only from a complete Lumina Cookie header (or a browser export containing `cookie_string`, `cookie_header`, or `cookies[]`).

## Database lifecycle

Forward-only, checksummed migrations create administrator/API-credential identity, the 19-model canonical catalog, model routes, account-route entitlements, quota buckets/reservations, dispatch attempts, and API-key-attributed events. Provider-account deletion cascades through its route bindings, quota buckets, and bucket-owned reservations, while event logs and nullable dispatch history remain. Startup refuses unknown or modified applied migrations. `AutoMigrate` is limited to compatible columns on retained operational tables and does not seed retired models.

## Verification

```powershell
go test ./...
go vet ./...
go build ./cmd/api
```

Operational probes are `GET /health/live` and `GET /health/ready`. Readiness requires PostgreSQL, Redis, the latest migration, and access to the configured private RustFS bucket.
