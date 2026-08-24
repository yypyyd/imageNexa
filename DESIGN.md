# Design Notes

### 2026-08-24 - Reduce frontend startup cost and make direct custom egress explicit

**Change**: Vue route components now load on demand, while route metadata drives
user and administrator authorization in the global navigation guard. The Custom
provider owns one reusable HTTP client whose transport explicitly ignores proxy
environment variables. CI now runs Go tests and Staticcheck plus frontend tests,
unused-code analysis, and a production build.

**Reason**: Eagerly importing every view put the entire console into the initial
JavaScript bundle, and a hard-coded protected-path list could drift whenever a
new account page was added. The Custom provider's unused proxy scaffolding also
created per-request clients and obscured the intended direct-egress policy.

**Impact**: Initial page loading downloads only the active route, authorization
requirements live beside route declarations, and Custom requests share a direct
connection pool. Public API contracts, account storage, and provider scheduling
behavior are unchanged.

### 2026-08-18 - Isolate explicit Oreate spam-user accounts and pin proxy sessions

**Change**: Oreate's explicit `212361`/`spam user` response now marks the
affected account `disabled + dead` and fails over without consuming the bounded
temporary-upstream retry budget. Generic risk-control and transport errors stay
temporary. The production proxy session was rotated and its lifetime changed
from five minutes to 60 minutes so the browser signer, chat creation, and full
SSE generation keep one egress address.

**Reason**: Multiple accounts returned the same explicit spam-user decision even
with neutral prompts and no reference media; one account reproduced it from a
new proxy IP. Separately, the five-minute proxy session changed egress during
long video streams and produced `unexpected EOF` on a previously healthy
account. Confirmed spam-user accounts are therefore isolated independently of
the recoverable low-credit `quota` state.

**Impact**: A risk-controlled account is removed from future rotation while a
request can continue with another account; temporary provider failures retain
bounded failover. Low-credit accounts remain recoverable and are refreshed by
the existing quota maintenance path. The new proxy session is shared by the
providers that use `proxy.url`.

### 2026-08-17 - Retain and refresh low-credit OreateAI accounts

**Change**: OreateAI accounts with a confirmed balance below 60 now remain in
the database in `quota` state instead of being permanently deleted. Import,
administrator quota reads, successful generations, quota-error reconciliation,
and the maintenance loop all share the same state transition: below 60 parks
the account, while a later successful reading of 60 or more reactivates it. The
account table exposes a per-row quota refresh action and also rechecks visible
Oreate quota rows during a normal page refresh. Maintenance probes at most four
due low-credit accounts concurrently and throttles each account to one attempt
per 30 minutes.

**Reason**: Oreate can grant points again on a later day. Deleting a low-credit
cookie discarded a still-valid account before that grant could be observed.

**Impact**: Low-credit accounts remain unschedulable but recover automatically
after a confirmed replenishment, or immediately when an administrator refreshes
their quota. Authentication failures may still mark the cookie dead, and manual
or bulk deletion remains available. Oreate is excluded from the generic reset
marker recovery because its marker is a current point-bucket expiry, not proof
that replacement points were granted.

**Decision**: Provider state is recoverable data. Balance snapshots may park or
reactivate an account but never destroy it; deletion is an explicit
administrator operation.

### 2026-08-16 - Preserve accepted submits and close provider recovery loops

**Change**: ChatGPT image-start finalization now recovers a fragmented
`conversation_id` before classifying an SSE read error, and ChatGPT 403 account
death requires an exact invalid/expired-token marker rather than a generic
`unauthorized` substring. Grok anti-bot 403 responses invalidate the current
process-wide Statsig snapshot so the next attempt enters the existing
singleflight refresh. Maintenance revalidates Oreate accounts whose cached
balance is below 60 and deletes them only after a fresh successful balance
response. The authenticated proxy bridge tracks hijacked CONNECT tunnels and
closes both ends during shutdown.

**Reason**: A transport reset after upstream acceptance could otherwise submit
the same image through another account; resource-level 403 messages could kill
valid ChatGPT credentials; a TTL-fresh but rejected Grok signature could be
reused for five minutes; scheduler-excluded Oreate rows had no path to the
destructive balance predicate; and `http.Server.Close` does not own hijacked
connections.

**Impact**: Accepted ChatGPT jobs proceed to polling without duplicate submits,
only explicit token failures poison ChatGPT accounts, Grok reships recover on
the next attempt, legacy low-credit Oreate rows are retired in bounded batches,
and bridge cancellation deterministically tears down active tunnels. Oreate
maintenance probes at most four candidates concurrently and retries failed
confirmations no more than once every 30 minutes.

**Decision**: Destructive account actions require provider-specific affirmative
evidence. Cached Oreate quota is a scheduling hint, not sufficient deletion
evidence, because it may temporarily reflect an in-flight reservation.

### 2026-08-16 - Scope image quality mapping to GPT Image 2

**Change**: `/v1/images/generations` and `/v1/images/edits` derive native-provider
aspect ratio and resolution from `size`. For the GPT Image 2 family only, an
explicitly supplied `quality=low|medium|high` selects 1K/2K/4K when the internal
`resolution` is absent.
Custom multipart image edits now forward `quality` just like JSON generations.

**Reason**: Native providers and unrelated custom models expose their own
resolution-specific parameters and should not have their tier silently changed
by an OpenAI quality enum. GPT Image 2 needs the adapter because its public
contract uses `quality` for the requested tier, and both generation and edit
endpoints must receive the same value.

**Impact**: Non-GPT-Image-2 models use `size` or explicit `resolution` as before,
and custom routes omit the outbound `quality` field for them. GPT Image 2
requests can select a tier through `quality`, clamped to the tiers configured
for that exact model. Thus native ChatGPT `gpt-image-2` remains 1K-only while
`firefly-gpt-image-2` can select 4K. Explicit internal `resolution` remains
authoritative on every route.

**Decision**: Provider-native resolution is the source of truth. Quality-to-tier
mapping is an adapter for the GPT Image 2 family only, not a global
image-resolution rule.

### 2026-08-16 - Selective residential egress and bounded Grok maintenance

**Change**: The persisted `proxy.url` is now injected only into ChatGPT, Grok,
and OreateAI clients. Adobe, Runway, Leonardo, Krea, Imagine, and Custom retain
direct server egress. Grok liveness/credit maintenance checks at most four due
accounts per sweep and gives each account a six-hour check interval. The
account-independent Grok homepage challenge is one immutable process-wide
snapshot; `singleflight` permits only one refresh, and requests continue using
the last good snapshot while that refresh is in flight.

**Reason**: Applying a metered residential proxy to every provider spent paid
traffic on endpoints that work from the Hong Kong host. More importantly, the
60-second maintenance sweep was rechecking the complete 93-account Grok pool
continuously, while a token-keyed cache downloaded the same anonymous homepage
once per account every five minutes.

**Impact**: At the current pool size, scheduled Grok credit probes fall from up
to 133,920 configured checks per day to about 372. Shared challenge discovery
falls from up to 26,784 duplicate homepage reads per day to at most 288 at the
existing five-minute freshness interval. Generation concurrency is unchanged:
the maintenance batch is independent of account job slots, the Goja signer pool
remains concurrent, and no mutex is held during network I/O. Failed liveness
attempts also record their check time so bad accounts cannot monopolize each
minute's batch.

**Decision**: Preserve one residential route for each protected transaction
instead of toggling a process-global transport during requests. Only idempotent
media reads may later gain direct-first/proxy-fallback behavior; ambiguous
generation POSTs must never be replayed across routes because that can duplicate
jobs or charges.

### 2026-08-16 - Grok video submit proxy affinity

**Change**: Grok video parent-post creation at `/rest/media/post/create` now
uses the same proxied TLS session as homepage challenge acquisition and the
subsequent `/rest/app-chat/conversations/new` submit. Reference uploads,
artifact polling, and downloads remain direct.

**Reason**: The flow previously acquired its dynamic Statsig challenge through
the configured proxy and then created the protected parent post through direct
server egress. That mid-flow IP change caused Cloudflare to return an HTML
`Just a moment` 403 even though the signer itself was current.

**Impact**: Text-to-video and image-to-video parent posts keep challenge,
cookies, TLS fingerprint, and source IP on one control-plane route. Only the
small JSON create request moves to the proxy; reference bytes and generated
artifacts retain direct egress.

### 2026-08-15 - Retire low-credit OreateAI accounts

**Change**: A successful OreateAI balance read now permanently deletes the
account when `remaining < 60`. The shared lifecycle check runs after import,
administrator quota refresh, and successful or quota-exhausted generation
reconciliation. Exactly
60 remains, and missing, malformed, timed-out, or otherwise failed balance reads
never delete an account.

**Reason**: Oreate accounts below the operating reserve should leave the pool
instead of remaining as quota rows that can be recovered or selected later.

**Impact**: Account removal is destructive but limited to an authoritative,
typed upstream balance. Concurrent reconciliation is idempotent, manual refresh
reports the deletion to the account page, and a post-generation delete failure
does not turn an already successful customer generation into an error.

### 2026-08-15 - OreateAI Seedance 2.5 and reference-media contract

**Change**: Expanded the OreateAI provider from four text-only Seedance routes
to five video routes by adding `oreate-seedance-2.5`. The 1.5/2.0 models retain
5/10 second outputs; only 2.5 exposes 20/30 seconds in addition to 5/10. The
provider now reproduces the authenticated website's text/image, first/last
frame, and reference image/video scenes, including its separate reference
`aiType` table and attachment schema. Existing database rows receive a
capability-only startup backfill, while operator enablement, aliases, prices,
weights, and generation counts remain untouched. Extended `/v1/models`
metadata is the default catalog contract; strict OpenAI objects remain
available with `?extended=false`.

**Reason**: The live OreateAI account configuration now exposes Seedance 2.5
and reference scenes, but the gateway still seeded zero reference limits. The
fields were present downstream, so clients correctly rendered no reference
slots based on incorrect persisted values. The authenticated configuration is
more reliable than marketing copy and currently declares two ordered images
for 1.5 Pro, or nine images plus three videos for 2.0/2.5; it exposes no
reference-audio slot.

**Impact**: Downstream catalogs receive six ratios, per-model resolutions and
durations, generated-audio support, and non-zero reference limits in both
snake_case and camelCase. Reference videos must be MP4/MOV and total 2-15
seconds after actual movie-header durations are summed and rounded up. 2.5
pricing seeds at zero like the other Oreate models and remains an operator
decision.

**Security boundary**: Oreate's upload-token request follows the configured
control-plane proxy, while the assigned media bytes upload directly to Google
Storage. The gateway generates its own filenames, bounds token responses,
never logs or returns the short-lived bearer token, and accepts resumable
locations only from HTTPS `storage.googleapis.com`. An in-process ISO BMFF
parser reads only bounded uploaded bytes and rejects malformed or missing
`mvhd` duration data; no external media process or client-selected upload host
is invoked.

### 2026-08-14 - OreateAI Seedance website provider

**Change**: Added an independent `oreate` account pool and a website-protocol
client for four Seedance video models: Seedance 2.0 Mini, Seedance 2.0 Fast,
Seedance 1.5 Pro, and Seedance 2.0. The first release supports text-to-video
only. Each generation creates an OreateAI video chat, obtains a fresh Banti
`jt` from the site's official Paris runtime in an ephemeral headless Chromium
profile, submits the SSE request, and returns or downloads the resulting MP4.
The runtime image installs Chromium and launches only the browser child as a
dedicated unprivileged user with a sanitized environment and ephemeral profile.

**Reason**: OreateAI does not provide an OpenAI-compatible Seedance API. Its
website request requires both the authenticated cookie and a short-lived token
computed by browser JavaScript, so a normal HTTP-only custom upstream cannot
represent this integration.

**Impact**: The model catalog exposes only the four confirmed Seedance models,
their confirmed resolution/duration/aspect-ratio combinations, and optional
audio output. Reference images, reference video/audio, first/last frames, and
motion controls remain disabled. The signer waits for the matching Banti `/dr`
report to finish successfully before it submits the token; missing, oversized,
or incompletely reported tokens are temporary failures and use the bounded pool
retry window. Only a definitive authentication failure disables an account,
while insufficient points moves it to quota state. Seed prices seed at zero and
remain an operator decision. `POST /v1/videos` retains its existing OpenAI
`size` mapping and adds an optional `resolution` extension so the confirmed
480p tier is addressable.

**Security boundary**: The admin import endpoint accepts only Cookie and
non-secret account metadata. Oreate export passwords and precomputed Banti
tokens are never accepted or persisted. Cookie values remain in the private
token-account value column and are not returned by account APIs or written to
event logs. Chromium profiles and `jt` values are ephemeral. Official site code
runs in an unprivileged Chromium child process whose environment excludes the
backend's database, Redis, object-store, and proxy credentials. For an
authenticated HTTP(S) egress proxy, the signer strips userinfo before creating
Chrome's `--proxy-server` argument and starts a short-lived loopback-only proxy
bridge. The bridge injects `Proxy-Authorization` only on its upstream proxy
connection, handles ordinary requests and HTTPS CONNECT, and never forwards
that header to the destination website. This keeps proxy credentials out of
process arguments, site requests, errors, and logs while the browser signer and
SSE submit retain the same configured egress. Container egress should still be
restricted to the expected OreateAI/CDN/Banti hosts because that remote code is
outside the gateway trust boundary. OreateAI's CDN currently omits its
GlobalSign GCC R3 DV TLS CA 2020 intermediate certificate. The runtime image
installs that public intermediate from its certificate AIA URL only after
checking a pinned SHA-256 digest. It is added to both Alpine's system bundle and
the dedicated Chromium user's NSS database as an untrusted chain certificate,
allowing the existing trusted GlobalSign root, normal hostname checks, and
certificate validation to remain in force. The certificate expires in March
2029 and the pin must be reviewed if GlobalSign rotates the intermediate before
then.

**Known risk**: The integration depends on undocumented website endpoints and
anti-abuse JavaScript that OreateAI can change without notice. Repeated Banti
report failures can add latency until the bounded retry window expires. Docker's
default seccomp profile blocks the namespace operations required by
Chromium's Linux sandbox, so the container wrapper uses `--no-sandbox` after
dropping to the dedicated UID. This is weaker than Chromium's normal renderer
sandbox. The dedicated UID, empty environment, temporary profile, and restricted
egress are therefore mandatory compensating controls; do not run the browser as
root or grant the backend `SYS_ADMIN` merely to enable namespaces.

### 2026-08-16 - Oreate-only Chromium lifecycle and acknowledged ChatGPT stream closes

**Change**: Chromium is now exclusive to OreateAI's official Banti signer.
Grok's headless Statsig refresher, startup hook, proxy wiring, and 403 trigger
were removed; Grok continues to fetch the live homepage challenge and execute
the current signer chunk in Goja through its normal HTTP client. Oreate signer
browsers run in dedicated process groups, cancellation kills the complete
group, chromedp waits for the launched process, and the backend container uses
an init process to reap any orphaned descendants. ChatGPT image submits now
treat an SSE read error after `conversation_id` has arrived as an accepted
submit and continue through conversation polling; pre-acknowledgement stream
errors remain temporary upstream failures eligible for failover.

**Reason**: Chromium was introduced specifically for OreateAI's browser-only
signature and should not change other provider execution paths. The previous
Grok background browser also rejected authenticated proxy arguments and left
unnecessary browser descendants. Separately, ChatGPT's intentional
post-acknowledgement HTTP/2 close was incorrectly aborting accepted image jobs.

**Impact**: Grok returns to its pre-Chromium HTTP/Goja self-healing path and no
longer starts browser processes at boot or after anti-bot responses. Oreate
retains authenticated browser-proxy support while browser timeouts and exits
have deterministic process-group cleanup plus PID 1 reaping. Accepted ChatGPT
image jobs survive the expected SSE-to-polling handoff; proxy resets and
malformed responses before acknowledgement remain temporary failures.

### 2026-08-14 - Unified control-plane and submit proxy, direct media egress, and unrestricted Adobe submits

**Change**: The persisted administrator setting `proxy.url` is the single
provider egress rule. Authentication and account validation, cookie/token
exchange and refresh, profile/quota/subscription calls, necessary upstream
bootstrap/challenge requests, and the request that creates a generation job use
the configured proxy for Adobe, ChatGPT, Runway, Leonardo, Krea, Imagine, Grok
Web/Build, OreateAI, and custom OpenAI-compatible upstreams. Project/session preparation,
reference-media uploads, generation polling, artifact downloads, post-submit
bookkeeping, and `/content` relays use direct local egress to keep metered proxy
traffic bounded. A custom multipart image/video request carrying references is
itself the generation submit, so the complete HTTPS request uses the proxy;
HTTPS cannot split headers and body across routes. An empty or missing
`proxy.url` makes all of these proxy-eligible calls direct as well. Legacy
provider-specific proxy environment settings do not participate in routing.

Adobe submit lanes, adaptive inter-submit spacing, and the endpoint circuit
breaker have been removed. A submit is dispatched as soon as its selected
account has an available account-level concurrency slot. Adobe's `system under
load`/`timeout_error` remains a normal temporary upstream error and follows the
existing bounded failover/retry behavior; it can no longer open a gateway-wide
breaker that makes downstream requests fail immediately.

**Reason**: A prior Adobe overload response opened a five-minute client-side
breaker after one failure and blocked all downstream calls. The opposite split
also left ChatGPT bootstrap and other edge-protected account routes on a blocked
server exit, while routing media bytes and repeated polling through a metered
proxy made traffic grow with job duration. The unified boundary protects
authentication and edge bootstrap without paying proxy bandwidth for bulk media.

**Impact**: There is no gateway submit rate limit or circuit breaker for Adobe.
Per-account concurrency limits and user concurrency groups remain unchanged,
so an account still cannot exceed its configured simultaneous-job capacity.
Bulk media and polling no longer consume metered proxy bandwidth, but the server
must be able to reach every provider's setup, upload, polling, and download
endpoints directly. There is no automatic proxy fallback: if local egress to
one of those endpoints is blocked, that phase fails. Removing the global Adobe guard increases the chance that a large burst reaches Adobe
and receives provider-side overload/rate-limit errors; scaling should therefore
use account concurrency and upstream capacity, not a hidden global throttle.

**Security boundary**: `proxy.url` is administrator-controlled and may contain
credentials. It is visible only through the authenticated administrator setting
surface and must never be returned by user/provider APIs, inserted into
event/error logs, or copied into support output. The proxy becomes a single
egress point that can observe destination metadata; restrict its access to the
application server and use an authenticated, trusted endpoint. Custom upstream URLs remain
administrator-controlled and retain the existing SSRF/egress-filtering
deployment responsibility.

### 2026-08-11 - Public cross-origin image delivery

**Change**: Generated image API responses now always use the opaque gateway URL
`/v1/images/{event_id}/content`. Persisted results resolve the event to a private
RustFS object key; no-store results resolve it to the provider artifact. New
public event IDs use 24 random uppercase characters. The legacy `/images/...`
route remains available for the site gallery and existing links, but its
owner/timestamp object key is no longer returned by the image API. Both media
routes are CORS-enabled and do not require an API key or session cookie.

**Reason**: Browser and desktop API clients could not reliably consume
successful image generations because each upstream provider has different CORS,
URL expiry, and asset-authentication behavior.

**Impact**: Gateway-hosted image URLs are intentionally shareable bearer links.
API responses no longer disclose the storage owner, username, timestamp, or
object filename. The object store and all non-media API routes remain protected.

**Security decision**: Keep the RustFS endpoint private and expose only gateway
media routes as anonymous resources. The API CORS middleware accepts arbitrary
origins without credentials so browser/desktop downstreams are not rejected;
authentication is still required by user, billing, account, and administration
endpoints. Deployments should enforce per-IP request and connection limits on
both public media routes at the outer reverse proxy.

**Known risk**: A leaked image URL can be downloaded without login until the
underlying object or upstream artifact expires. The opaque event ID prevents
practical enumeration but is not revocable independently of the event/object.
Operators requiring expiring access should add signed URLs.

**HTTPS deployment**: An inner proxy must preserve the outer TLS terminator's
`X-Forwarded-Proto`, and the application includes a non-default
`X-Forwarded-Port` in returned absolute URLs. This prevents an HTTPS API request
on ports such as `9445` from producing an unusable `http://` media URL.
`PUBLIC_BASE_URL` overrides request-derived origins in API responses and should
be set to the canonical Cloudflare/CDN HTTPS hostname in production.

### 2026-08-08 - API-key image URL passthrough

**Change**: API-key image requests using `response_format=url` now return the provider artifact URL directly when the provider supplies one, even when an idempotency key also causes a private recovery copy to be stored. The session-gated `/images/...` URL is only a fallback when no provider URL exists; ChatGPT/Grok account-gated assets continue using the authenticated `/v1/images/{id}/content` proxy.

**Reason**: `/images/<owner>/<file>` is protected by a browser session cookie and cannot be fetched by ordinary downstream API clients. Returning it as an API-key URL made a successful generation appear broken with “需要登录后访问”.

**Impact**: Existing private gallery access and idempotency recovery remain unchanged. Downstream clients receive the upstream URL for ordinary public provider assets; upstream URLs that expire or require provider login remain subject to provider-side availability and use the dedicated proxy path when supported.

### 2026-08-07 - Adobe points-account concurrency safety

**Change**: Adobe points accounts now default to four simultaneous generations per account. Existing points accounts are normalized to an explicit concurrency of four during deployment; newly imported accounts inherit the same effective limit when no override is stored.

**Reason**: High same-account concurrency increases provider throttling and account-risk exposure. A bounded per-account limit of four balances throughput and account stability; pool-level throughput still scales by adding accounts.

**Impact**: Concurrent Adobe jobs are distributed across distinct eligible accounts or queued until an account slot is released. Other provider pools and user concurrency groups are unchanged.

### 2026-08-08 - Face-only thin red-silk reference veil

**Change**: Adobe Seedance 2/2 Fast and all built-in Oreate Seedance reference images now apply a light, medium-weight red mesh with extreme density only inside an inward-trimmed Pigo face rectangle. The previous hair-inclusive expansion and dense near-black mesh were removed; no 3x3 grid, external face, or heavy strands are added.

**Reason**: The previous face veil was too sparse in its reduced version and black lines were harder to distinguish from dark hair. Thin red strands improve visibility while preserving the face-only, non-solid treatment.

**Impact**: Hair, shoulders, clothing, and background remain unchanged. Images without a reliable face remain unchanged, and models outside the Adobe/Oreate Seedance routes still receive original reference bytes.

### 2026-08-06 - Optional reference-image face swapping

**Change**: Adobe Seedance 2/2 Fast and built-in Oreate Seedance reference images run a local Pigo face transform on the gateway for `/generate`, `/v1/images/edits`, and `/v1/videos` when an individual image contains a reliable face. This entry records the earlier experimental 3x3 grid version; it was superseded by the face-only veil above.

**Reason**: Pigo provides a small pure-Go face detector, allowing the gateway to break the original face identity before an upstream reference-image check while keeping the target composition.

**Impact**: Only images with a reliable detected face are transformed; all other references pass through unchanged. Processing is bounded by the existing 20 MiB reference limit and a 100-megapixel decode guard; invalid or oversized transformed images fail before charging.

### 2026-08-06 - Dedicated full-path ChatGPT egress

**Change**: `CHATGPT_PROXY_URL` now pins every ChatGPT Web phase—bootstrap, quota, requirements, reference uploads, prepare/submit, polling, URL resolution, and protected asset downloads—to a provider-specific proxy. An HTML/edge 403, and any 403 on the unauthenticated bootstrap page, is classified as a temporary upstream failure rather than account authentication failure.

**Reason**: The Hong Kong application host receives a Cloudflare HTML 403 from ChatGPT while the former host remains a viable egress. The previous split path proxied only generation submit, and interpreted the account-neutral bootstrap 403 as invalid credentials; one failover loop consequently disabled the entire ChatGPT pool.

**Impact**: User authentication, API keys, billing, logs, account scheduling, and all non-ChatGPT providers remain on the primary application/database. Only encrypted ChatGPT upstream traffic exits through the dedicated host. A true 401 or authenticated non-HTML 403 still disables the affected credential.

**Security boundary**: The egress proxy must require authentication and/or allowlist only the application server IP. Its URL is deployment configuration, never returned by APIs or written to event logs. The proxy sees CONNECT destinations but ChatGPT payloads and bearer tokens remain inside end-to-end TLS.

**Known risk**: The dedicated egress host is now a single point of failure for ChatGPT only. Its outage produces temporary upstream errors without poisoning account health; Adobe and other providers are unaffected.

### 2026-08-05 - Retry randomly unsafe Adobe image outputs

**Change**: Adobe moderation responses now retain their upstream error code. Image generation retries `image_unsafe` on the same account up to two additional times (three total attempts), rebuilding the payload with a unique seed each time. `prompt_unsafe`, `reference_image_privacy_error`, and generic `legal_error` responses are not retried.

**Reason**: Identical short prompts can succeed and fail across calls because Adobe evaluates both the request and each randomly generated output. Treating the first unsafe output as a permanently blocked prompt caused downstream clients to see avoidable `content_policy_violation` failures.

**Impact**: Recoverable output moderation failures are absorbed inside one provider call and one gateway billing event, so they neither charge downstream users multiple times nor trigger account failover/health penalties. Three consecutive unsafe outputs still return a content-policy error with guidance to specify an adult subject, clothing, and scene.

**Decision**: Retry only the explicit `image_unsafe` code. Prompt and reference privacy refusals remain deterministic request errors, while region/legal errors remain upstream failures.

### 2026-08-04 - Seedance shared reference limit and Adobe privacy refusals

**Change**: Seedance 2 and Seedance 2 Fast now advertise 9 reference images, 3 reference videos, and 3 reference audios, with `max_reference_media: 9` enforcing Adobe's shared `referenceBlobs` limit. Their images use the upstream `style` asset mode and all validated image blob IDs are forwarded. Adobe's `reference_image_privacy_error` is classified as a request-level content rejection with a user-facing Chinese explanation.

**Reason**: Adobe's resolved discovery schema declares both per-media limits and a lower shared total. Advertising only the old 2/1/1 limits hid supported inputs, while advertising 9/3/3 without the shared cap would incorrectly imply that 15 files can be combined. Privacy refusals are caused by a real face in the reference image, not by account health.

**Impact**: Model discovery, admin catalog/model views, the playground, and admin test modal expose and enforce the shared total before charging. Content/privacy refusals fail immediately without account failover or failure penalties; generic Adobe 451 legal errors retain their existing upstream-failure classification.

**Decision**: Keep per-type and aggregate limits as separate model fields. A zero aggregate limit means no additional shared cap, preserving existing custom and non-Seedance model behavior.

### 2026-08-04 - Typed video reference media and generated audio

**Change**: Video model capabilities now independently declare image, video, and audio reference limits plus generated-audio support. Session generation, admin tests, and `POST /v1/videos` accept multipart `reference_images`, `reference_videos`, and `reference_audios` fields and a `generate_audio` flag. Adobe media uploads use the matching `/v2/storage/{image|video|audio}` endpoint, and model-specific payloads map those blob IDs into Seedance multimodal, Kling/Luma video-modify, Firefly structure-reference, and Veo/Kling/Seedance audio-output requests.

**Reason**: Treating every reference as a Base64 image made Adobe's video/audio inputs unusable and inflated large media by roughly one third. It also hid the upstream audio-output capability from clients.

**Impact**: Multipart requests are buffered into owned byte slices before asynchronous jobs start; per-file limits are 20 MiB for images, 200 MiB for video, and 50 MiB for audio. MP4/MOV and a conservative audio MIME allowlist are enforced before charging. The reverse-proxy and request upload limit increases to 320 MiB. Existing JSON image-reference requests remain compatible; JSON video/audio references are accepted as raw Base64 or data URIs.

**Decision**: Capabilities are model data and validation is fail-closed. Models whose video, audio, or generated-audio support is not confirmed retain zero/false fields and reject those inputs before debit. Known built-in Adobe rows are backfilled during startup migration.

**Security boundary**: Media endpoints require the existing session or API-key authentication. The gateway bounds the whole request and every file before provider selection, ignores client filenames for filesystem access, and sends accepted bytes only to the configured Adobe storage endpoint. Model capability checks and MIME allowlists run before debit and upload.

**Known risk**: MIME headers and filename extensions are format hints, not a full media decoder; Adobe remains responsible for parsing the actual container. A request near the 320 MiB ceiling is held in memory because asynchronous jobs cannot retain temporary multipart handles, so deployments should keep the existing user-concurrency controls and add edge rate limits when exposing large uploads publicly.

### 2026-07-26 - OpenAI-compatible text reverse proxy

**Change**: Added `POST /v1/chat/completions` for both the existing ChatGPT Web account pool and custom OpenAI-compatible upstream accounts. Custom upstreams support ordinary JSON and live SSE pass-through; ChatGPT Web responses are normalized into OpenAI JSON or a compatible SSE sequence after the web turn completes. Managed models now accept `type: "text"` and use `prices.request` / `prices_agent.request` as a fixed per-request charge. The admin catalog exposes ChatGPT's real model slugs `gpt-5-5-mini` and `gpt-5-5-thinking`.

**Reason**: API clients need to use the same gateway, API keys, account routing, concurrency controls, and credit balance for text models as they already use for image and video generation.

**Impact**: For custom upstreams the gateway rewrites only the upstream `model` field and preserves other Chat Completions request fields. ChatGPT Web receives a role-labelled transcript because its new-conversation endpoint does not accept the OpenAI message history shape directly. HTTP-200 business errors are rejected before billing is finalized; streaming calls must end with `data: [DONE]`. Invalid, interrupted, or incomplete responses are logged as failed and refunded. Streaming holds the selected account and user concurrency slots until the body completes or closes.

**Security boundary**: Clients can select only an enabled local text-model name; they cannot supply an upstream URL or credential. Upstream `base_url` and keys remain admin-managed account data, keys are sent only in the upstream Authorization header, and only a small allowlist of response headers is exposed downstream. Request bodies are capped at 10 MiB and non-stream responses at 32 MiB. This defends against credential disclosure, user-controlled SSRF, and unbounded buffering. As with the existing custom image/video connector, administrators are trusted not to configure an internal or malicious upstream URL; network-level egress filtering remains the deployment operator's responsibility.

## Account credential import

The account-management import parser accepts pasted credentials as well as CPA (`type: codex`) JSON, CPA multi-account ZIP archives, Sub2API account bundles, grok2api `sso*` pool exports, and OreateAI account exports. These formats are normalized in the browser to provider-specific `{ type, value, meta? }` items, so persistence, duplicate-account updates, and asynchronous quota checks continue to use the established provider import endpoints. OreateAI exports are reduced to Cookie, email, device ID, user agent, registration timestamp, and VIP metadata; the export password and cached point details are discarded before the browser sends the import request.

ZIP processing is memory-only and never extracts paths to disk. It accepts JSON entries only, caps the archive at 1,000 JSON files, each uncompressed JSON entry at 2 MiB, and total uncompressed JSON data at 20 MiB. Duplicate credentials are removed before requests are sent. Agent Identity-only records are skipped because the image provider requires a ChatGPT `access_token`; when a file contains no supported credential, the UI reports that limitation explicitly.

## Change history

### 2026-08-12 - Adaptive Adobe submit pacing with in-request retry

**Change**: Adobe submit lanes pace adaptively instead of at a fixed 1.2-second serialized cadence. Each lane starts at a 600 ms floor with up to 2 in-flight submits; overload responses double the spacing toward a 10-second ceiling, successes decay it back to the floor, and the existing breaker still trips on consecutive overloads. `ADOBE_SUBMIT_INTERVAL_MS` now sets the floor and `ADOBE_SUBMIT_INTERVAL_MAX_MS` the ceiling. Temporary upstream failures (overload responses, an open breaker, a temp-failover cap hit) no longer fail the request: the pool scheduler waits with 3→12 second backoff and retries inside the request for up to 120 seconds before surfacing the error.

**Reason**: The fixed serialized cadence capped submits below one per second even when Adobe was healthy, so concurrent bursts queued for a long time behind the lane gate. Raising throughput alone made ten-way bursts trip Adobe's overload response and the breaker, which surfaced `system under load` and circuit-open errors to downstreams that would have succeeded seconds later. The synchronous response heartbeats keep those connections alive while the scheduler waits.

**Impact**: Bursts drain at the floor rate whenever Adobe accepts submits; genuine overload converges to a conservative pace, trips the breaker, and is absorbed as extra latency instead of user-visible errors while the retry window lasts. Only sustained (2+ minute) overload still reaches the downstream as an error.

### 2026-08-12 - Keep points accounts out of default Adobe image routing

**Change**: Ordinary Adobe image requests exclude accounts whose cached quota identifies them as points accounts. An admin-pinned account test still bypasses this filter, and Adobe video entitlement routing is unchanged. Adobe submits are serialized and start at least 1.2 seconds apart across accounts on the direct server egress, while accepted jobs continue polling concurrently. Temporary failures remain capped at three accounts per request. Adobe no longer reads the shared `proxy.url` generation setting.

**Reason**: A burst of image requests received `timeout_error: system under load` across many different ordinary accounts using the same direct egress. Changing accounts did not change the rate-limited dimension and amplified one downstream request into repeated submits.

**Impact**: Image capacity now comes only from ordinary Adobe accounts unless an administrator explicitly pins a points account. Burst submits queue at the gateway instead of hitting Adobe simultaneously; polling and downloads retain their existing concurrency. Provider-wide overload remains bounded and does not disable otherwise healthy accounts.

### 2026-08-12 - Synchronous image response heartbeats

**Change**: Synchronous `/v1/images/generations` and `/v1/images/edits` requests start a chunked `application/json` response after 10 seconds and flush JSON whitespace every 10 seconds until the final OpenAI success or error object is available. The `/v1/` Nginx location disables response buffering and the backend emits `X-Accel-Buffering: no`.

**Reason**: A downstream Go client closed real image-edit requests after 31 and 62 seconds while Adobe was still rendering, producing Nginx `499` entries. Opt-in async cannot help an unchanged client that expects the synchronous OpenAI image shape. JSON-leading whitespace keeps response-header and idle timers active without changing the final parseable document.

**Tradeoff**: Once the first heartbeat commits HTTP `200`, a later provider failure is represented by the standard OpenAI `{"error": ...}` body rather than a non-2xx transport status. Fast validation and admission failures that finish before the first heartbeat retain their original status codes.

### 2026-08-11 - Opt-in asynchronous image API behind Cloudflare

**Change**: `/v1/images/generations` and `/v1/images/edits` accept `Prefer: respond-async` or `?async=true`. Opt-in requests immediately return `202` with a user-scoped `request_id`, `poll_url`, and retry interval while rendering continues independently. The existing task endpoint reports queued, completed, or failed state. Synchronous behavior remains the default.

**Reason**: Cloudflare terminates proxied HTTP requests that produce no origin response for roughly 120 seconds, while valid image jobs can take several minutes. Returning a task before that edge timeout gives controlled downstreams a deterministic recovery contract without changing strict OpenAI clients that depend on the synchronous response shape.

**Impact**: Existing downstream integrations are unchanged. Async clients must poll the supplied URL. Same-user requests sharing an idempotency key are coalesced in-process and guarded by Redis; PostgreSQL enforces a partial unique index for persisted API image events. A persistence failure after charging refunds the unlogged debit.

**Decision**: Keep Cloudflare proxying and make async behavior explicit instead of silently changing every image response to `202`. This preserves edge protection and current client compatibility while providing a supported path for jobs that exceed CDN timeouts.

### 2026-07-30 - URL-first image relay

**Change**: `/v1/images/generations` and `/v1/images/edits` now default to `data[].url`. Ordinary API requests leave provider media upstream and skip gateway downloads, base64 encoding, and object-storage writes. Callers may still explicitly request `response_format=b64_json`.

**Reason**: The gateway is being optimized as a lightweight 2api relay. Returning large base64 payloads by default multiplies memory use and bandwidth under high concurrency without adding routing value.

**Impact**: Clients that relied on the omitted `response_format` producing `b64_json` must either consume `data[].url` or explicitly request `b64_json`. Auth-gated ChatGPT/Grok assets continue to use the gateway's streaming `/content` URL. Requests carrying `Idempotency-Key` retain private persistence for timeout recovery.

**Decision**: Make the low-copy URL path the default while retaining an explicit compatibility escape hatch; do not expose authenticated upstream URLs that downstream clients cannot fetch.

### 2026-07-30 - Recover image results after gateway timeouts

**Change**: API image requests may supply `Idempotency-Key`. Those requests store the completed image in private object storage and expose `GET /v1/images/tasks?request_id=...` for `in_progress`, `completed`, or `failed` recovery after a synchronous gateway timeout.

**Reason**: A CDN can return `524` while the detached backend generation continues. Without a lookup contract, downstream clients mark a real in-flight job failed and may submit a duplicate generation.

**Impact**: Requests without an idempotency key keep the existing no-store behavior. Recovery-enabled requests add one indexed event field and one private stored image; the original synchronous OpenAI response remains unchanged.

**Security boundary**: Task lookup authenticates the API key, scopes the query to that key owner's user ID, image kind, and API source, and uses a parameterized database query. Request IDs are limited to 191 characters, and recovered image reads are capped at 32 MiB. A caller cannot use a known request ID to retrieve another user's task or media.

**Decision**: Reuse the downstream idempotency key as the recovery identifier instead of exposing provider job IDs. This keeps provider details private and lets existing clients recover only the request they originally submitted.

### 2026-07-30 - Grok Web SSO to Build OAuth

**Change**: Added an unattended xAI Device OAuth conversion for existing Grok Web SSO accounts, renewable Build credentials stored in private account metadata, and a native `cli-chat-proxy.grok.com/v1/responses` text client for literal models such as `grok-4.5`.

**Reason**: Grok Web uses mode-based `fast/auto/expert/heavy` requests and cannot select `grok-4.5` literally. The upstream grok2api implementation proves that a valid Web SSO session can authorize a separate Build OAuth credential without asking the operator to export CPA credentials.

**Impact**: Existing Grok accounts are upgraded lazily on their first Build-model request. Web image/video and `grok-chat-*` behavior is unchanged. Access, refresh, and ID tokens are never included in account API responses. Refresh tokens are rotated before expiry; if refresh fails, the service retries the SSO conversion.

**Decision**: Keep one visible Grok account row and store its linked Build credential in private metadata instead of introducing a second account pool. This preserves current scheduling, per-account pinning, imports, and administration while routing literal Build models separately from Web modes.

### 2026-07-30 - Grok-only full-path proxy egress

**Change**: Added the `GROK_PROXY_URL` environment fallback and applied a configured Grok proxy to session/quota probes, Statsig discovery, media upload, generation submits, polling, and artifact downloads.

**Reason**: On networks where `grok.com` is blocked or DNS-poisoned, proxying only the generation submit still leaves account validation, anti-bot refresh, and result downloads unable to connect.

**Impact**: Grok traffic uses the dedicated proxy whenever configured. Other providers retain their existing egress behavior, and Grok continues to connect directly when the setting is empty.

**Decision**: Prefer a provider-specific environment variable over changing the shared `proxy.url`, preventing a Grok connectivity workaround from altering Adobe, Runway, or other provider routes.

### 2026-07-30 - grok2api account-pool imports

**Change**: Added structured parsing for grok2api JSON exports whose Grok SSO tokens are stored under `sso*` account pools.

**Reason**: The existing importer recognized pasted Grok SSO JWTs but did not inspect the `ssoBasic[].token` wrapper used by grok2api exports.

**Impact**: Frontend import parsing and account-import guidance only; backend routes and database schema are unchanged.

**Decision**: Accept future `sso*` pool names while validating every entry by the Grok-specific `session_id` JWT claim before classifying it as a Grok credential.

### 2026-07-29 - Strict OpenAI model discovery by default

**Change**: `GET /v1/models` now returns only the four standard OpenAI model fields (`id`, `object`, `created`, and `owned_by`) by default. The existing gateway-specific capability fields remain available through `GET /v1/models?extended=true`.

**Reason**: Some downstream importers validate model objects with a strict OpenAI schema and reject the entire list when an otherwise valid model contains extension fields.

**Impact**: Standard SDKs and strict model importers receive a minimal compatible response. Clients that use `kind`, supported ratios, resolutions, video durations, or reference-image capabilities (`max_reference_images` and `reference_mode`) must opt into the extended response.

**Decision**: Compatibility is the default contract on the OpenAI endpoint; gateway-specific discovery remains explicit and backward-accessible instead of being removed.

All model-discovery surfaces canonicalize numeric aspect ratios to `W:H` (for example, `16x9` becomes `16:9`). Generation inputs continue to accept either separator and normalize internally, so this output-only consistency rule does not alter provider payload behavior or persisted model data.

### 2026-07-26 - Restore the OpenAI image response contract

**Change**: `/v1/images/generations` and `/v1/images/edits` now default to `data[].b64_json`, honor an explicit `response_format` of `b64_json` or `url`, and pass `quality` into the existing resolution mapping.

**Reason**: The API documentation promised base64 responses, but a URL-only optimization caused successful generations to return a small URL JSON body. Downstream clients expecting the documented OpenAI-compatible base64 field treated those successes as missing images.

**Impact**: API-key image requests download the generated asset before responding when base64 is requested/defaulted, increasing response size and adding the asset-download time. Playground/admin generation and persisted media are unchanged.

**Decision**: Preserve `url` as an explicit bandwidth-saving option while making the documented `b64_json` response the default. Invalid response formats fail before charging.

### 2026-07-25 - CPA and Sub2API account imports

**Change**: Added JSON/ZIP file selection and structured parsing for CPA and Sub2API exports in account management, plus parser tests and format documentation.

**Reason**: Accounts exported by the registration service should be importable without manually extracting and pasting each JWT.

**Impact**: Frontend import parsing and account-import UI only; backend routes and database schema are unchanged.

**Decision**: Normalize into the current ChatGPT import flow so existing deduplication and background validation remain the single source of truth.
