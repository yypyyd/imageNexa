# 2API Design

Status: locked product architecture
Last updated: 2026-09-01

## 1. Goals and boundaries

2API is an OpenAI-compatible data plane plus a singleton super-administrator control plane. It exposes text, image, image-edit, and video generation while hiding provider-specific protocols, credentials, model names, and account selection.

The design has five fixed goals:

1. One downstream authentication scheme: `Authorization: Bearer sk-*`.
2. One closed set of canonical model IDs shared by every provider route.
3. Model-aware, quota-aware scheduling across multiple upstream accounts.
4. Durable dispatch and quota state that remains correct under concurrency and ambiguous upstream outcomes.
5. One super administrator with the smallest practical web control surface.

There is no public application role. The browser UI is an administrator console only. Provider adapters may evolve, but they cannot enlarge the public model catalog or introduce provider-prefixed API IDs without an explicit product migration.

## 2. System context

```text
OpenAI-compatible client
        |
        | Bearer sk-*
        v
  /v1 data plane -------> canonical model resolver
        |                         |
        |                         v
        |                  model route matcher
        |                         |
        |                         v
        +----------------> quota-aware account scheduler
                                  |
                                  v
                         provider protocol adapters

Administrator browser
        |
        | HttpOnly session + CSRF
        v
 /admin/api control plane ----> models / routes / accounts / keys / logs

PostgreSQL: durable configuration, dispatch, quota, and audit state
Redis: concurrency and short-lived coordination
RustFS: private generated artifacts
Nginx: SPA, /v1 proxy, health proxy, and long-response streaming
```

The data plane and control plane share application services and durable state, but use different authentication and CORS policies.

## 3. External HTTP contract

### 3.1 Authentication

Every `/v1` route is behind API credential middleware. The only accepted credential form is:

```http
Authorization: Bearer sk-your-api-key
```

The middleware authenticates once, stores the resolved API credential on the request context, updates last-use metadata, and applies the credential's concurrency limit. Handlers reuse that principal instead of re-reading the token.

Plaintext keys are returned only on create or rotate. PostgreSQL stores the hash, a safe preview, status (`active`, `disabled`, or `revoked`), and concurrency limit. Revocation is terminal; rotation creates a replacement secret and invalidates the previous one.

### 3.2 Public endpoints

| Method | Path | Contract |
|---|---|---|
| `GET` | `/v1/models` | Closed canonical catalog. The default returns `id`, `object`, `created`, `owned_by`, and `shutdown_date` (`null` in 2API); `extended=true` opts into capability metadata. |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions JSON and SSE. |
| `POST` | `/v1/images/generations` | Synchronous image generation by default; async is opt-in with `Prefer: respond-async` or `async=true`. |
| `POST` | `/v1/images/edits` | Multipart image editing with PNG/JPEG/GIF/WebP reference images; unsupported mask semantics are rejected. |
| `GET` | `/v1/images/tasks` | API-key-scoped image task lookup by `request_id`. |
| `GET` | `/v1/images/:id/content` | API-key-authenticated image content. |
| `POST` | `/v1/videos` | Create an asynchronous video task. |
| `GET` | `/v1/videos/:id` | Read video task state. |
| `GET` | `/v1/videos/:id/content` | API-key-authenticated video content. |

`GET /health/live` and `GET /health/ready` are unauthenticated operational probes outside `/v1`.

Every error exposed by `/v1` uses the OpenAI error envelope. Provider-specific response bodies, account identifiers, route IDs, and credentials are not part of the public contract.

### 3.3 Idempotency and acceptance boundary

Image and video creation accept `Idempotency-Key`. The key is scoped to the API credential and request kind. Reusing a key with a different request fingerprint is a conflict. Every public image request has a durable request ID: when the client omits the header, the gateway creates one and exposes it in `Idempotency-Key` and `Location` response headers. This makes an accepted task recoverable, while reliable cross-request deduplication still requires the client to resend a stable key.

Failover is allowed only before a provider has accepted the generation. After an upstream task ID or equivalent acknowledgement exists, polling and artifact retrieval remain pinned to that attempt. A synchronous request that crosses this boundary but has no final artifact returns `202` immediately with the same durable task payload and recovery headers; it never waits beyond the reverse proxy's timeout. If the submission outcome is ambiguous, the reservation and dispatch are marked uncertain and the request is never replayed through another account or route. Avoiding duplicate upstream work is more important than optimistic retry after the acceptance boundary.

## 4. Canonical model catalog

The catalog is versioned data, not a user-extensible namespace. Startup settings do not recreate removed aliases or unknown models. `/v1/models`, request validation, the administrator console, route bindings, and tests use the same canonical IDs.

### Text

`gpt-5-5-mini`, `gpt-5-5-thinking`, `grok-4.5`, `grok-chat-fast`

### Image

`gpt-image-2`, `seedream-5.0-pro`, `seedream-5.0-lite`, `nano-banana-2`, `nano-banana-pro`, `grok-imagine-image`

### Video

`veo-3.1`, `veo-3.1-lite`, `kling-3`, `kling-o3`, `runway-gen-4.5`, `runway-gen-4-turbo`, `seedance-2.0`, `seedance-2.0-fast`, `seedance-2.0-mini`, `seedance-1.5-pro`, `seedance-2.5`, `grok-imagine-video`, `luma-ray`, `firefly-video`

A provider implementation is represented by a `ModelRoute`, not by another public model. For example, `gpt-image-2` may have ChatGPT, BytePlus, and Adobe routes, but clients always request `gpt-image-2`. Likewise, multiple providers can serve `nano-banana-2` without introducing prefixed aliases.

## 5. Routing data model

### `LogicalModel`

The only model identity visible to downstream clients. It stores canonical ID, kind, display name, enabled state, ordering weight, and generation count.

### `ModelRoute`

Maps one logical model to one provider adapter and upstream model. A route contains:

- enabled state, priority, and weight;
- provider, runtime adapter selector, and upstream model ID;
- one quota-bucket key and model-specific cost rules;
- complete capability profiles for operation, ratio, resolution, duration, reference limits, reference mode, and generated audio.

Capability matching evaluates one complete profile at a time. The system must not union individual fields across profiles because that can invent unsupported combinations.

### `ProviderAccount`

The durable upstream account record. The existing adapter-facing Go type name may remain `TokenAccount`, but the product concept and database table are provider accounts. It stores private credential material, status, health counters, display identity, weight, concurrency, and provider metadata.

### `AccountModelRoute`

Authorizes one account for one route. It stores entitlement, enabled state, route-specific quota bucket, cooldown, consecutive failures, and success/failure timestamps. Disabling an account for one route does not remove it from unrelated models.

### `AccountQuotaBucket`

Durable schedulable upstream allowance for one account and bucket. `remaining` excludes `reserved`; both values are changed under a row lock. A null balance means unknown, not zero. `revision` and `refreshed_at` make stale snapshots observable.

### `QuotaReservation`

Records quota held for one event/dispatch attempt. State is `held`, `settled`, `released`, or `uncertain`. A definite pre-acceptance failure releases the hold; accepted work settles it; ambiguous work remains conservative until reconciliation.

### `DispatchAttempt`

Records route, account, upstream task ID, timestamps, state, failure class, and sanitized error for every attempt. Valid lifecycle states are `created`, `submitting`, `accepted`, `unknown`, `succeeded`, and `failed`.

### `APICredential` and `Admin`

`APICredential` is the downstream security principal. `Admin` is singleton-constrained in the database, so a second administrator cannot be created by a race or direct repository call.

## 6. Route selection

For each request, the scheduler performs these steps in order:

1. Validate that `model` is an enabled canonical ID of the requested kind.
2. Normalize request dimensions, ratio, duration, operation, reference counts, and generated-audio requirement.
3. Query enabled routes for the logical model and retain only routes whose single capability profile satisfies the complete requirement set.
4. Order routes by configured priority and weight.
5. For each route, load only accounts with an enabled and entitled `AccountModelRoute` binding.
6. Remove accounts that are disabled, authentication-invalid, cooling down, at their concurrency limit, or known to have less than the model's required quota.
7. Within the remaining group, apply account weight, available concurrency, health, and quota best-fit ordering. Round-robin state breaks equivalent choices without starving peers.
8. Atomically acquire concurrency and create the dispatch/quota reservations before provider submission.

Provider names are never client-selectable. A Custom provider account may implement an existing canonical route, but it must bind at least one catalog model and cannot create a new public ID.

## 7. Model-aware quota scheduling

Quota is scoped by account and route bucket rather than one provider-wide status flag. Different models on the same account can consume different buckets or different costs from one bucket.

Before submission:

- derive the exact normalized model cost from route cost rules;
- reject known insufficient balances without touching the provider;
- reserve the amount transactionally and increment the account's in-flight usage;
- permit an unknown balance while still recording a reservation, so the first later snapshot cannot erase concurrent work.

After every generation, 2API refreshes or reconciles the selected account's upstream quota. Reconciliation releases only that request's reservation and writes `upstream remaining - other active reservations`. Concurrent completions therefore cannot overwrite each other's holds.

Outcome rules:

| Outcome | Reservation | Account/route action | Failover |
|---|---|---|---|
| Request or capability error | release if created | no health penalty | no |
| Definite pre-acceptance temporary error | release | cooldown/failure counter as configured | safe route/account retry allowed |
| Authentication failure | release | disable affected credential or binding | allowed |
| Quota exhausted | settle/reconcile actual balance | remove affected account/bucket from eligible set | allowed |
| Accepted and completed | settle with fresh balance | update success and quota snapshot | no further route |
| Accepted then polling/download error | settle conservatively and refresh | keep original task/account | never resubmit |
| Submission outcome unknown | mark uncertain | schedule reconciliation | never resubmit |

The administrator may explicitly refresh an account. Import also performs identity and quota validation before returning success. Background maintenance can refresh due accounts, but it supplements rather than replaces per-generation reconciliation.

## 8. Provider account lifecycle

Supported control-plane provider types are ChatGPT, BytePlus, Adobe, Runway, Grok, OreateAI, and Custom. Importing a credential is an administrator-only action.

General lifecycle:

1. Normalize only the provider's credential fields.
2. Create or update the provider account without exposing the secret in the response.
3. Query identity where the provider supports it.
4. Create candidate account-route bindings from the provider's canonical route set. Providers generally do not expose a reliable zero-cost entitlement discovery API; an affirmative model-entitlement failure disables only the affected binding.
5. Fetch real upstream quota and populate quota buckets.
6. Mark explicit authentication failure as unusable; keep generic provider failures non-destructive.

Destructive account health transitions require affirmative provider-specific evidence. A generic HTTP 403, transport failure, or provider-wide challenge is not enough to declare every credential invalid.

### BytePlus credential rule

BytePlus accepts only the complete Lumina website Cookie. The administrator may submit a raw Cookie Header or a structured browser export; `cookie_string`, `cookie_header`, or `cookies[]` are reduced to one normalized Cookie before persistence.

The Cookie must include a non-empty `csrfToken` and valid session data. A standalone CSRF token is insufficient. The server derives `X-Csrf-Token` from the same Cookie for write requests. Profile, avatar, tenant, cached quota, and other exported metadata are ignored and fetched again from BytePlus.

The Cookie is stored only in private account credential storage. It must never appear in event logs, model discovery, content URLs, errors, or administrator list responses.

## 9. Administrator control plane

The administrator lifecycle has three public session actions under `/admin/api/auth`: status, one-time initialization, and login. Initialization is serialized by the singleton database constraint. Authenticated actions include logout, password change, API credential management, logical models/routes, accounts, logs/artifacts, settings, banned words, and hit records.

Security rules:

- initialization requires a high-entropy deployment secret in the fixed `X-Admin-Bootstrap-Token` header, and production refuses to start with a missing, weak, or template secret;
- opaque HttpOnly, SameSite=Strict session cookie;
- `Secure` cookie in HTTPS deployments;
- trusted-Origin validation on initialization and login;
- per-session CSRF token required for authenticated mutations;
- password changes advance the session version so older sessions become invalid;
- no administrator token, password, or session secret in localStorage;
- admin CORS is allowlist-based and permits credentials only for configured origins.

The SPA contains only the administrator views: overview, models, accounts, API Key, logs/artifacts, banned words, settings, and API documentation.

## 10. Data-plane security boundaries

- `/v1` CORS may accept browser origins but never permits credentials; Bearer authentication is still mandatory.
- API key hashes, upstream credentials, CSRF values, and proxy credentials are excluded from serialized models and logs. Panic recovery logs only a bounded request ID, method, path, and panic type; it never dumps request headers or panic values.
- Model and route selection is server-owned; clients cannot supply a provider URL or upstream credential.
- Custom upstream URLs are administrator-controlled. Operators must enforce outbound network policy if internal destinations are not trusted.
- Reference uploads and response bodies have bounded sizes. Provider-selected upload and artifact hosts must pass adapter-specific HTTPS/host validation.
- Artifact fetches and every redirect revalidate the provider allowlist and reject userinfo, unsafe ports, and private, loopback, link-local, multicast, or unspecified destinations before any provider credential is attached.
- Content endpoints remain inside the authenticated `/v1` group. RustFS buckets stay private.
- Event errors are sanitized before persistence or response so credential-bearing transport strings are not exposed.
- Login rate-limit identity is derived from the TCP peer unless that peer is in `TRUSTED_PROXY_CIDRS`; trusted forwarding chains are walked from right to left so a client-supplied leftmost address cannot override the actual peer.

## 11. Persistence and migrations

PostgreSQL is authoritative for the administrator, API credentials, canonical models, routes, provider accounts, route bindings, quota buckets, reservations, dispatch attempts, event logs, and settings.

Redis owns ephemeral concurrency and coordination. It must not be the sole source of truth for quota, dispatch acceptance, or key status. RustFS stores private generated artifacts; metadata and ownership remain in PostgreSQL.

Forward migrations install the singleton administrator/API-key schema, the canonical routing/quota schema, and API-credential attribution on events. After copying retained state, the final migration drops the unreachable legacy `users`, `api_keys`, `model_configs`, and `token_accounts` tables so old customer PII, password hashes, plaintext credentials, and retired model rows are not retained for rollback. Canonical catalog migrations are idempotent and remove non-canonical rows and aliases. Startup seeding is limited to control-plane settings so a restart cannot resurrect retired models.

## 12. Deployment architecture

`docker-compose.yml` is the supported deployment entry point:

- `postgres`: durable relational state;
- `redis`: concurrency and short-lived coordination;
- `rustfs` plus `createbucket`: private S3-compatible artifact storage;
- `backend`: Go API on the internal `6666` port;
- `web`: administrator SPA and reverse proxy on host/container port `2000`.

Operators copy `.env.example` to `.env`, replace every secret (including a random `ADMIN_BOOTSTRAP_TOKEN`), validate with `docker compose config`, and start with `docker compose up -d --build`.

TLS terminates at an operator-managed reverse proxy in front of port `2000`. Production must set an HTTPS canonical `PUBLIC_BASE_URL`, exact HTTPS `CORS_ORIGINS`, `COOKIE_SECURE=true`, and non-default PostgreSQL/RustFS secrets. The web proxy preserves `Host`, appends `X-Forwarded-For`, overwrites client-supplied forwarded schemes, and uses `PUBLIC_BASE_URL` as the only authority for public links.

Liveness checks only the process. Readiness checks dependencies and gates the web container. Nginx disables buffering for `/v1` so chat SSE and media streams pass through immediately. Synchronous image generation writes exactly one final JSON response, preserving the real non-2xx status when validation or a provider fails; long-running callers can opt into `Prefer: respond-async`.

The backend runtime is non-root and its generated-media directory is owned by that dedicated UID. Docker's default seccomp profile blocks Chromium user namespaces, which was confirmed with an in-container launch test, so the Oreate signer explicitly uses `--no-sandbox`. Chromium receives an empty environment and ephemeral profile; parent-death and process-group cancellation tear down the complete browser tree. This accepted residual risk is limited to the Oreate adapter and is preferable to running the API as root or granting the container `SYS_ADMIN`/an unconfined seccomp profile.

## 13. Verification gates

Before release:

```bash
# Backend
cd backend
go test ./...
go build ./cmd/api

# Frontend
cd ../frontend
npm ci
npm test
npm run lint:unused
npm run build

# Deployment syntax, from the repository root with .env present
cd ..
docker compose config
```

Contract tests must assert:

- the model set is exactly 4 text + 6 image + 14 video IDs;
- provider-prefixed and unknown model IDs are rejected;
- every `/v1` endpoint rejects missing, malformed, disabled, or revoked Bearer keys;
- one administrator can be initialized and a second cannot;
- initialization rejects missing, weak, template, or incorrect bootstrap tokens without reflecting them;
- per-key and per-account concurrency are isolated;
- quota reservation is atomic and concurrent settlement preserves other holds;
- every generation reconciles quota;
- accepted or uncertain submissions never fail over and resubmit;
- BytePlus rejects incomplete Cookie material and never serializes the Cookie.
- multipart requests enforce total/file-count limits and remove temporary files;
- artifact redirects cannot reach private networks or send provider credentials to an untrusted host.

## 14. Known operational risks

- Website-backed provider protocols can change without notice. Adapter failures must remain isolated from account credential health unless authentication evidence is explicit.
- Some providers do not expose an authoritative balance. Unknown quota remains schedulable but visible as unknown; conservative reservations still apply.
- A provider may accept work before the connection fails. The no-resubmit acceptance boundary prevents duplicate cost at the expense of sometimes requiring later reconciliation.
- Custom upstreams are an administrator trust boundary and require deployment-level egress controls.
- Long image/video requests need outer-proxy timeouts compatible with Nginx and should use idempotency or asynchronous task mode.

## 15. Change record

### 2026-09-01 — 2API product lock

**Change**: Reframed the repository as a data-plane API plus singleton administrator console; standardized Bearer API credentials; installed a 24-model canonical catalog; separated logical models, provider routes, account bindings, quota buckets, reservations, and dispatch attempts; and made model-aware quota reconciliation part of every generation lifecycle.

**Reason**: Provider-prefixed model names and unrelated application surfaces made the API contract hard to understand and account scheduling unable to preserve model-specific quota.

**Impact**: Downstream clients use stable canonical IDs regardless of the selected provider. Administrators manage routes and accounts without exposing credentials. Deployment is defined by `docker-compose.yml` and `.env.example`.

**Security decision**: One administrator, hashed downstream keys, HttpOnly sessions with CSRF, private upstream credentials, server-owned routing, authenticated content endpoints, conservative accepted-task handling, and a Go 1.26.6-or-newer patched build toolchain define the trust boundary.
