# Rin Renderer Markdown engine

This private ESM package owns the pinned Node dependency graph for Rin Renderer's Markdown data
plane. It is deliberately independent from the Rinspace browser/UI package and from the heavy
LaTeXML/TeXLive image.

The worker speaks `rin-node-worker/v1` as newline-delimited JSON on stdin/stdout. It exposes
`health`, the bootstrap-only `markdown.smoke`, the non-publishable `markdown.compile-draft`, and
the typed `shiki.render-batch` operation. The draft operation applies the fixed Rin MDAST plugins
and returns a validated `rin-document-bundle/v2` in `draft` state with stable semantic blocks and opaque Math/Diagram/Code
placeholders. The Shiki operation uses only the pinned `github-light` theme and grammar allowlist;
unknown languages return escaped plain code. `markdown.finalize` inserts only exact, same-kind,
known work IDs into parsed HAST, rewrites DOM identifiers/references into the `rin-md-` namespace,
applies the explicit final MathJax/Shiki/diagram/GFM/Rin sanitize schema, and returns a validated
publishable final Bundle plus immutable generated-artifact metadata. Draft and resolved fragments
are never publishable without this final operation.

The final sanitizer and book cache use version 3. Pinned MathJax 4.1.3 explicit
line breaks emit `mjx-linestack`, `mjx-linebox` and `mjx-linestrut`; their inert
layout attributes are admitted only on the specific CHTML tags. Older sanitizer
cache records are not reused. Unknown custom tags, handlers, unsafe URLs and
styles remain rejected rather than silently stripped.

The reported `pipelineVersion` is derived from a canonical hash of every locked package version and
integrity value. Updating any dependency graph entry therefore changes the version deterministically.

Install and check with Node 22:

```sh
npm ci --omit=dev --ignore-scripts
npm run check
```

After installing the sibling `engines/mathjax` locked runtime, also run
`node mathjax-finalizer.test.mjs`. CI runs this producer-to-finalizer contract
with real inline/display multiline TeX and adversarial output mutations.

Production-grade checks and future image builds run on the self-hosted `rinspace-build` GitHub
Actions runner.
