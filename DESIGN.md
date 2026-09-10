# 2API Design

Status: locked product architecture
Last updated: 2026-09-11

## 1. Goals and boundaries

2API is an OpenAI-compatible data plane plus a singleton super-administrator control plane. It exposes text, image, image-edit, and video generation while hiding provider-specific protocols, credentials, upstream model names, and account selection.

The design has five fixed goals:

1. One downstream authentication scheme: `Authorization: Bearer sk-*`.
2. One closed set of canonical model IDs. When the same product is offered by more than one provider, each provider has its own public ID.
3. Model-aware, quota-aware scheduling across multiple upstream accounts.
4. Durable dispatch and quota state that remains correct under concurrency and ambiguous upstream outcomes.
5. One super administrator with the smallest practical web control surface.

There is no public application role. The browser UI is an administrator console only. Provider adapters may evolve, but they cannot enlarge the public model catalog without an explicit product migration.

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

`chatgpt-gpt-image-2`, `byteplus-gpt-image-2`, `adobe-gpt-image-2`, `seedream-5.0-pro`, `seedream-5.0-lite`, `byteplus-nano-banana-2`, `adobe-nano-banana-2`, `byteplus-nano-banana-pro`, `adobe-nano-banana-pro`, `grok-imagine-image`

### Video

`kling-3`, `kling-o3`, `adobe-seedance-2.0`, `oreate-seedance-2.0`, `adobe-seedance-2.0-fast`, `oreate-seedance-2.0-fast`, `seedance-2.0-mini`, `seedance-1.5-pro`, `oreate-seedance-2.5`, `dola-seedance-2.5`, `grok-imagine-video`, `firefly-video`

A provider implementation is represented by a `ModelRoute`. When a product exists on more than one provider, clients request a provider-prefixed public ID such as `chatgpt-gpt-image-2` or `byteplus-gpt-image-2`. Single-provider products keep unprefixed IDs. Retired merged IDs such as `gpt-image-2` and `seedance-2.5` return `model_not_found`.

## 5. Routing data model

### `LogicalModel`

The only model identity visible to downstream clients. It stores canonical ID, kind, display name, enabled state, ordering weight, and generation count. Multi-provider products use one logical model per provider.

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
6. Remove accounts that are disabled, authentication-invalid, cooling down, or known to have less than the model's required quota. Busy accounts remain queue candidates; batched Redis observations avoid acquiring their full gates individually.
7. Within the remaining group, apply account weight, available concurrency, health, and quota best-fit ordering. Round-robin state breaks equivalent choices without starving peers.
8. Atomically acquire concurrency, revalidate the current credential, account state, account binding, model/provider/route switches and quota, and create dispatch/quota reservations before provider submission. A queued first admission respects cooldown; a bounded retry already admitted on that account can continue through its own cooldown.

When a model has multiple routes, make a non-waiting pass across them before queueing on capacity. Only full routes are revisited within one shared 90-second wait; failed routes do not enter a repeated cross-route submit loop. Single-route requests retain their bounded temporary retry, and the verified BytePlus beta chain retains its separate six-distinct-account budget. Queue polling uses jitter and batches capacity reads; per-request account snapshots refresh at most once per second, while each actual admission is revalidated. Equal ranked accounts preserve the distributed round-robin order. Concurrency cleanup uses an independent three-second context so cancelled work cannot leak a slot until its 15-minute lease expires. Capacity observation uses Redis time and never deletes leases.

Clients select a channel by requesting that channel's public model ID. Runway and Custom are not published as public channels in this split. Custom bindings on retired merged IDs are not cloned onto the new IDs. Custom cannot substitute the Dola-only `dola-seedance-2.5` model.

## 7. Model-aware quota scheduling

Quota is scoped by account and route bucket rather than one provider-wide status flag. Different models on the same account can consume different buckets or different costs from one bucket.

Before submission:

- derive the exact normalized model cost from route cost rules;
- reject known insufficient balances without touching the provider;
- reserve the amount transactionally and increment the account's in-flight usage;
- permit an unknown balance while still recording a reservation, so the first later snapshot cannot erase concurrent work.

All trusted manual and automatic balance probes update the same scheduling quota bucket, preserving outstanding holds. Account reset recovery probes every supported provider. Maintenance also claims up to 20 stale/reset-due quota buckets with PostgreSQL `FOR UPDATE SKIP LOCKED` and probes with four workers; successful snapshots expire for probing after 15 minutes, and `updated_at` supplies a five-minute retry delay for failed claims without pretending the balance was refreshed. Unknown/auth-failed probes never replace a balance with zero.

After every generation, 2API refreshes or reconciles the selected account's upstream quota. Reconciliation releases only that request's reservation and writes `upstream remaining - other active reservations`. Concurrent completions therefore cannot overwrite each other's holds.

Outcome rules:

| Outcome | Reservation | Account/route action | Failover |
|---|---|---|---|
| Request or capability error | release if created | no health penalty | no |
| Definite pre-acceptance temporary error | release | cooldown/failure counter as configured | safe route/account retry allowed |
| Authentication failure | release | disable affected credential or binding | allowed |
| Quota exhausted | settle/reconcile actual balance | remove affected account/bucket from eligible set | allowed |
| Adobe image completed, then artifact download failed | already settled at generation completion | retain the result URL; retry that download up to three times | never regenerate or switch routes for download failure |
| Accepted and completed | settle with fresh balance | update success and quota snapshot | no further route |
| Accepted then transient polling/download error | settle conservatively and refresh | keep original task/account and resume it | never resubmit |
| Accepted then explicit terminal provider failure | settle conservatively and refresh | close event/dispatch as failed without permanently disabling the account | never resubmit |
| BytePlus GPT Image 2 exact beta-instability terminal failure, with unchanged live pre-submit and three post-failure balances | release that account's confirmed-unconsumed reservation | retain task ID and cool the failed account | up to six distinct BytePlus accounts on the same route; never revisit an attempted account, enter the ordinary temporary loop, or fail over routes |
| Submission outcome unknown | mark uncertain | schedule reconciliation | never resubmit |

An unknown submission is one whose provider answer was lost (transport failure, timeout, 5xx, undecodable body). A parsed refusal — HTTP 4xx or a business error code such as BytePlus 200402 "No Active Combos." — is definite and follows the matching row above instead. Maintenance reconciles unknown submissions two minutes after they finish by reading the account's authenticated task history for the same model, prompt, and window: a matching task is adopted and resumed as an accepted task; a verified absence fails the event and releases the reservation. If the history cannot be read (for example an expired session), the event is closed after a one-hour hard cap so a downstream client is never left polling `in_progress` indefinitely.

The BytePlus exception is deliberately fail-closed: the fixed GPT Image 2
inference ID, parent/child statuses, and exact failure text must all match, and
each failed account's four balance snapshots must be known and identical. The
bounded chain can submit to at most six distinct accounts, never revisits one,
never enters the ordinary 300-second temporary retry loop, and never crosses the
selected route. Any mismatch, changed/unknown balance, failed probe, or ambiguous
submission stops account switching and falls back to the ordinary accepted-task
rule. Accounts that are merely at their concurrency limit remain at the end of
the candidate set and can be admitted if a slot becomes free during the bounded
account wait.

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

The Cookie must include a non-empty `csrfToken` and valid session data. A standalone CSRF token is insufficient. The server derives `X-Csrf-Token` from the same Cookie for write requests. Profile, avatar, tenant, cached quota, and other exported metadata are ignored and fetched again from BytePlus. An optional email/password pair is persisted only in the private refresh profile for local renewal and never copied into provider metadata or API responses.

The observed Lumina login contract gives `digest` JWTs a fixed 48-hour lifetime while keeping `AccountID` stable across logins. The service hashes `AccountID` to form a non-secret identity fingerprint, uses it to update the existing provider-account row on Cookie rotation, and reads the unverified `digest.exp` claim only as a local scheduling deadline. A known-expired session is excluded from ordinary dispatch; legacy credentials whose expiry cannot be parsed remain probeable so an unknown claim format does not destroy an otherwise valid account. An explicit administrator account test may still select an expired row for diagnosis.

Optional Lumina password-login material lives in private `refresh_profiles` columns and is never serialized by the control plane. The 2API maintenance worker owns renewal end to end: a profile becomes due six hours before `digest.exp`, performs the narrow BytePlus `getLoginCredential` + `mixtureLogin` protocol, verifies that the new Cookie hashes to the same stable `AccountID`, and atomically rotates the existing credential generation. Failures use bounded backoff without killing a still-valid old session. Imports without email/password remain valid but cannot auto-renew.

The Cookie is stored only in private account credential storage. It must never appear in event logs, model discovery, content URLs, errors, or administrator list responses.

### Adobe credential rule

Adobe continues to accept a Cookie as the refresh credential. When no ARP value is imported, the gateway reproduces SherlockSdk's initial local session token: compact JSON containing a random v4 UUID under `sid`, encoded with standard padded Base64. This initial callback happens before Adobe's optional Forter/BFP browser vendors finish and therefore requires no upstream request. A structured browser export may still carry a richer ARP token under `x-arp-session-id` or a supported field alias; imported values take precedence. The token is stored in dedicated private columns on the provider account and its refresh profile, survives access-token refresh and plain-Cookie re-imports, and is attached only to image/video generation submission requests.

The Cookie and ARP token are administrator-supplied secrets. Neither may be serialized by account/profile models, copied into general JSON metadata, included in event errors, or logged. The trust boundary ends at the Adobe generation endpoint; profile, credits, upload, polling, and artifact-download requests do not receive the ARP header without separate protocol evidence.

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

The SPA contains only the administrator views: overview, models, accounts, API Key, logs/artifacts, banned words, settings, and API documentation. The account inventory includes an explicit capability test action: it can run only a canonical model already authorized for that account, pins dispatch to that account, bypasses downstream charging, and serves resulting media only through the authenticated administrator control plane.

### Provider switches

Settings expose one enable switch per provider pool (`provider.<pool>.enabled`, surfaced as `providers_enabled` in the settings API). A pool is enabled unless the administrator stores an explicit false-like value, so existing deployments keep every pool schedulable. The switch is enforced at the single route-matching chokepoint used by text, image, video dispatch and the preflight availability probe: routes whose provider is switched off are removed before any account is read, reserved, or contacted. Routes, account bindings, and account state are left untouched, so re-enabling a pool restores it immediately. When capability matching finds routes but every one of them belongs to a disabled pool, the request fails with `provider_disabled` rather than being misreported as unsupported parameters or spending upstream retries.

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

PostgreSQL is authoritative for the administrator, API credentials, canonical models, routes, provider accounts, route bindings, quota buckets, reservations, dispatch attempts, event logs, and settings. Deleting a provider account removes its route bindings, quota buckets, and bucket-owned reservation ledger entries transactionally; immutable event logs and dispatch history retain their account snapshots/nullable references.

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
go vet ./...
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

Release verification should cover:

- the model set is exactly 4 text + 6 image + 9 video IDs;
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

### 2026-09-05 — Revalidate queued accounts and recover scheduling capacity

**Change**: Detached concurrency cleanup from cancelled work, used Redis-time batched lease observations, unified trusted balance probes with scheduling buckets, and added bounded stale/reset quota refresh. Every actual media/text admission now revalidates account and route/model/provider state. Requests exclude their failed auth/quota candidates, preserve round-robin ties, and try alternate routes before waiting for capacity. Adobe artifact downloads run after generation settlement and cannot trigger a second generation.

**Validation**: Regression tests exercise cancellation cleanup, atomic concurrency under contention, skewed worker clocks, full-pool batch admission, disabled accounts during queueing, auth failure followed by temporary retry, quota recovery with outstanding reservations, equivalent-account rotation, alternate-route capacity, and the accepted-task/download no-resubmit boundary. Existing BytePlus beta retry tests remain part of the suite.

### 2026-09-03 — Make abnormal-account cleanup atomic and FK-safe

**Change**: Migration 000008 makes quota reservations cascade with their account-owned quota bucket. The administrator's abnormal-account cleanup now deletes all selected dead accounts in one database statement, and manual disablement no longer marks a valid credential dead.

**Reason**: A used account could not be deleted because its quota bucket was cascade-deleted while historical reservation rows still held a restrictive foreign key. The handler hid that database error behind `administrator operation failed`; per-row bulk cleanup could also stop after deleting only part of the batch.

**Impact**: Dead-account cleanup no longer fails on accounts with quota history and is all-or-nothing at the provider-account deletion step. Route bindings, quota buckets, and their reservation ledger entries are removed; immutable event logs remain, dispatch account references become null, and manually disabled credentials are excluded from dead-account cleanup.

### 2026-09-02 — Per-provider scheduling switches

**Change**: The settings page gains one enable switch per provider pool. Disabled pools are filtered out at route matching, the shared entry point for text, image, video dispatch and the availability preflight, so their accounts are never selected. The settings API reads and writes them as `providers_enabled`.

**Reason**: Adobe's third-party image gateway currently rejects every server-originated submission with a disguised `408 system under load`. With Adobe left as a live fallback route, a request whose higher-priority providers were filtered out spent dozens of account retries against Adobe before failing. Operators need a way to take a whole provider out of rotation without deleting routes or accounts.

**Impact**: Default is enabled for every pool, so behaviour is unchanged until an administrator turns a pool off. Turning one off is immediate for new requests and reversible; route definitions, account bindings, cooldowns and quota snapshots are untouched. A model whose every capable route is disabled now returns `provider_disabled` instead of `unsupported parameters`.

**Security decision**: Switch values are plain booleans in the existing settings store. The public API learns only that no provider is available, never which pools exist or their state.

### 2026-09-02 — Preserve Adobe ARP session credentials

**Change**: Structured Adobe imports now retain an Adobe-issued ARP session token in private provider-account and refresh-profile columns. Access-token refresh and later Cookie-only imports preserve it, while image/video generation submissions conditionally send it as `x-arp-session-id`.

**Reason**: The previous smart importer reduced Adobe JSON to a Cookie string, so valid session context was discarded before the provider request. Adobe generation submission now supports this session header even though profile and credit endpoints continue to work without it.

**Impact**: Existing Cookie-only accounts are backfilled with the same base session shape emitted by SherlockSdk, while richer imported ARP values remain intact. No extra proxy request is made; only the negligible generation-request header is added. The value is never returned through the administrator or public API.

**Security decision**: ARP is treated as write-only credential material, stored outside public metadata and sent only to Adobe generation submission endpoints. Empty input never overwrites a previously retained token.

### 2026-09-02 — Generate Adobe's base ARP session locally

**Change**: Reverse-engineering the current UniversalNav `ArpService` and commerce `SherlockSdk` showed that the first ARP callback is not an IMS response or signed server token. SherlockSdk immediately emits padded Base64 of compact `{\"sid\":\"<random-v4-uuid>\"}` JSON, then may asynchronously add Forter/BFP vendor fields. Imports now create and persist that base token when none was supplied, refresh repairs legacy empty rows, and migration 000007 backfills existing Adobe accounts and profiles.

**Reason**: Adobe generation submissions increasingly expect `x-arp-session-id`, but running the full Firefly page or fingerprint vendors merely to obtain the base session would add proxy traffic and operational fragility without changing the initial token semantics.

**Impact**: Basic ARP acquisition is local and effectively traffic-free. Existing structured exports with richer tokens still win and plain-Cookie re-imports preserve the stored session.

**Security decision**: Generated ARP values remain write-only secrets. The implementation does not execute third-party fingerprint scripts, send browser telemetry, or expose session values in logs or APIs.

### 2026-09-01 — Restore retained control-plane operations after the rebuild

**Change**: Restored account inventory details and actions that remain valid in the 2API product: per-account success/failure counters, created/last-used timestamps, batch selection/deletion, dead-account cleanup, expandable quota-bucket and failure details, image/video limit markers, and per-account canonical route authorization switches. Restored custom OpenAI-compatible upstream add/edit, account- and model-level pinned capability testing, retained operational overview analytics, and banned-word batch deletion. Restored log type/source filters, source badges, prompt/error copy actions, successful artifact previews, and corrected model capability rendering for array-backed route profiles.

**Reason**: The compact control-plane rebuild retained the corresponding database fields and administrator APIs but omitted their UI surfaces. The artifact gallery also emitted private object keys as if the retired public `/images` handler still existed, so ordinary API-key artifacts could not be previewed by an administrator.

**Impact**: Operators regain the retained account, model-test, operational-health, blocklist, and audit workflows without reintroducing the deliberately retired public-user, billing, payment, invitation, or playground product. Custom upstream keys are write-only in the browser and their public HTTPS base URL remains SSRF-validated. Artifact bytes are streamed only through the authenticated administrator session and reuse the existing provider-aware ownership and SSRF checks; private object keys and downstream API secrets are not exposed.

### 2026-09-01 — Restore pinned account capability testing

**Change**: Restored the account-row capability-test action and its text/image/video result dialog. Tests use only canonical models already authorized for the selected account and are pinned to that account rather than the normal provider pool.

**Reason**: The 2API control-plane rebuild removed the previous account-test UI even though operators still need to verify one credential independently of scheduler failover.

**Impact**: Administrators can test an individual account again without creating or charging a downstream API credential. The mutation endpoint requires the normal administrator session and CSRF token; generated test artifacts are available only through an administrator-session-protected endpoint and cannot expose ordinary API-key artifacts.

### 2026-09-01 — Restore request intent and dimensions in logs

**Change**: Restored prompt, ratio, resolution, duration, reference count, and DeAI metadata to administrator log and artifact responses and their corresponding table/card views.

**Reason**: The control-plane rebuild kept the fields in durable event storage and search predicates but omitted them from serialized log rows, leaving operators unable to see the user's original request intent or requested output size.

**Impact**: The administrator-only log view again shows the submitted prompt and normalized size parameters. No credential or provider response body is exposed, and existing retention policy remains unchanged.

### 2026-09-01 — Close accepted BytePlus terminal failures

**Change**: BytePlus tasks that have been accepted and later report an explicit terminal failure now close their event and dispatch rows as failed instead of remaining recoverable forever. Transient polling and artifact-download errors still resume the same upstream task, and the artifact gallery now includes only successful rows with an artifact path.

**Reason**: A preallocated object path is not proof of a completed artifact, and an upstream terminal failure cannot succeed through further polling. Treating both as incomplete state produced contradictory administration rows and unbounded downstream status polling.

**Impact**: Failed BytePlus generations stop at a sanitized `provider request failed` result without route/account failover or duplicate resubmission. Pending rows display as processing, failed rows display as failed, and unsuccessful preallocated paths no longer appear as completed artifacts.

### 2026-09-01 — Retire five unused video models

**Change**: Removed `luma-ray`, `runway-gen-4-turbo`, `runway-gen-4.5`, `veo-3.1`, and `veo-3.1-lite` from the closed public catalog, request validation, administrator model list, documentation, and account-route bindings. Migration 000005 physically deletes unreferenced routes while retaining disabled tombstones if immutable dispatch history exists.

**Reason**: These models are not part of the product's supported downstream surface and keeping them visible makes model selection and account inventory harder to operate.

**Impact**: The public catalog is now 19 models (4 text, 6 image, 9 video). Requests using a retired ID receive `model_not_found`; historical event rows remain readable.

### 2026-09-01 — 2API product lock

**Change**: Reframed the repository as a data-plane API plus singleton administrator console; standardized Bearer API credentials; installed a 24-model canonical catalog; separated logical models, provider routes, account bindings, quota buckets, reservations, and dispatch attempts; and made model-aware quota reconciliation part of every generation lifecycle.

**Reason**: Provider-prefixed model names and unrelated application surfaces made the API contract hard to understand and account scheduling unable to preserve model-specific quota.

**Impact**: Downstream clients use stable canonical IDs regardless of the selected provider. Administrators manage routes and accounts without exposing credentials. Deployment is defined by `docker-compose.yml` and `.env.example`.

**Security decision**: One administrator, hashed downstream keys, HttpOnly sessions with CSRF, private upstream credentials, server-owned routing, authenticated content endpoints, conservative accepted-task handling, and a Go 1.26.6-or-newer patched build toolchain define the trust boundary.


### 2026-09-10 — Dola 视频协议与账号调度

Dola 使用 `seedance_v2.5`，仅提供 30 秒、720p 文生视频。公共模型 `dola-seedance-2.5` 仅路由到 Dola，原通用 Seedance 2.5 路由保留兼容；两者共享账号日期额度桶。迁移 000019 保留账号绑定的 enabled、entitled、cooldown，不重置额度。未验证的参考图片、视频和音频能力不对外发布。

每个账号同时处理一个请求，每天最多两次，每次预占 1 generation；多个账号可以并行处理请求。UTC 00:00（北京时间 08:00）是本地记账边界，尚不代表已验证的上游重置时间。首次建桶使用 ON CONFLICT DO NOTHING，扣次使用 SELECT FOR UPDATE；结算始终指向预占时的日期桶，避免跨日退款增加新一天额度。网页或独立脚本消耗的次数需单独校准。

上游明确每日额度耗尽后封存当天桶，一般内容拒绝不作为额度耗尽。确认未启动生成的时长拒绝释放预占；已受理后的终态失败保留扣次并禁止重发。发送结果不明保留预占，轮询失败或成品下载失败不能触发换号重试。上游是否返还失败任务次数，需要可靠余额或流水证据，不能自动推断。

账号导入经历 pending → checking → ready。调度同时核验 readiness 版本与 credential generation，管理员指定账号也不能绕过。行锁、探测租约与条件更新防止重复探测及旧结果覆盖；停用或重新导入废弃旧探测，验证不消耗生成次数。

协议生成使用 Passport HTTP 认证、设备初始化、离线 Node 签名、HTTP completion 提交及 chain 轮询。每次创建新会话，提交和轮询使用同一账号代理会话，成片校验实际时长。协议就绪版本独立于旧网页验证。运行时签名 SDK 与脚本属于必要依赖，凭证通过 stdin 输入，日志仅记录安全的错误类别。

30 秒扩展只修改页面配置响应中的时长选项，不提供独立生成接口。协议请求保留成功浏览器请求的字段结构，并区分 Alice device_id 与统计 web_id/tea_uuid，保持请求头与 UA 平台一致。跨域设备注册不携带账号 Cookie，禁用重定向和 Cookie jar，目的域固定，响应有大小与格式限制；身份按账号会话隔离缓存。单次 30 秒成片验证不代表上游长期稳定支持。

登录阶段的 Passport Cookie 可通过 sessionid 与 sid_tt 一致、sid_guard、Passport 字段及地区字段的组合识别，单独 sessionid 不足以认定 Dola。识别仅决定导入类型，账号仍需认证。缺少 ttwid 时通过一次 /chat/ HTTP 请求获取服务端 Cookie，避免加载网页资源或生成视频。

明确终态内容拒绝记为 failed/request，同时独立保持已受理任务的额度结算和禁止重试规则；轮询中断仍保留 accepted。最终 assistant 错误码 710092006 表示成片版权拒绝，710092007 表示音频版权拒绝，向下游返回对应原因；用户消息和未完成消息不用于判定终态。

Dola 的发布验证重点包括请求结构、时长限制、导入识别、就绪验证、模型发现、多账号并发、原子预占、第三次拒绝、跨日隔离、退款幂等、耗尽封存、提交失败禁止重发及迁移保留权限和额度。

### 2026-09-10 — 精简源码发布文件

按维护者要求移除 Go 测试源文件及专用夹具，数据库迁移合并到 `backend/internal/migrations/definitions.go`，不再保留独立 SQL 文件。合并保留原始 SQL 字节、版本、名称与 SHA-256 校验和，已有数据库继续按原规则校验和升级。新增迁移应追加版本，不能修改已应用的定义。

后端 CI 改为编译、go vet 和 staticcheck；前端检查保留。Go 自动回归测试不再随源码发布，静态检查和编译不能替代行为验证。合并时已逐项核对全部 19 个迁移内容与校验和，并在移除测试源文件前通过现有迁移检查。

### 2026-09-11 — Split multi-provider models onto per-channel public IDs

**Change**: Stop merging multiple providers behind one public model ID. `gpt-image-2`, `nano-banana-2`, `nano-banana-pro`, `seedance-2.0`, `seedance-2.0-fast`, and `seedance-2.5` are retired. Clients now request provider-prefixed IDs such as `chatgpt-gpt-image-2`, `byteplus-nano-banana-pro`, and `oreate-seedance-2.5`. `dola-seedance-2.5` remains the only Dola video entry. Runway and Custom are not published in this split. Migration 000020 inserts the new logical models, re-points existing native ChatGPT/BytePlus/Adobe/Oreate routes (IDs unchanged), and leaves merged IDs plus leftover Runway/Custom routes as disabled tombstones when dispatch history still references them.

**Reason**: Channels differ in ratio, resolution, duration, and reference limits. Unioning capabilities and failing over across providers hid those differences and made the public catalog look like a single product. Runway and Custom stay out of the public catalog until those channels are ready.

**Impact**: The closed catalog is 26 models (4 text, 10 image, 12 video). Requests using a retired merged ID, `runway-nano-banana-2`, or `runway-nano-banana-pro` receive `model_not_found`. Scheduling fails over across accounts on the selected native channel only. Existing Custom bindings on merged IDs are not cloned onto the new IDs.
