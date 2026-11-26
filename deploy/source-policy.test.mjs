import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const read = name => readFileSync(new URL(name, import.meta.url));
const json = name => JSON.parse(read(name));

test('compiler wrappers match their source locks', () => {
  for (const [name, wrapper] of [
    ['latex-pdf', 'rinspace-latex-compile'],
    ['typst-pdf', 'rinspace-typst-compile'],
  ]) {
    const lock = json(`${name}.lock.json`);
    const digest = createHash('sha256').update(read(wrapper)).digest('hex');
    assert.equal(digest, lock.compiler.sha256, `${name} compiler source changed without a lock update`);
    assert.match(lock.baseImage.reference, /@sha256:[a-f0-9]{64}$/);
    assert.equal(lock.baseImage.reference.endsWith(lock.baseImage.platformDigest), true);
  }
  const recovery = createHash('sha256').update(read('rinspace-latex-recover-source.pl')).digest('hex');
  assert.equal(recovery, json('latex-pdf.lock.json').compiler.legacySourceRecovery.sha256);
});

test('LaTeX compatibility archives match the image and source SPDX pins', () => {
  const { texlive } = json('latex-pdf.lock.json');
  const dockerfile = read('Dockerfile.latex-pdf').toString('utf8');
  const archiveBase = /^ARG TEXLIVE_ARCHIVE=(.+)$/m.exec(dockerfile)[1];
  const spdx = json('latex-pdf.source.spdx.json');
  const artifacts = { ...texlive.compatibilityLicenseSources };
  const bibliography = texlive.bibliography;
  artifacts[`${bibliography.sourcePackage}.deb`] = bibliography.archive;
  for (const [name, { archive, documentation, revision }] of Object.entries(texlive.compatibilityPackages)) {
    artifacts[`${name}.tar.xz`] = archive;
    artifacts[`${name}.doc.tar.xz`] = documentation;
    const record = spdx.packages.find(item => item.SPDXID === `SPDXRef-TeXLive-${name}`);
    assert.equal(record.versionInfo, revision);
    assert.equal(record.downloadLocation, archive.url);
    assert.deepEqual(record.checksums, [{ algorithm: 'SHA256', checksumValue: archive.sha256 }]);
  }
  for (const [name, version] of Object.entries(bibliography.packageVersions)) {
    const record = spdx.packages.find(item => item.SPDXID === `SPDXRef-TeXLive-${name}`);
    assert.equal(record.versionInfo, version);
    assert.equal(record.downloadLocation, bibliography.archive.url);
    assert.deepEqual(record.checksums, [{ algorithm: 'SHA256', checksumValue: bibliography.archive.sha256 }]);
  }
  const imageArtifacts = Object.fromEntries([...dockerfile.matchAll(/^ADD --checksum=sha256:([a-f0-9]{64}) (\S+) \/archives\/(\S+)$/gm)]
    .map(([, sha256, source, name]) => [name, { url: source.replace('${TEXLIVE_ARCHIVE}', archiveBase), sha256 }]));
  assert.deepEqual(imageArtifacts, artifacts);
});

test('image sources default to the sole public repository root', () => {
  for (const name of ['latexml', 'latex-pdf', 'typst-pdf']) {
    const dockerfile = read(`Dockerfile.${name}`).toString('utf8');
    const stages = [...dockerfile.matchAll(/^FROM\s.+$/gm)].length;
    const sourceArgs = [...dockerfile.matchAll(/^ARG RENDERER_SOURCE_ROOT=\.$/gm)].length;
    assert.equal(sourceArgs, stages, `${name} has a stage without a source-root build argument`);
    for (const line of dockerfile.split('\n').filter(line => line.startsWith('COPY ') && !line.includes('--from='))) {
      assert.match(line, /\$\{RENDERER_SOURCE_ROOT\}\//, `${name} has a nonportable local COPY`);
      assert.doesNotMatch(line, /\s(?:server|ui|contracts)\//, `${name} copies private product source`);
    }
  }
});

test('Typst compiler remains pinned and offline', () => {
  const lock = json('typst-pdf.lock.json');
  const candidate = json('typst.candidate.lock.json');
  assert.equal(lock.compiler.version, candidate.compiler.version);
  assert.equal(lock.compiler.binarySha256, candidate.compiler.binarySha256);
  assert.equal(candidate.packagePolicy.downloadsInWorker, false);
  assert.equal(candidate.packagePolicy.externalImports, false);
  const html = json('typst-html-toolchain.lock.json');
  assert.deepEqual(candidate.packagePolicy.approvedPackages, html.packages.approved.map(item => `${item.name}@${item.version}`));
  assert.equal(candidate.packagePolicy.packageSetDigest, html.packages.packageSetDigest);
  assert.deepEqual(lock.previewPackageCache, {
    lock: 'typst-html-toolchain.lock.json', packageSetDigest: html.packages.packageSetDigest,
  });
  assert.equal(lock.runtime.packageDownloads, false);
  assert.equal(lock.runtime.network, 'none');
  assert.equal(lock.limits.nanoCpus, 1_000_000_000);
  assert.equal(lock.limits.memoryBytes, 768 * 1024 * 1024);
  assert.equal(lock.limits.memorySwapBytes, lock.limits.memoryBytes);
  assert.ok(lock.compiler.arguments.includes('--jobs=1'));
  const dockerfile = read('Dockerfile.typst-pdf').toString('utf8');
  assert.ok(dockerfile.includes(`ARG TYPST_PACKAGE_SET_SHA256=${html.packages.packageSetDigest}\n`));
  assert.match(dockerfile, /COPY --from=typst-packages \/out\/packages/);
  assert.match(read('verify-typst-pdf-runtime.sh').toString('utf8'), /sealed Typst package cache digest mismatch/);
  assert.equal(html.profile.mathPresentation, 'mathjax-chtml');
  assert.equal(html.rendererEnvironment.defaults.RIN_RENDERER_TYPST_TIMEOUT_SECONDS, '120');
});

test('ordinary CI cannot silently skip real Typst HTML/PDF regressions', () => {
  const workflow = read('../.github/workflows/renderer-check.yml').toString('utf8');
  const prepare = read('prepare-typst-test-runtime.mjs').toString('utf8');
  assert.match(workflow, /node deploy\/prepare-typst-test-runtime\.mjs/);
  assert.match(workflow, />> "\$GITHUB_ENV"/);
  assert.match(prepare, /compiler\.assetSha256/);
  assert.match(prepare, /compiler\.binarySha256/);
  assert.match(prepare, /font\.packageSha256/);
  assert.match(prepare, /font\.sha256/);
  assert.match(prepare, /RIN_TYPST_TEST_BINARY=/);
  assert.match(prepare, /RIN_TYPST_TEST_FONT=/);
});

test('local image and compose require immutable inputs and bounded runtime', () => {
  const dockerfile = read('Dockerfile.runtime').toString('utf8');
  const compose = read('local.compose.yml').toString('utf8');
  assert.match(dockerfile, /node@sha256:[a-f0-9]{64}/);
  assert.match(dockerfile, /^ARG RENDERER_BASE_IMAGE$/m);
  assert.match(dockerfile, /^FROM --platform=linux\/amd64 \$\{RENDERER_BASE_IMAGE\}$/m);
  assert.match(dockerfile, /COPY --from=api-build \/out\/rin-renderer-api \/usr\/local\/bin\/rin-renderer-api/);
  assert.match(dockerfile, /COPY --from=texsvg-build \/out\/rin-renderer-texsvg \/usr\/local\/bin\/rin-renderer-texsvg/);
  assert.match(compose, /RIN_RENDERER_IMAGE:\?exact reviewed OCI digest required/);
  assert.match(compose, /RIN_RENDERER_COMPLETION_MODE: local/);
  assert.match(compose, /RIN_RENDERER_STORAGE_PROVIDER: local/);
  assert.match(compose, /RIN_RENDERER_WORKER_CONCURRENCY: "1"/);
  assert.match(compose, /network_mode: host/);
  assert.match(compose, /127\.0\.0\.1:8090/);
});
