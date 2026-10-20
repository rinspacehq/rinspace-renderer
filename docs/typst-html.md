# Typst reading pipeline

The pinned Typst compiler exports semantic HTML. The adapter preserves its
structure, normalizes SVG/raster assets and routes references across automatic
book pages. Articles remain one page. Authors use ordinary headings; no
`rinWebPage` macro or helper file is part of the source format.

Article outlines use the native heading hierarchy. The renderer assigns stable,
unique IDs only where Typst did not emit an anchor, retaining native IDs and
cross-references. It reserves every existing ID before allocating a new one and
exports the same ordered heading IDs/text/depth in the canonical single article
page's TOC. Generated outline and endnote headings are not article sections.
No author outline command, custom marker or browser-invented anchor is required.

Built-in native HTML presentation is retained, rather than stripping every style.
The policy is reviewed against Typst 0.15.1 source commit
`9dfd3a08500b7896045f907433cf7b4b02434fad`: `typst-html/src/rules.rs`,
`convert.rs`, `css/encode.rs`, `css/resolve.rs`, and `encode.rs`.
Native MathML `math-shift` and `typst-svg/src/lib.rs::svg_in_html` are included
in the same review. The checked-in `native-presentation.html` is actual compiler
output from the adjacent synthetic `.typ`/`.svg` fixtures; CI checks that output
unconditionally and recompiles the sources with the pinned compiler as well.
This includes image dimensions/scaling/display, three-term relative `calc`
lengths, inline boxes, under/overlines, small caps, syntax-highlight colors,
preserved whitespace and outline/footnote list styles. Inline SVG normalization
retains the frame's display dimensions on its authenticated image replacement.
A single explicit image dimension keeps the original aspect ratio; intrinsic
raster dimensions do not override it. `html.elem` can inject arbitrary CSS even
through the official compiler: unsupported or unsafe body declarations fail
closed, never silently lose presentation. External CSS/resource fetching and
page-global styling are not admitted into reader chrome. MathML layout continues
to use the existing equivalent semantic attributes before MathJax conversion.

MathML uses the same pinned MathJax CHTML engine and fonts as LaTeX. Equations
retain anchors and assistive MathML. Identical formulas reuse one conversion;
bounded batches avoid turning a short math-operation timeout into a book limit.
Native MathML remains available through the explicit debug setting
`RIN_RENDERER_TYPST_MATH_RENDER=false`.

Typst's experimental HTML backend can warn that content was ignored even when
the process succeeds. Such capability loss must not publish incomplete HTML:
the same immutable source snapshot is compiled to PDF and inspected before
using the shared authenticated PDF reader. Syntax/dependency errors, canceled
or timed-out jobs, sanitizer failures and MathJax failures remain errors.
The current shared inline PDF delivery contract is limited to 4 MiB; larger
outputs fail explicitly rather than exposing a private PDF via public storage.

Packages are pinned by registry archive digest, reviewed license and a complete
file-tree hash. Runtime jobs copy verified dependency bytes into their isolated
package directory; they never use an ambient host cache or download packages.
`RIN_RENDERER_TYPST_PACKAGE_PATH` names the sealed artifact directory and
`RIN_RENDERER_TYPST_PACKAGE_SHA256` pins its content. Adding a package requires
a new exact toolchain artifact, SBOM, profile and consumer acceptance.

Typst itself supplies SVG export and `html.frame`. CeTZ is a drawing package,
not a TeX-to-SVG service. The compiler's local/embedded graphics enter the same
validated asset pipeline as other renderer resources.

Native compiler HTML is bounded at 64 MiB: repeated images can expand into
repeated data URLs before the adapter deduplicates them into assets. The
normalized reader fragment and unique embedded-asset limits remain unchanged.
An output-limit failure reports both the actual and permitted byte counts;
ordinary compiler warnings must not hide that failure. Set
`RIN_RENDERER_TYPST_MAX_OUTPUT_BYTES=67108864` for this capacity profile.

Resource-bearing results use `rin-document-bundle/v3`: the canonical response
contains each sanitized image once with its exact Base64 bytes, SHA-256, length,
type and content-addressed path. Persist the complete result before publication;
the compatibility payload and temporary job artifacts are not image storage.
Consumers serve those bytes under their document's current read permissions and
map the declared image paths in the reading projection, without altering the
stored bundle. v3 retains non-annotation page semantics; it does not claim v2
semantic blocks. Native output without images and inspected PDF fallback do not
need this resource-carrying contract.

For local regression, run `go run ./tools/typstcheck` from `api/` with an
absolute `-root`, `-mode article|book`, pinned `-binary` and `-font`. Optional
`-package-root`/`-package-hash`, `-mathjax`, `-pdf` and exclusive `-report` flags
exercise real source without connecting to or publishing an account.
`-canonical-output /absolute/private/result.json` with `-publication` writes the
complete canonical result exclusively (mode 0600); it never prints source/image
bytes in the short report. Keep real-work outputs outside the source tree.
Use `-publication` to exercise the actual publication executor (including the
HTML-versus-PDF decision and canonical result validation), not just the native
HTML adapter. This is still a local check: it does not submit a job or publish.
