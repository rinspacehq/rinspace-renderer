# Rin Renderer Protocol

This package owns the shared Rin Renderer API contract.

Current contents:

- `openapi.yaml`: canonical HTTP contract.
- `schemas/job-status.schema.json`, `job-submit-response.schema.json`, `job-event.schema.json`, and
  `queue-status.schema.json`: authenticated durable-job, replayable SSE payload, and privacy-safe
  aggregate queue contracts.
- `schemas/job-submit-request.schema.json`, `latex-pdf-preview-result.schema.json`, and
  `latex-pdf-preview-error.schema.json`: the additive, feature-gated current-draft PDF preview
  request, private artifact, and bounded error contracts.
- `schemas/typst-pdf-result.schema.json`: the additive Typst PDF contract. `typst-pdf-preview`
  carries a session snapshot identity; `typst-pdf-export` carries an exact project commit identity.
  The two branches are mutually exclusive, only a required PDF and a bounded log artifact are
  allowed, and no TeX aux/fls/synctex artifact is expected.
- `schemas/job-support.schema.json`: an owner-scoped support projection for job, attempt, wait
  estimator, and private artifact metadata. It intentionally excludes identities, titles, source
  paths, storage keys, worker identities, and lease material.
- `schemas/*.schema.json`: JSON Schemas for core request and response payloads.
- `src/index.ts`: TypeScript types used by clients before code generation is added.
- `RinDiagramBatchV1` / `RinDiagramBatchResultV1`: the versioned shared service boundary used by
  LaTeX extraction and restricted Markdown diagram directives.
- `validate-protocol.mjs`: lightweight schema/type consistency checks for self-hosted CI.
- `contracts/rin-control-plane/v1/renderer-completion.schema.json`: reviewed snapshot of the
  product-owned completion event contract. The private product CI checks its bytes against the
  canonical `contracts/rin-control-plane/v1/renderer-completion.schema.json`; the public protocol
  check reads only this package so it can run without the private repository.

The protocol package is intentionally lightweight at this stage so GitHub Actions can validate it without installing a full frontend toolchain.

The project render response includes a manifest payload with normalized project files, resolved source fields, include references, diagram placeholders, asset inventory, bibliography inventory, and diagnostics. Document engine adapters must consume this manifest instead of reparsing multipart uploads.

`POST /api/render/projects` is a compatibility adapter over the durable queue. Existing clients
continue to receive the synchronous project response; clients may send `Prefer: respond-async`
with an `Idempotency-Key` to receive a durable job location immediately.

`POST /api/render/jobs` also accepts the additive `latex-pdf-preview` output kind. Preview jobs are
always LaTeX `latexmk` work with resource class `latex-pdf` and priority class `preview`; they carry
the immutable snapshot hash, session, draft revision, entrypoint, engine policy, and pinned compiler
image digest. Their `Idempotency-Key` is the lowercase SHA-256 of those identity values in the order
documented by OpenAPI. A successful result is private and contains exactly one required PDF,
SyncTeX, bounded raw log, aux, and fls artifact, with optional bbl/blg/toc/out files. Artifact paths
must remain in the root-adjacent `.rinspace/` directory; PDF, log, and aggregate limits are 32 MiB,
2 MiB, and 48 MiB respectively.

This branch is additive: requests that omit `outputKind` keep the existing document-render behavior,
the multipart `bookPages` field remains a JSON string, and `/api/render/pdf/inspect` keeps the
`rin-pdf-inspection/v1` / `pdf-inspect` contract. Setting the server-side
`pdf_preview_enabled=false` rejects only the new output kind and is the protocol rollback switch.

The bounded `error` projection on a failed job status is shared: draft previews keep their preview
codes, while a committed document job classifies a rejected project source as `source_invalid` and a
compiler rejection as `compile_failed`. Document jobs carry the engine diagnostic in `message`, so a
Markdown, LaTeX or Typst build failure reaches the author instead of a generic transport summary.

`POST /api/render/jobs` also accepts the additive Typst `typst-pdf-preview` and `typst-pdf-export`
output kinds, all with `contentKind: typst`, `documentEngine: typst`, and resource class
`typst-pdf`. A preview submission carries the same session/snapshot identity fields as the LaTeX
preview and hashes them with its Typst engine policy and pinned compiler image; an export
submission carries `controlProjectId`, `sourceCommit`, and `controlProjectHash` instead. A
successful result is a private `rin-typst-pdf/v1` payload containing exactly one PDF and an
optional bounded log, with artifact paths confined to `.rinspace/typst/`. LaTeX preview v1 is
unchanged and the two result schemas are never interchangeable.

Queued job responses always include an explicit `estimate` field. It is `null` until every
workload that can block the target has at least 20 successful samples. A known estimate contains
an observed p50/p90 start range, confidence tier, minimum contributing sample count, estimator
version, scope, and calculation time; it is advisory rather than a guaranteed start time.

Operational endpoints are intentionally split: `/health` is process liveness, `/ready` reports
normalized database/artifact/scheduler-worker readiness, and `/metrics` exposes bounded Prometheus
metrics. A degraded worker returns readiness 503 while capabilities and authenticated job
status/result routes remain responsive; readiness payloads never include raw dependency errors.

The Manager can download one retained LaTeX PDF preview artifact through the owner-scoped
`GET /api/render/jobs/{jobId}/artifacts/{artifactKind}` route. The API validates the stored result,
database metadata, expiry, byte count, and SHA-256 before returning private `no-store` bytes; the
editor never receives the Renderer service token.

Authenticated owners can inspect `/api/render/jobs/{jobId}/support`, retry an eligible failed job
with `POST .../retry`, or stage a terminal job and its attached private artifacts for cleanup with
`POST .../expire`. Retry never expands the job's existing attempt budget, and both mutations write
durable audit events. The existing authenticated `DELETE /api/render/jobs/{jobId}` remains the
bounded cancellation operation.
