# OreateAI Seedance Provider Design

## Goals

- Expose only the confirmed OreateAI Seedance video models through the existing
  account pool and asynchronous `/v1/videos` contract.
- Support the official text/image, ordered first/last frame, and reference
  image/video scenes while advertising only capabilities observed in the live
  authenticated account configuration.
- Reproduce the official website request flow without persisting Banti tokens,
  browser profiles, account passwords, or proxy credentials outside their
  existing private stores.
- Fail closed on unsupported model options, incomplete Banti reports, malformed
  SSE output, and invalid artifact URLs.

## Non-Goals

- General OreateAI model discovery or support for non-Seedance models.
- Browser automation for video creation itself; Chromium is used only for the
  official anti-abuse signer, while generation remains an HTTP/SSE request.
- Reference audio or motion controls until confirmed by the authenticated
  upstream protocol and account capability data.

## Request Flow

```text
V1 service
  -> account pool selects an Oreate Cookie
  -> for references, request scoped upload assignments from OreateAI
  -> upload media bytes directly to fixed-host Google Storage resumable URLs
  -> Chromium loads the official AI-video page
  -> Paris/Banti runtime produces an opaque jt
  -> signer waits for the matching /dr request to finish with 2xx
  -> signed page submits the Seedance SSE request with JS-Token (subid sse), Acs-Token, jt and BID
  -> parser returns the final HTTPS artifact URL
  -> gateway records success and exposes /v1/videos/{id}/content
```

The `jt` callback can run before the browser's report request finishes. Closing
Chromium immediately at callback time cancels that report and leaves a token
that the generation API cannot validate reliably. `bantiReportWaiter` arms
immediately before dispatch, tracks only newly-created `banti.oreateai.com/dr`
requests, and returns only after the tracked request fully loads with a 2xx
status. Empty and implausibly large tokens are rejected.

## Model Contract

The public model IDs are:

- `oreate-seedance-2.0-mini`
- `oreate-seedance-2.0-fast`
- `oreate-seedance-1.5-pro`
- `oreate-seedance-2.0`
- `oreate-seedance-2.5`

`SeedanceConfig` is the single strict mapping from public IDs and gateway
options to website model names and `aiType` values. Unsupported resolution,
duration, ratio, or audio combinations fail before an upstream submit.

Seedance 1.5 Pro and the 2.0 variants expose 5 and 10 second output durations.
Seedance 2.5 additionally exposes 20 and 30 seconds, with 480p and 720p output.
All five models support the six confirmed ratios and generated audio.

The authenticated scene configuration currently allows:

- Seedance 1.5 Pro: up to two ordered images, mapped to first/last frames.
- Seedance 2.0 Mini/Fast/standard and 2.5: up to nine reference images and
  three reference videos, with twelve total items at most.
- Reference-video total duration: 2-15 seconds after summing actual MP4/MOV
  durations and rounding up. Reference audio remains disabled.

`SeedanceReferenceConfig` owns the separate video-reference `aiType` table.
Image-only reference scenes continue to use `SeedanceConfig`, matching the
official frontend.

`SeedanceRequiredCredits` maps the resulting `aiType` to the point price from
the official `getmodelconfigv3` response. Prices are explicit rather than
formula-derived because reference-duration bands contain irregular values.
The service applies this price before round-robin selection: known balances
below the request cost are skipped without changing account status, while an
exact balance is eligible. This check is also applied to administrator-pinned
accounts. If all otherwise selectable accounts are known insufficient, the
request returns Oreate's quota-exhausted error instead of claiming that no
provider account exists. Rows with no cached balance remain eligible so missing
or inconclusive balance data is never silently interpreted as zero.

Immediately before submit, the service atomically reserves the exact cost from
the cached balance. This prevents queued or explicitly concurrent requests from
both spending the same cached points. A failed generation refunds the hold; a
successful generation replaces it with the authoritative upstream balance. If
upstream reports insufficient points but reconciliation still shows at least
the 60-point operating floor, the failure remains task-specific and does not
move the account into the pool-wide quota state.

After normal health, weight, and round-robin ordering, requests priced at 80
points or less apply a stable 80-point-balance preference inside each cooling
group. This drains the low tier without disturbing order among equal-tier
accounts or allowing a cooling account to jump ahead of healthy accounts.

## Account Lifecycle

Oreate accounts are recoverable when their confirmed remaining balance cannot
cover the operational reserve. A successful integer `remaining < 60` moves the
row to the unschedulable `quota` state; a later successful `remaining >= 60`
returns a quota row to `active`. Network errors, missing fields, malformed
types, and otherwise unknown balances leave the previous status and cached
balance intact. Authentication failures remain a separate dead-cookie signal.

One shared service-layer transition owns the threshold and is used by import
hydration, administrator quota refresh, and generation-time reconciliation.
Quota keys are merged without replacing unrelated account metadata. An
administrator-disabled or dead row is never reactivated merely because a
balance refresh succeeds.

An explicit upstream `212361`/`spam user` response is classified separately
from generic Banti risk-control failures. The service marks the affected account
`disabled` and `dead`, removes it from future scheduling, and fails over without
counting the permanent account failure against the temporary-failure cap.
Transport and signer timeouts remain eligible for bounded failover and never
disable an account.

Oreate awards 50 bonus credits for an account's first successful image
generation and only pays them out when the site's first-use endpoint asks for
the grant. Importing a new account therefore generates one image on the
cheapest tier (Kling3.0 Omini, 1k, 1:1, 6 credits), requests the grant even
when the stream broke, and polls the balance until the award lands. The
attempt is stamped in account metadata before any credit is spent, so a
re-imported cookie or a crashed probe never pays for a second image. The image
itself is discarded.

The maintenance loop closes the recovery gap for scheduler-excluded rows. It
selects both quota-state accounts and legacy active accounts cached below the
floor, records a probe timestamp, and fetches at most four balances
concurrently. Successful readings update the cache and lifecycle state; failed
or inconclusive probes keep the previous state and are not attempted again for
30 minutes. The generic reset-marker recovery explicitly skips Oreate because
`reset_after` is the expiry of a current positive point bucket, not evidence of
a new grant. The account page uses the same authoritative refresh path for its
per-row action and when reconciling visible Oreate quota rows.

## Proxy And Browser Isolation

Chromium does not support credentials embedded in `--proxy-server`. For an
authenticated HTTP(S) proxy, the signer creates a short-lived listener bound to
`127.0.0.1`, gives Chromium only that credential-free address, and forwards
ordinary requests and CONNECT tunnels through the configured upstream proxy.
`Proxy-Authorization` is injected only on the bridge-to-proxy hop and stripped
from destination headers and copied responses.

CONNECT connections are hijacked from Go's HTTP server, so the shared bridge
tracks them explicitly. Closing the bridge marks it closed before shutting down
the listener, rejects late registrations, and closes both ends of every active
tunnel; relying on `http.Server.Close` alone would leave hijacked connections
outside the server lifecycle.

On Linux, the container runs the backend and Chromium as the dedicated,
unprivileged `chrome` UID. Chromium starts with an empty environment, a
temporary chromedp profile, and a parent-death signal. The
runtime wrapper uses `exec`, so Chromium replaces the wrapper instead of
becoming its child. Each browser is also a process-group leader: context
cancellation or signer timeout kills the complete browser process group, and
chromedp waits for the launched process before returning. The backend service
runs with a container init process to reap any Chromium descendant that exits
after being orphaned. Chromium is an Oreate-only dependency; no other provider
starts it. The runtime wrapper exposes only `HOME`, `PATH`, and locale. Docker's
default seccomp profile prevents Chromium namespaces, so the
already-unprivileged process uses `--no-sandbox`; the dedicated UID, empty
environment, ephemeral profile, and restricted egress are mandatory
compensating controls.

## TLS Chain Decision

OreateAI's CDN currently omits the GlobalSign GCC R3 DV TLS CA 2020 intermediate
from its TLS handshake. Alpine command-line clients use the system CA bundle,
while Chromium 131 uses an NSS database under the browser user's home.

The Docker build downloads the exact public intermediate from its certificate
AIA URL, verifies a pinned SHA-256 digest, and installs it into both stores. In
NSS it is stored as an untrusted chain certificate, so the existing trusted
GlobalSign root and normal hostname validation remain authoritative. Build-only
`openssl` and `nss-tools` packages are removed from the final image. Disabling
certificate validation is not an accepted fallback. The intermediate expires
in March 2029; its URL and digest must be reviewed if GlobalSign rotates it.

## Security Boundary

Threats considered:

- Cookie, Banti token, or proxy credential leakage through logs and process
  arguments.
- Proxy credentials being forwarded to OreateAI.
- Arbitrary remote JavaScript reading backend database or storage credentials.
- Malformed upstream responses causing unbounded reads or unsafe URL handling.
- A missing certificate chain encouraging global TLS bypass.

Controls:

- Imports persist only Cookie and non-secret metadata; exported passwords and
  precomputed Banti values are discarded.
- Probe credentials arrive through stdin. Probe output contains only lengths,
  booleans, timings, statuses, and sanitized hosts.
- Browser proxy arguments never contain userinfo, and the bridge is loopback
  only and short-lived.
- HTTP and SSE bodies have explicit limits, and artifact URLs must be HTTPS.
- Upload-token responses are bounded and validated. Short-lived GCS bearer
  tokens are never logged or returned; resumable uploads accept only HTTPS
  `storage.googleapis.com` locations, so an upstream response cannot redirect a
  token to an arbitrary host. Client filenames are not used as object names.
- Browser profiles and `jt` values are ephemeral; Banti reports must complete.
- TLS verification stays enabled with a pinned intermediate-chain repair.

Official website JavaScript remains outside the gateway trust boundary.
Production egress should allow only required OreateAI, CDN, Banti, and confirmed
risk-control hosts.

## Alternatives Rejected

- A static or reused `jt`: tokens are per-browser and short-lived.
- Reimplementing Banti: brittle, difficult to audit, and unnecessary when the
  official runtime can execute in an isolated browser.
- CDP proxy authentication: Chromium 131 resource loading was unstable under
  Fetch interception, and credentials risked wider browser exposure.
- `--ignore-certificate-errors`: hides unrelated TLS failures and weakens the
  entire browser session.
- Returning as soon as the Banti callback fires: cancels the asynchronous `/dr`
  report that accompanies the token.

## Known Limitations

- Website endpoints, model names, and Banti JavaScript are undocumented and can
  change without notice. The signer waits for `PARIS_INSTANCE_CACHE` or any
  `window.paris_*` instance that exposes `sendBantiReport`, not a hardcoded
  global name.
- Signer latency includes page load and the full Banti report, and transient
  proxy failures may consume the bounded account-pool retry window.
- The proxy bridge currently supports authenticated HTTP and HTTPS proxies;
  authenticated SOCKS proxies are rejected.
- Browser egress isolation is a deployment network control, not an in-process
  hostname allowlist.

## Change History

### 2026-09-11 - SSE-only submit

- Video and first-image bonus post only `/oreate/sse/stream` from the signed
  page, with the official `JS-Token` / `Acs-Token` headers and Banti subid `sse`.

### 2026-09-11 - Live Paris instance discovery

- Wait for `PARIS_INSTANCE_CACHE` or any `window.paris_*` Banti instance instead
  of the hardcoded `paris_21a851acb0` global the current frontend no longer
  assigns before its 3MB errorMonitor bundle finishes.
- Accept the official `sendBantiReport` fallback token argument and Banti
  report hosts under `*.oreateai.com`.
- Log a credential-free signer verdict when the runtime is missing so the next
  frontend change is visible in backend logs.

### 2026-08-19 - First-use bonus claimed at import

- Added a low-tier image generation plus first-use grant request on import.
- Stamped the one-time attempt in metadata and refreshed the cached balance
  once the award landed.

### 2026-08-17 - Recoverable low-credit quota state

- Replaced permanent below-60 deletion with a retained `quota` state.
- Added bounded 30-minute balance rechecks and automatic reactivation after a
  confirmed balance reaches 60 or more.
- Added administrator per-row refresh and visible quota-row reconciliation.
- Excluded Oreate from optimistic generic reset-marker recovery.

### 2026-08-16 - Authoritative retirement sweep and tunnel shutdown

- Added a bounded maintenance pass for cached-below-60 rows, with a fresh
  balance confirmation and a 30-minute retry interval after failed probes.
- Kept cached reservations non-destructive so in-flight work cannot cause an
  account deletion by itself.
- Made bridge shutdown close active hijacked CONNECT tunnels explicitly.

### 2026-08-16 - Oreate-only Chromium lifecycle

- Kept Chromium exclusive to Oreate's official Banti signer.
- Started every signer browser in its own process group and made timeout or
  cancellation terminate the complete group before chromedp returns.
- Enabled a container init process to reap orphaned Chromium descendants.

### 2026-08-15 - Cost-aware account routing

- Recorded the official point cost for every supported Seedance `aiType`.
- Excluded accounts whose known balance cannot pay for the current request,
  including administrator-pinned tests, without disabling accounts that can
  still serve cheaper combinations.
- Prioritized confirmed 80-point accounts for requests costing at most 80
  points while preserving cooling, weight, and round-robin behavior.
- Preserved exact-balance eligibility and the non-destructive treatment of
  unknown balances.

### 2026-08-15 - Low-credit account retirement

- Added permanent account deletion after an authoritative balance response
  reports fewer than 60 remaining points.
- Kept exactly 60 points and all unknown or failed balance reads non-destructive.
- Applied the lifecycle rule consistently during import, manual quota refresh,
  and successful or quota-exhausted generation reconciliation.

### 2026-08-15 - Seedance 2.5 and confirmed reference scenes

- Added Seedance 2.5 with 5/10/20/30 second output durations.
- Added confirmed image/video reference capabilities, media upload, ordered
  frames, reference `aiType` mappings, and MP4/MOV duration validation.
- Added capability-only seed backfills so existing model rows expose the same
  limits to downstream `/v1/models` clients without overwriting operator prices
  or enablement.

### 2026-08-15 - Production validation

- Added authenticated proxy bridging, Chromium privilege isolation, ad-domain
  blocking, NSS chain repair, and the Banti `/dr` completion barrier.
- Added deployment-only signer and balance probes with stdin-only credentials.
- Validated one 5-second 480p Mini generation and streamed the resulting MP4
  without persisting it locally.

### 2026-08-14 - Initial provider

- Added account/profile/quota operations, four Seedance model mappings, website
  chat and SSE generation, and gateway service integration.
