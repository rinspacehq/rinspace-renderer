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
});

test('image sources build from either reviewed private subtree or public repository root', () => {
  for (const name of ['latexml', 'latex-pdf', 'typst-pdf']) {
    const dockerfile = read(`Dockerfile.${name}`).toString('utf8');
    const stages = [...dockerfile.matchAll(/^FROM\s.+$/gm)].length;
    const sourceArgs = [...dockerfile.matchAll(/^ARG RENDERER_SOURCE_ROOT=rin-renderer$/gm)].length;
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
  assert.deepEqual(candidate.packagePolicy.approvedPackages, []);
});

test('local image and compose require immutable inputs and bounded runtime', () => {
  const dockerfile = read('Dockerfile.runtime').toString('utf8');
  const compose = read('local.compose.yml').toString('utf8');
  assert.match(dockerfile, /node@sha256:[a-f0-9]{64}/);
  assert.match(dockerfile, /^ARG RENDERER_BASE_IMAGE$/m);
  assert.match(dockerfile, /^FROM --platform=linux\/amd64 \$\{RENDERER_BASE_IMAGE\}$/m);
  assert.match(compose, /RIN_RENDERER_IMAGE:\?exact reviewed OCI digest required/);
  assert.match(compose, /RIN_RENDERER_COMPLETION_MODE: local/);
  assert.match(compose, /RIN_RENDERER_STORAGE_PROVIDER: local/);
  assert.match(compose, /RIN_RENDERER_WORKER_CONCURRENCY: "1"/);
  assert.match(compose, /network_mode: host/);
  assert.match(compose, /127\.0\.0\.1:8090/);
});
