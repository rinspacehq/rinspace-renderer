# Third-party source and image inputs

This is the source-level dependency review for the initial Renderer candidate. It is **not** a substitute for the exact SBOM and full notice bundle that must accompany each published binary, Node runtime, or OCI image. Dependency versions are fixed by the checked-in locks; a lock or image digest change requires a new notice review.

| Input | Pin / source | License evidence |
| --- | --- | --- |
| LaTeXML | `engines/latexml/latexml.lock`, commit `ae2c8b266d1aa04af4350a64c79215bbe4b7c482` | Upstream `LICENSE` at that commit dedicates the NIST-authored software to the public domain (CC0-equivalent statement). |
| Typst compiler | `deploy/typst-pdf.lock.json`, v0.15.1 asset and binary SHA-256 | Upstream v0.15.1 `LICENSE`: Apache-2.0. |
| WenQuanYi Zen Hei font used by the Typst HTML toolchain | `deploy/typst-html-toolchain.lock.json`, Debian package `fonts-wqy-zenhei=0.9.45-8`, file SHA-256 `79c18ebe…78a66cfe` | The lock records `GPL-2.0-only WITH Font-exception-2.0`; the toolchain release must carry its full license, exception, copyright notice and corresponding Debian source reference. |
| TeXLive, LaTeX packages, Perl, Debian base and fonts | `deploy/Dockerfile.latexml`, `deploy/latex-pdf.lock.json`, `deploy/typst-pdf.lock.json` | Many distinct package licenses. The exact installed package closure and license/notice files must be captured in each image's SPDX document; source locks alone are insufficient. |
| Node.js runtime image input | `deploy/Dockerfile.runtime`, Linux/amd64 digest `sha256:46e94f8cf91baab69a2deb3153e74eeffd73c20c7cc1d8432f5b96469eaa0322` | Node.js and its Debian base retain their respective licenses; the built runtime image must carry their exact closure and notices. |
| Go API dependencies | `api/go.mod` and `api/go.sum` | Direct and transitive module license files were checked at pinned versions: MIT, ISC, BSD-style, Apache-2.0, and the MIT/Apache dual license of the YAML modules. `github.com/pdfcpu/pdfcpu@v0.14.0` is Apache-2.0; `github.com/jackc/pgx/v5@v5.8.0` is MIT. The binary release notice bundle must include each module's actual license and any `NOTICE`. |
| TeX SVG Go module | `engines/texsvg/go.mod` | No external Go module requirements in this module. The external TeX/diagram tools it invokes belong to the image package closure above. |
| Markdown Node graph | `engines/markdown/package-lock.json` | 126 pinned packages: 124 MIT, 1 ISC, 1 BSD-2-Clause according to lock metadata. Includes Shiki 4.4.2 (MIT). |
| MathJax Node graph | `engines/mathjax/package-lock.json` | 17 pinned packages: 5 Apache-2.0, 5 MIT, 7 BlueOak-1.0.0 according to lock metadata. `@mathjax/src` is Apache-2.0. |
| KaTeX Node graph | `engines/katex/package-lock.json` | 2 pinned packages, both MIT according to lock metadata. KaTeX's checked package `LICENSE` contains the MIT copyright and permission notice. |

No production secrets, user works, bundled extension fonts, or third-party binaries are part of the reviewed source candidate. Public CI must build from this source and preserve licenses/notice files from installed packages; Rinspace product integration must consume the exact reviewed artifact digests rather than rebuilding an altered private copy.
