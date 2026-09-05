# BytePlus Lumina provider design

## Goals and non-goals

The package translates image2api's image request into the authenticated Lumina
website contract for exactly five supported models. It owns model/service
mapping, Cookie and CSRF handling, ImageX reference upload, task lifecycle, and
safe artifact retrieval.

It implements only the narrow password-login flow needed by 2API's private
renewal worker. It does not provide browser automation, accept a bare CSRF token,
expose arbitrary upstream model IDs, or act as a generic BytePlus SDK. The
website protocol is private and may change; tests encode the observed contract
so changes fail closed.

## Request flow

```text
complete Cookie
    -> profile and optional ImageX upload/risk check
    -> create_task (one non-idempotent submission)
    -> query_task_list(parent_task_id)
    -> validated BytePlus result URL
    -> optional bounded download of that same URL
```

JSON/control-plane requests send the complete Cookie, the matching
`X-Csrf-Token`, language, origin, and referer headers. Authenticated API calls
reject redirects so credentials and POST bodies cannot cross origins. ImageX
temporary credentials and Lumina result URLs each have separate HTTPS host
allowlists.

## Key decisions

| Decision | Reason and impact |
|---|---|
| Closed five-model catalog | Prevents clients from selecting unreviewed service IDs and avoids public ID collisions with OpenAI and Runway models. |
| Complete Cookie is the durable credential | Lumina requires the browser session; a bare CSRF token cannot authenticate and is rejected. |
| Stable account identity across Cookie rotation | `AccountID` remains stable across the observed 48-hour login sessions. Only its SHA-256 fingerprint is retained for identity, so re-import replaces the existing account credential without exposing the identifier. |
| Expiry-aware scheduling | The unverified `digest.exp` claim is a local scheduling deadline, not authentication proof. Known-expired sessions are skipped; unknown legacy formats remain eligible for provider validation. |
| JSON and artifact paths are separate | The website uses CSRF-authenticated Axios calls for control-plane operations but direct resource fetches for output bytes. |
| Provider-owned size mapping | GPT Image 2 uses seven exact sizes and independent quality values; other models use Lumina resolution/aspect controls. |
| At-most-once task submission | `create_task` is non-idempotent and may consume credits. Ambiguous submission responses and unknown post-accept failures stop account failover. The sole exception is Lumina's exact beta-instability verdict after one pre-submit and three post-failure live balance snapshots confirm that account was uncharged; it may advance through at most six distinct accounts on the same route, with an independent proof for every failed account. |
| Bounded same-resource retry | Polling retries only a known parent task; downloads retry the same URL at most three times within the shared post-accept deadline. |
| Model-aware credit scheduling | The scheduler calculates the official fractional point cost from the normalized outbound payload, filters known-insufficient accounts, and uses best fit within existing weight/cooldown groups. |
| Tracked fixed-point reservations | Tenths-of-a-point row-locked holds prevent concurrent over-commit; reconciliation subtracts other in-flight holds before writing a fresh upstream balance. |

## Failure semantics

Before task submission, explicit authentication and quota failures can disable
or rotate the affected account, while risk and invalid-parameter failures fail
the request without poisoning the pool. Temporary profile or upload failures
retain the gateway's normal temporary failover policy.

The submission boundary is intentionally conservative:

- A lost `create_task` answer (transport failure, timeout, 5xx, undecodable body)
  or a success response without `parent_task_id` returns
  `ErrTaskSubmissionUnknown`.
- A definitive refusal (HTTP 4xx or a parsed business error code such as 200402
  "No Active Combos.") additionally carries `ErrUpstreamRejected`. Nothing was
  created upstream, so it keeps its own class (quota, auth, risk, invalid
  parameters, or plain temporary) and never becomes ambiguous.
- `FindSubmittedTask` lets the service settle an ambiguous submission later by
  reading the account's authenticated task history for the model, prompt, and
  submission window: a match is adopted as an accepted task, an authenticated
  empty result proves nothing was created. An expired session (code-0 guest
  profile) is `ErrAuth`, never evidence of absence.
- After a parent task ID is known, final polling, URL-validation, or download
  failures return `ErrTaskAccepted`.
- A GPT Image 2 task with its fixed inference ID, parent `failed`, and a same
  child exactly `downstream_execute` whose trimmed `fail_reason` equals Lumina's
  known beta-instability message additionally returns
  `ErrRetryableTaskFailed`. The failed parent ID remains available for dispatch
  audit. 2API reads a live balance before submission and three times after
  failure; only four known, unchanged values prove the attempt uncharged, release
  its reservation, and permit the next distinct account. Every failed account
  must independently pass the same proof. Unknown, changed, or failed balance
  probes retain the ordinary no-resubmit semantics and stop the chain. The
  bounded chain submits to at most six distinct accounts; it does not return to
  any attempted account, enter the normal 300-second temporary retry loop, or
  switch provider routes. Eligible accounts that are only concurrency-full stay
  at the candidate tail and may be admitted if their slot becomes free during
  the bounded account wait.
- All three sentinels remain `ErrTemporaryUpstream`.
  `ErrTaskSubmissionUnknown` and ordinary `ErrTaskAccepted` are non-failover
  errors; `ErrRetryableTaskFailed` is account-failover eligible only with 2API's
  private no-charge proof. An accepted task may additionally expose an
  explicit quota, risk, or invalid-parameter terminal class so the public API
  keeps its business semantics. A refreshed positive balance leaves the account
  eligible for cheaper models; only a balance below the minimum usable cost
  retires it, and the current accepted request is never retried. Authentication-looking and generic
  temporary causes remain diagnostic text only.

This can surface an error even when Lumina eventually completes an orphaned
unknown task, but it prevents replay whenever an accepted outcome is not known
terminal and uncharged.

## Security boundaries

- Cookie and CSRF values are accepted only through administrator account import,
  stored privately, excluded from logs and public metadata, and never forwarded
  outside the exact API origin.
- API redirects are rejected. ImageX upload redirects are rejected, and upload
  hosts are restricted to the observed BytePlus HTTPS suffixes.
- Result URLs are normalized against a BytePlus asset allowlist. Cookies are
  attached only to the exact Lumina API origin; public CDN URLs receive none.
- Reference images and response bodies have hard size limits. Downloaded output
  must sniff as an image (including AVIF handling) before it is returned.
- A CDN/ImageX 401 or 403 is not evidence that the durable Lumina Cookie is dead;
  only an explicit session error from the API origin can invalidate the account.
- Business error codes are parsed before HTTP status so a quota, risk, or input
  rejection carried by a 403 cannot be misclassified as expired authentication.
- Optional Lumina login identities and secrets are private refresh-profile
  fields. The local maintenance worker performs password login six hours before
  expiry, accepts the result only when stable `AccountID` matches, and backs off
  failures without disabling a still-valid Cookie.

## Known limitations

- The integration depends on an undocumented website contract and requires
  maintenance if Lumina changes its payloads or endpoints.
- Ambiguous task submission cannot be reconciled without an upstream idempotency
  key or a safe history lookup, so the package chooses no-resubmit over automatic
  recovery.
- The provider returns one image per task even when the upstream representation
  contains multiple output fields.
- Reference uploads and result downloads buffer bounded files in memory.

## Change history

### 2026-09-05 - Bounded multi-account beta-failure failover

Allowed a GPT Image 2 task ending with Lumina's exact parent `failed`, child
`downstream_execute` beta-instability verdict to advance through at most six
distinct BytePlus accounts on the same route. Four live, unchanged balance
snapshots are required independently for every failed account before the narrow
exception records that attempt as failed/temporary and releases its unconsumed
reservation. Any unknown or changed balance fails closed and stops switching.
The chain never cycles to an attempted account, enters the normal temporary
retry loop, or leaves the BytePlus route. Concurrency-full eligible accounts are
retained at the candidate tail so they can be used when a slot is released. All
ambiguous, generic, quota, risk, authentication, and invalid-parameter failures
keep their previous no-resubmit behavior.

### 2026-09-03 - Session rotation and expiry-aware scheduling

Added stable account matching from a SHA-256 fingerprint of `AccountID`, so a
new 48-hour login Cookie replaces the prior credential even for rows created
before this identity scheme. The service records `digest.exp`, excludes a
known-expired session from ordinary dispatch, and exposes valid, expiring,
expired, or unknown session state to the administrator console. The final six
hours are considered expiring. When a private refresh profile contains login
material, 2API renews the session locally six hours before expiry, verifies the
stable account identity, and rotates the Cookie in place. Accounts without
login material continue to use explicit Cookie re-import.

### 2026-08-31 - Initial provider

Added the five-model catalog, complete-Cookie account flow, ImageX reference
uploads, task submission/polling, safe result access, account quota probes, and
frontend/service integration.

### 2026-08-31 - At-most-once submission hardening

Added no-resubmit sentinels for ambiguous submissions and accepted tasks,
same-task polling retry, bounded same-URL download retry, and regression tests
covering network failures, 5xx responses, classification, and URL-only mode.

### 2026-08-31 - Model-aware account credit scheduling

Added official Lumina cost calculation for all five models, including 3.5-point
and 0.3-point fractional charges, canvas-specific GPT Image 2 pricing, and the
Seedream Pro pixel breakpoint. Account selection now filters by each request's
exact cost and prefers the smallest sufficient balance within the existing
operator weight and cooldown groups.

Generation reserves credits under an account row lock before submission. A
separate in-flight reservation total makes successful balance reconciliation
safe under concurrency: an upstream snapshot is reduced by every other pending
hold. Explicit pre-billing failures refund; successes and ambiguous/accepted
submissions commit and refresh `lumi/computing_points`. The at-most-once boundary
is unchanged: a possibly paid task is never refunded and retried on another
account merely because polling, download, or the create response failed.
