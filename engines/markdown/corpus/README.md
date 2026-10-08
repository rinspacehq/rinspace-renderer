# Markdown deterministic corpus

This corpus runs the pinned Markdown page pipeline twice for every synthetic fixture and compares
the two semantic records before consulting the accepted baseline. It records final HTML and Bundle
hashes, HAST element/class/data surfaces, headings and internal references, diagnostics, typed work
counts and metadata, assets/artifacts, pipeline/plugin/sanitizer/Shiki versions, and hashes of the
behavior-owning Markdown and shared MathJax contract sources.

The `book-multipage` fixture covers multiple project entrypoints through the page pipeline. It does
not claim navigation or global Book assembly, which remains owned by task 7.1.

Math and Diagram corpus work uses deterministic fixtures matching the shared Go wrapper/artifact
contracts; Code uses the live pinned Shiki renderer. Real shared-service integration remains
covered by Go tests and becomes end-to-end through the article/Book adapters in later milestones.

Run only on the self-hosted build path after `npm ci`:

```sh
npm run corpus:test
```

The checked-in baseline is not silently updated. Sensitive dependency, theme, font, plugin, corpus,
or sanitizer changes are evaluated by `check-upgrade-gate.mjs`; non-empty deltas require a matching
human-reviewed acceptance manifest supplied as `RIN_RENDERER_MARKDOWN_CORPUS_ACCEPTED_DELTAS`:

```json
{
  "schemaVersion": "rin-markdown-corpus-delta-acceptance/v1",
  "deltaHash": "sha256-from-report",
  "reviewedBy": "maintainer-name",
  "reviewedAt": "2026-08-10",
  "reason": "Expected renderer upgrade semantics."
}
```

CI uploads the report, field-level delta document, and changed-file list for review. Artifact quota
failure is reported but does not conceal or bypass the in-job gate.

## Article shadow manifests

`run-article-shadow.mjs` renders an explicit bounded set of article sources through the durable
Markdown Job API without publishing or mutating Rinspace content. A manifest uses
`rin-markdown-article-shadow-manifest/v1`, declares whether its sources are private, and contains at
most 50 entries with a stable ID, title, exactly one relative Markdown source path or allowlisted
public Rinspace article API URL, optional legacy HTML path or numeric HTML byte baseline, optional
legacy render time, and expected feature names. URL sources also require the exact extracted source
SHA-256 and are accepted only for manifests declaring that they contain no private source. The
response must remain an open Markdown article; redirects, credentials, query strings, fragments,
non-HTTPS URLs, non-article API paths, metadata drift, and source-hash drift are rejected.

The report contains source/output hashes and byte counts—not source prose—plus structural counts
for headings, paragraphs, lists, tables, quotes, footnotes, MathJax, Shiki/plain code, links,
images, directives and diagrams. It records bounded diagnostic identities, exact Renderer
versions, safety invariants, elapsed time, and legacy structure/size/latency deltas when supplied.
Numeric baselines must document their implementation fingerprint, environment, warmup, sample
count, and statistic in manifest provenance so comparisons are reviewable without storing prose.
The new elapsed value covers durable submission, queueing, render, polling, and result retrieval;
the documented legacy baseline measures the synchronous browser fallback parser, so its delta is an
end-to-end migration cost comparison rather than an engine-only microbenchmark.
Exact `\\not\\exists` source metadata is an explicit gate. Final HTML snapshots are opt-in because
they can contain author prose.

The public source checks use committed synthetic manifests only. Product promotion can supply
separately reviewed representative manifests and run them in an isolated private environment;
the public repository must not fetch live Rinspace user content in CI. A synthetic smoke test
does not substitute for representative product acceptance.

## Book shadow manifests

`run-book-shadow.mjs` applies the read-only durable-job gate to ordered multi-page Books. Its
`rin-markdown-book-shadow-manifest/v1` contract requires at least one `small` and one `large`
representative, stable page IDs/paths/titles, and per-Book output-byte and end-to-end latency
budgets and exactly one changed-page probe. Each Book is first rendered with cache reads disabled
to seed verified page records. The changed project is then rendered incrementally and once more
with cache reads forced off. Promotion fails unless the warm run reuses unaffected page-finalizer
records and its result hash, Bundle hash, ordered page records, source dependency hashes, features,
navigation, assets, and safety surface are identical to the changed project's clean full render.
The report also records a median reader JSON/index/HTML-parse probe against a declared budget.
This is the equivalence gate required before a Book cohort can rely on incremental page reuse.

The runner can process credential-free, query-free open URLs only when a product owner supplies a
separate reviewed manifest. Public CI runs synthetic local sources only. The runner rejects
redirects and unapproved hosts or paths, and checks source SHA-256 identities. Sources stay in
memory and reports contain no prose.

The checked-in `fixtures/book-shadow-synthetic.json` covers the runner contract. Product cohort
promotion requires an approved representative manifest and reviewed report outside the public
source tree. Reports contain hashes, counts, sizes and timings, never source prose.
