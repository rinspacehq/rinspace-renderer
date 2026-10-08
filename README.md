# Rinspace Renderer

Rinspace Renderer turns Markdown, LaTeX, Typst, and diagram projects into validated reading results. Its Go API provides authenticated durable jobs, bounded workers, private artifacts, and a versioned protocol. LaTeXML, TeX SVG, MathJax, KaTeX, Shiki, and the fixed PDF/Typst compilers are separate pinned engine inputs.

The same Renderer code supports two deployment modes. `local` keeps jobs and sanitized SVGs on local persistent storage and lets callers poll job status; it needs no Rinspace account, CloudBase credential, or Control Plane endpoint. The default `control-plane` mode preserves the Rinspace product contract: a trusted service submits a project scope and immutable source identity, Renderer records the result, and a signed persistent completion event is delivered to Control Plane. End-user login and publication authorization belong to the Rinspace product, not this service.

A service token authenticates each caller. `X-Rin-Renderer-Owner-Scope` is the service's declared project scope; it is not proof of an individual user's identity. Clients must use a unique `Idempotency-Key` for each submission and keep the job ID returned by `POST /api/render/jobs` for status, result, cancellation, and retry operations. Protocol schemas and synthetic fixtures are in [`packages/protocol`](packages/protocol/README.md). Product completion contracts are included there as snapshots; the private product checks that they still match its authoritative versions.

## Source layout

| Directory | Contents |
| --- | --- |
| `api/` | Go API, admission, queue, worker, completion outbox, artifact stores, and adapters |
| `engines/` | Pinned Markdown/MathJax/KaTeX/TeX SVG inputs and LaTeXML lock |
| `packages/protocol/` | Schemas, TypeScript types, and synthetic contract fixtures |
| `deploy/` | Engine Dockerfiles, version locks, compiler wrappers, and source policy checks |
| `corpus/` | Synthetic article, book, diagram, Typst, and PDF editor cases |

The Go module paths are `github.com/rinspacehq/rinspace-renderer/api` and `github.com/rinspacehq/rinspace-renderer/engines/texsvg`. The Dockerfiles accept `RENDERER_SOURCE_ROOT=.` when built from this repository root. The default `rin-renderer` source root keeps reviewed private builds compatible during the transition.

## Configuration boundary

On Linux, the local Compose profile starts PostgreSQL, schema migration, API, one document worker, and one TeX SVG worker with bounded CPU and persistent volumes. Its startup command generates local credentials in an ignored mode-0600 file and requires a released image by exact digest:

```sh
node deploy/start-local.mjs --image ghcr.io/rinspacehq/rinspace-renderer/runtime@sha256:<reviewed-digest>
```

Once a reviewed `deploy/release-manifest.json` is present, `node deploy/start-local.mjs` validates its source-contract hashes and uses its pinned local image; an image override must match. The API listens on `127.0.0.1:8090`. The optional PDF and Typst compiler profiles require separate restricted broker images and are not enabled by this local Compose file.

A durable API or worker requires PostgreSQL (`RIN_RENDERER_DATABASE_URL`), a persistent private artifact directory (`RIN_RENDERER_ARTIFACT_ROOT`), and `RIN_RENDERER_SERVICE_TOKEN`. API and worker roles may run separately with `RIN_RENDERER_RUNTIME_ROLE=api` and `worker`; a migration command initializes the schema before `RIN_RENDERER_SCHEMA_MODE=require-current` is used.

For standalone use, explicitly select both `RIN_RENDERER_COMPLETION_MODE=local` and `RIN_RENDERER_STORAGE_PROVIDER=local`. Set an absolute `RIN_RENDERER_LOCAL_ASSET_ROOT`, a matching loopback `RIN_RENDERER_LOCAL_PUBLIC_BASE_URL`, and bind `RIN_RENDERER_ADDR` to the same loopback port. This mode rejects Control Plane publication fields, so it cannot silently accept work requiring a callback. Local SVG URLs are valid only on that machine and must never be written into a Rinspace publication.

The v1 diagram response keeps `cloudbaseUrl` as a compatibility alias for its public asset URL. In local mode that field and `url` can both contain the loopback URL; callers should read `storage.provider` from capabilities to distinguish local files from CloudBase. The next incompatible protocol revision can rename this field without breaking current Rinspace clients.

For Rinspace product use, keep `RIN_RENDERER_COMPLETION_MODE=control-plane` and `RIN_RENDERER_STORAGE_PROVIDER=cloudbase`. Worker startup requires a Control Plane event URL and a separate HMAC key of at least 32 bytes; the service token cannot double as the signing key. The existing durable outbox, retry, terminal version, and readiness checks remain active. CloudBase access is injected at runtime. Product deployment configuration and user credentials stay outside this repository.

PDF and Typst compiler workers use a restricted broker profile and their own pinned image and toolchain locks. A local installation may leave those optional workers disabled; the API does not claim a compiler is available merely because its protocol exists.

## Lightweight source checks

Use Node.js 22 and Go 1.25 for the API (Go 1.24 for the TeX SVG module). Install the three pinned Node engine graphs before running the complete Go tests, since the Go suite starts Node workers:

```sh
npm --prefix engines/markdown ci --omit=dev --ignore-scripts
npm --prefix engines/mathjax ci --omit=dev --ignore-scripts
npm --prefix engines/katex ci --omit=dev --ignore-scripts
node packages/protocol/validate-protocol.mjs
node --test deploy/source-policy.test.mjs
npm --prefix engines/markdown run check
(cd api && GOMAXPROCS=1 GOFLAGS=-p=1 go test ./...)
(cd engines/texsvg && GOMAXPROCS=1 GOFLAGS=-p=1 go test ./...)
```

The Go tests use synthetic fakes by default. Database integration tests require an isolated test database and are skipped when none is configured. Heavy TeXLive/PDF image builds and corpus runs belong on a bounded CI runner, not a production host.

Rinspace's private product consumes only a reviewed release manifest with exact artifact digests after public checks and a separate private integration decision. A mutable branch or tag does not change production input. See [`LICENSING.md`](LICENSING.md) and [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) for source and third-party rights.
