# Rin Final Output Contract

`rin-final-output/v1` is the source-format-neutral success gate applied after an adapter inserts
trusted math, diagram, and code output. It validates the complete reader fragment; it does not
replace the LaTeXML body sanitizer or the Markdown adapter's future `rehype-sanitize` schema.

Native rendering error nodes (`ltx_ERROR`, MathML `merror`, MathJax `mjx-merror`,
KaTeX `katex-error`) are incomplete output, never publishable success. The same
applies to `rin-math-source-fallback`, where failed math work is only shown as code;
successful SVG fallback remains valid. This quality check applies to individual reader pages and already canonical inline
bundles. Escaped examples in documentation are ordinary text and remain allowed.
This deterministic failure is terminal and retains diagnostics; it does not
silently replace author macros or hide errors with CSS.

`fixtures/security-output.json` is the shared adapter conformance corpus. Go consumes it directly
from `api/internal/finaloutput`; the Markdown worker must consume the same cases when its finalizer
is added. A successful adapter result must satisfy these fixtures and declare every generated
public artifact with content-addressed metadata. Local project resources must carry a
`data-rin-asset-path` that exists in the explicit client-owned upload manifest.
