# Licensing

The Rinspace-owned source files in this repository are licensed under **GNU Affero General Public License version 3 only** (`AGPL-3.0-only`). The full license text is in [`LICENSE`](LICENSE). This matches the license choice of `rinspacehq/rinspace-web`; it does not transfer that repository's asset or contributor rights to Renderer.

Third-party dependencies retain their own licenses. The Node and Go package graphs are pinned by `package-lock.json`, `go.mod`, and `go.sum`; LaTeXML, Typst, TeXLive, fonts, and base image packages are separate engine or image inputs. Their versions and source license evidence are summarized in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md). Each distributed binary or image must carry the corresponding exact dependency notices and SBOM for its digest. The `extension/code-server` client and its bundled KaTeX fonts are outside the initial public Renderer source boundary and are not covered by this grant.

The AGPL grant does not convey Rinspace trademarks, service credentials, private product data, or production deployment configuration. Contributions and any alternative commercial license require a separate written agreement with the rights holder; a public pull request is not automatically accepted into the private product.
