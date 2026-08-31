# BytePlus Lumina provider

This package implements the authenticated website protocol used by
[BytePlus Lumina](https://ai.byteplus.com/lumina/en). It is an internal provider
for image2api, not a general-purpose BytePlus SDK.

## Scope

The model catalog is deliberately closed to these five public gateway IDs:

| Public ID | Lumina model |
|---|---|
| `lumina-seedream-5.0-pro` | Seedream 5.0 Pro |
| `lumina-gpt-image-2` | GPT Image 2 |
| `lumina-seedream-5.0-lite` | Seedream 5.0 Lite |
| `lumina-nano-banana-2` | Nano Banana 2 |
| `lumina-nano-banana-pro` | Nano Banana Pro |

The package supports text-to-image and reference-image generation, including
ImageX upload, risk prediction, task submission and polling, URL-only results,
and bounded result download. It does not expose arbitrary Lumina service IDs,
scrape login credentials, or create browser sessions.

## Credential and request contract

`Client` receives a complete Lumina browser Cookie. The Cookie must contain a
non-empty `csrfToken`; JSON/control-plane calls mirror it in `X-Csrf-Token`.
Credentials must stay in private account storage and must never be logged.

Task creation is non-idempotent. Once submission may have reached
`create_task`, the package returns a no-resubmit sentinel on an ambiguous
response. After a parent task ID is known, polling retries only that task and
artifact retries use only the returned URL. Callers must honor
`ErrTaskSubmissionUnknown` and `ErrTaskAccepted` and must not replay the whole
generation.

## Internal use

```go
client := byteplus.NewClient(proxyURL)
data, metadata, err := client.GenerateImage(ctx, accountCookie, byteplus.ImageRequest{
    Model:          "lumina-seedream-5.0-lite",
    Prompt:         "A lighthouse at dusk",
    Resolution:     "2K",
    AspectRatio:    "16:9",
    DownloadResult: true,
})
```

`GenerateImage` returns bytes only when `DownloadResult` is true. Metadata
always includes the provider model, parent task ID, status, and validated image
URL after a successful task.

## Public API

- `Models` and `LookupModel` expose the immutable five-model mapping.
- `IsBytePlusCookie` and `CSRFTokenFromCookie` validate imported credentials.
- `FetchProfile` and `FetchCreditsBalance` support account probes.
- `RequiredCredits` calculates the exact upstream point cost for the normalized
  model, canvas, quality, and reference count used by account scheduling.
- `GenerateImage` runs upload, submission, polling, and optional download.
- `OpenAsset` retrieves a validated Lumina result URL with strict host, size,
  redirect, and image-content checks.

The implementation depends only on the Go standard library and the shared
image2api provider/service integration.

## Account scheduling and credits

The service reads `lumi/computing_points`, keeps fractional balances, and
reserves each request's upstream cost atomically before submission. Current
Lumina billing rules are:

| Model | Upstream cost |
|---|---|
| Seedream 5.0 Pro | 9 points at <= 2.36 MP, otherwise 18; references after the first add 0.3 each |
| GPT Image 2 | 1/3/7 (low), 8/25/60 (medium), or 30/96/230 (high), selected by the normalized canvas |
| Seedream 5.0 Lite | 3.5 points |
| Nano Banana 2 / Pro | 12 points at 1K/2K, 24 at 4K |

Known-insufficient accounts are skipped for that request while unknown balances
remain eligible. Within the same operator weight and cooldown group, the
smallest sufficient known balance is preferred so larger balances remain for
expensive requests. Every successful or possibly accepted generation refreshes
the upstream balance. In-flight holds are tracked separately so concurrent
refreshes cannot erase one another.

## Files

```text
byteplus/
├── client.go       Account, model, HTTP, and error contracts
├── client_test.go  Catalog, credential, redirect, and quota tests
├── image.go        Payloads, ImageX upload, polling, and artifact handling
├── image_test.go   Generation, retry, upload, and security tests
├── README.md       Package usage and scope
└── DESIGN.md       Design decisions and security boundaries
```

See [DESIGN.md](DESIGN.md) for protocol and failure semantics.
