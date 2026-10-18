# Typst reading pipeline

The pinned Typst compiler exports semantic HTML. The adapter preserves its
structure, normalizes SVG/raster assets and routes references across automatic
book pages. Articles remain one page. Authors use ordinary headings; no
`rinWebPage` macro or helper file is part of the source format.

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

For local regression, run `go run ./tools/typstcheck` from `api/` with an
absolute `-root`, `-mode article|book`, pinned `-binary` and `-font`. Optional
`-package-root`/`-package-hash`, `-mathjax`, `-pdf` and exclusive `-report` flags
exercise real source without connecting to or publishing an account.
Use `-publication` to exercise the actual publication executor (including the
HTML-versus-PDF decision and canonical result validation), not just the native
HTML adapter. This is still a local check: it does not submit a job or publish.
