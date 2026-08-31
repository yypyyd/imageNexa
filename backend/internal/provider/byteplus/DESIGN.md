# BytePlus Lumina provider design

## Goals and non-goals

The package translates image2api's image request into the authenticated Lumina
website contract for exactly five supported models. It owns model/service
mapping, Cookie and CSRF handling, ImageX reference upload, task lifecycle, and
safe artifact retrieval.

It does not automate login, accept a bare CSRF token, expose arbitrary upstream
model IDs, or provide a generic BytePlus SDK. The website protocol is private
and may change; tests encode the observed contract so changes fail closed.

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
| JSON and artifact paths are separate | The website uses CSRF-authenticated Axios calls for control-plane operations but direct resource fetches for output bytes. |
| Provider-owned size mapping | GPT Image 2 uses seven exact sizes and independent quality values; other models use Lumina resolution/aspect controls. |
| At-most-once task submission | `create_task` is non-idempotent and may consume credits. Ambiguous submission responses and all post-accept failures stop account failover and whole-generation retries. |
| Bounded same-resource retry | Polling retries only a known parent task; downloads retry the same URL at most three times within the shared post-accept deadline. |
| Model-aware credit scheduling | The scheduler calculates the official fractional point cost from the normalized outbound payload, filters known-insufficient accounts, and uses best fit within existing weight/cooldown groups. |
| Tracked fixed-point reservations | Tenths-of-a-point row-locked holds prevent concurrent over-commit; reconciliation subtracts other in-flight holds before writing a fresh upstream balance. |

## Failure semantics

Before task submission, explicit authentication and quota failures can disable
or rotate the affected account, while risk and invalid-parameter failures fail
the request without poisoning the pool. Temporary profile or upload failures
retain the gateway's normal temporary failover policy.

The submission boundary is intentionally conservative:

- A temporary/undecodable `create_task` response or a success response without
  `parent_task_id` returns `ErrTaskSubmissionUnknown`.
- After a parent task ID is known, any final polling, URL-validation, or download
  failure returns `ErrTaskAccepted`.
- Both sentinels remain `ErrTemporaryUpstream`, and the service scheduler treats
  them as non-failover errors. An accepted task may additionally expose an
  explicit quota, risk, or invalid-parameter terminal class so the public API
  keeps its business semantics. A refreshed positive balance leaves the account
  eligible for cheaper models; only a balance below the minimum usable cost
  retires it, and the current accepted request is never retried. Authentication-looking and generic
  temporary causes remain diagnostic text only.

This can surface an error even when Lumina eventually completes an orphaned
task, but it prevents a single gateway request from creating and charging for
multiple images.

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
