# Rin Renderer Document Contracts

This package contains source-format-neutral contracts used between Project Core, document
adapters, shared rendering services, and the control plane.

Current contracts:

- `rin-project-graph/v1`: validated project identity, entrypoints, files, references, and options.
- `rin-document-bundle/v1`: source-format-neutral pages, assets, diagnostics, provenance, and typed
  Math/Diagram/Code work units.
- `rin-document-bundle/v2`: the v1 publication model plus a strict semantic block manifest whose
  IDs occur exactly once in each final page fragment.

Consumers accept v1 and v2. Renderer producers remain pinned to v1 until both Markdown and LaTeX
block emitters pass their compatibility gates; changing a bundle to v2 changes its canonical JSON
and therefore its deterministic bundle/cache identity.

Draft page fragments use only the canonical placeholder form
`<rin-work data-id="rw_…"></rin-work>`. Adapters must reject/escape every `rin-work` element in
user-controlled source before injecting placeholders. Placeholder IDs are generated from 128 bits
of cryptographic randomness and validated one-to-one against the bundle work-unit table; unknown,
duplicate, malformed, or unused IDs invalidate the bundle.

Page IDs are opaque publication identity, not DOM IDs. To preserve existing multilingual
Rinspace Book identity they may contain up to 128 Unicode letters, numbers, combining marks, and
the separators `._:-`, and must begin with a letter. TOC, asset, work-unit, and other identifiers
retain their narrower ASCII/namespace contracts. Page IDs are validated and preserved byte-for-byte;
they are never normalized or derived again during compilation or finalization.

Validate schemas, fixtures, TypeScript declarations, and deterministic hashes with:

```bash
node rin-renderer/packages/document-contract/validate-contract.mjs
```

The Go counterpart lives in `rin-renderer/api/internal/contracts`.
