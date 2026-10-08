import { strict as assert } from 'node:assert';
import { test } from 'node:test';
import { verifyReleaseManifest, verifyLocalFiles } from './verify-release-manifest.mjs';

const hex = 'a'.repeat(64);
const manifest = {
  schemaVersion: 'rin-renderer-release/v1', version: 'v0.1.0', sourceCommit: 'b'.repeat(40), platform: 'linux/amd64',
  protocol: { completionSchemaSha256: hex, openapiSha256: hex },
  artifacts: {
    runtimeImage: { reference: `ghcr.io/rinspacehq/rinspace-renderer/runtime@sha256:${hex}`, sbomSha256: hex, noticeSha256: hex },
    latexPdfImage: { reference: `ghcr.io/rinspacehq/rinspace-renderer/latex-pdf@sha256:${hex}`, sbomSha256: hex, noticeSha256: hex },
    typstPdfImage: { reference: `ghcr.io/rinspacehq/rinspace-renderer/typst-pdf@sha256:${hex}`, sbomSha256: hex, noticeSha256: hex },
    typstHtmlToolchain: { file: 'rin-renderer-typst-html-toolchain.tar.gz', sha256: hex, sbomSha256: hex, noticeSha256: hex },
  },
  capabilities: Object.fromEntries(['markdown', 'latexml', 'texsvg', 'latexPdf', 'typstPdf', 'typstHtml', 'localStorage', 'cloudbaseStorage', 'controlPlaneCompletion'].map(key => [key, true])),
};
const copy = () => structuredClone(manifest);

test('accepts a fully pinned complete artifact set', () => assert.equal(verifyReleaseManifest(copy()).version, 'v0.1.0'));
test('rejects mutable image tags and missing artifact checksums', () => {
  const tagged = copy(); tagged.artifacts.runtimeImage.reference = 'ghcr.io/rinspacehq/rinspace-renderer/runtime:latest';
  assert.throws(() => verifyReleaseManifest(tagged), /exact GHCR digest/);
  const missing = copy(); delete missing.artifacts.typstPdfImage.noticeSha256;
  assert.throws(() => verifyReleaseManifest(missing), /missing/);
});
test('rejects unverified capabilities and unknown fields', () => {
  const disabled = copy(); disabled.capabilities.typstHtml = false;
  assert.throws(() => verifyReleaseManifest(disabled), /must be verified/);
  const extra = copy(); extra.artifacts.runtimeImage.mutableTag = 'latest';
  assert.throws(() => verifyReleaseManifest(extra), /unknown/);
});
test('rejects source protocol mismatch', async () => {
  await assert.rejects(verifyLocalFiles(copy(), new URL('../', import.meta.url).pathname), /differs from source/);
});
