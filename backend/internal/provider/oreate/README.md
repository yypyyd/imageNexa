# OreateAI Seedance Provider

`oreate` is the internal website-protocol client used by image2api to expose
OreateAI Seedance video models through the gateway's `/v1/videos` API.
OreateAI does not publish an OpenAI-compatible API, so this package implements
the authenticated website flow and its browser-generated Banti signature.

## Responsibilities

- Validate Oreate account cookies and read profile and point balances.
- Map the five public `oreate-seedance-*` model IDs to confirmed website model
  names, resolutions, durations, aspect ratios, audio options, and point costs.
- Upload JPEG/PNG/WebP images and MP4/MOV videos with Oreate's short-lived
  Google Storage credentials, then construct text, ordered-frame, or reference
  scenes with the same attachment metadata as the official frontend.
- Parse MP4/MOV movie headers and enforce Oreate's 2-15 second aggregate
  reference-video window without adding an `ffprobe` runtime dependency.
- Obtain a fresh Banti `jt`, submit the SSE generation request from a warm
  signed page, parse the final artifact URL, and optionally download the MP4.
  Risk control refuses a generation that a browser did not request: an A/B run
  in production had every in-page submit accepted and 20 of 21 Go submits
  refused as spam users, with a freshly minted token and the same exit IP. The
  page is released as soon as upstream accepts the job and the render itself is
  followed from Go, over the sticky proxy session of the submitting page so the
  generation is seen from one exit IP.
- Resolve a dropped stream from the `logId` CDN path when the page already
  collected a log identifier.
- Classify authentication, quota, content, risk-control, and temporary upstream
  failures for the shared account-pool retry policy.
- Route browser and submit traffic through the configured global proxy without
  exposing proxy credentials to Chromium or the destination website.

The package intentionally does not implement reference audio, motion controls,
or models outside the confirmed Seedance set. Account persistence, scheduling,
billing, API authentication, and event storage belong to the service and
repository layers.

## Dependencies

- Go standard-library HTTP and SSE primitives.
- `chromedp` and CDP network events for the official browser-side signer.
  The signer waits for `PARIS_INSTANCE_CACHE` or any `window.paris_*` instance
  that exposes `sendBantiReport`; it does not hard-code a Banti global name.
  In-page SSE fetches send the official `JS-Token` and `Acs-Token` headers
  (`sse` Banti subid).
- A Chromium runtime selected by `OREATE_CHROME` in production.   Signed pages
  stay open between generations, so a submit costs one Banti report (sse) plus
  an ACS token and one stream fetch, instead of a page load (about fifteen),
  and pages are never shared across accounts or used by two submits at once. The pool size is
  derived from the cgroup CPU and memory limits (or the machine's own) at
  startup, so nothing has to be sized by hand; a page is recycled once it goes
  unused, gets old or has been used enough times. `OREATE_SIGNER_PAGES` overrides
  the pool size and `OREATE_PROXY_SESSION=false` disables sticky proxy sessions
  for proxy pools that do not support the session label in the user name.
- The gateway's global `proxy.url` setting and Oreate account pool.

The Docker runtime also supplies a dedicated unprivileged `chrome` user and a
pinned GlobalSign intermediate in both the system and Chromium NSS certificate
stores. Chromium is used only by this provider. Each signer browser runs in its
own process group; cancellation terminates the group, chromedp waits for the
browser process, and the backend container's init process reaps any orphaned
descendants. See [DESIGN.md](DESIGN.md) for the security boundary.

## Account Lifecycle

The service layer retains an Oreate account when a successful balance response
contains an integer `remaining` value below 60, but moves it to the
unschedulable `quota` state. A later successful response at or above 60 returns
that account to `active`. Missing values, malformed responses, timeouts, proxy
failures, and other inconclusive probes do not change its lifecycle state. This
policy is applied after import validation, an administrator quota refresh, and
successful or quota-exhausted generation reconciliation.

The maintenance loop rechecks both quota-state rows and legacy active rows
whose cached balance is below 60. It probes at most four due accounts
concurrently and throttles failed or still-low accounts to once per 30 minutes,
so a next-day point grant is discovered without operator intervention. The
account page also exposes an explicit per-row quota refresh action. Low balance
never deletes the row; deletion remains an administrator-only operation.

Before dispatch, the service resolves the request's official point cost from
its `aiType` and excludes every account with a known cached balance below that
cost. This is separate from account retirement: an 80-point account remains in
the pool and can run a 30- or 60-point request, but cannot be selected for a
100-point request. Unknown cached balances remain eligible until an
authoritative balance probe fills them, preserving compatibility with legacy
rows without treating missing data as zero.

The selected account's exact request cost is atomically reserved immediately
before submit and refunded on generation failure. This closes the queueing race
where two requests could otherwise both observe the same stale balance. An
upstream quota response only changes the account to the global quota state when
balance reconciliation cannot establish that the account still has enough
points to remain useful for cheaper requests.

For requests costing at most 80 points, accounts with a confirmed balance of
exactly 80 are ordered ahead of other balance tiers. This spends the low tier
on work it can afford and preserves higher balances for expensive requests.
Cooling status still takes precedence, and accounts within the 80-point tier
retain their existing weight and round-robin order.

## Internal Usage

```go
client := oreate.NewClient(globalProxy)
account := oreate.Account{
    Cookie:    storedCookie,
    UserAgent: storedUserAgent,
}

video, result, err := client.GenerateVideo(ctx, account, oreate.VideoOptions{
    ModelID:        "seedance-2.5",
    Prompt:         "A paper boat on a calm pond",
    Ratio:          "16:9",
    Resolution:     "480p",
    Duration:       20,
    Audio:          true,
    DownloadResult: true,
})
```

Production callers use the provider through `service.V1Service`; they should
not construct accounts from untrusted request data. `GenerateVideo` returns
either downloaded bytes or a result map containing the upstream artifact URL.

## Verification

```powershell
go vet ./internal/provider/oreate
go build ./internal/provider/oreate
```

## Files

- `client.go`: account, profile, balance, headers, and error classification.
- `models.go`: strict Seedance capability, `aiType`, and point-cost mapping.
- `media_duration.go`: bounded ISO BMFF `mvhd` duration parser.
- `upload.go`: Oreate upload-token exchange and fixed-host GCS resumable upload.
- `video.go`: SSE generation, parsing, logId recovery, and artifact download.
- `signer.go`: Chromium signer, Banti report barrier, and proxy configuration.
- `signer_pool.go`: warm pages, their in-page submits and sticky proxy sessions.
- `video_inpage.go`: the in-page submit script and its result parsing.
- `proxy_bridge.go`: Oreate adapter for the shared authenticated proxy bridge.
- `browser_isolation_*.go`: Linux child-process privilege isolation.
