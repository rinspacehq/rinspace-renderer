import { strict as assert } from 'node:assert';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { collectGoNotices } from './collect-go-notices.mjs';
import { verifyNoticeLock } from './fetch-notice-sources.mjs';
import { generateImageSBOM } from './generate-image-sbom.mjs';

test('release evidence rejects mutable references and untrusted notice sources', async () => {
  const lock = JSON.parse(await readFile(new URL('./notice-sources.lock.json', import.meta.url)));
  assert.equal(verifyNoticeLock(lock).sources.length, 5);
  const changed = structuredClone(lock);
  changed.sources[0].url = 'https://example.com/license';
  assert.throws(() => verifyNoticeLock(changed), /untrusted/);
  const directory = await mkdtemp(join(tmpdir(), 'renderer-evidence-test-'));
  try {
    const dpkg = join(directory, 'dpkg.tsv');
    await writeFile(dpkg, 'ca-certificates\t1.0\nlibc6\t2.0\n');
    await assert.rejects(generateImageSBOM({ component: 'runtime', reference: 'ghcr.io/rinspacehq/rinspace-renderer/runtime:latest', dpkgPath: dpkg }), /exact component OCI digest/);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test('runtime SBOM includes pinned Node and Go packages and exact image identity', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'renderer-evidence-test-'));
  try {
    const dpkg = join(directory, 'dpkg.tsv');
    const go = join(directory, 'go.tsv');
    await writeFile(dpkg, 'ca-certificates\t1.0\nlibc6\t2.0\n');
    await writeFile(go, 'github.com/rinspacehq/rinspace-renderer/api@|/tmp/main\nexample.org/module@v1.0.0|/tmp/module\n');
    const digest = 'a'.repeat(64);
    const result = await generateImageSBOM({ component: 'runtime', reference: `ghcr.io/rinspacehq/rinspace-renderer/runtime@sha256:${digest}`, dpkgPath: dpkg, goModulesPath: go });
    assert.match(result.documentNamespace, new RegExp(`${digest}$`));
    assert.ok(result.packages.some(item => item.name === 'example.org/module'));
    assert.ok(result.packages.some(item => item.name === 'markdown/remark-math' && item.licenseDeclared === 'MIT'));
    assert.ok(result.packages.some(item => item.name === 'LaTeXML'));
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test('Go notices require license text and record copied byte hashes', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'renderer-evidence-test-'));
  try {
    const moduleDir = join(directory, 'module');
    await mkdir(moduleDir);
    const list = join(directory, 'go.tsv');
    await writeFile(list, `github.com/rinspacehq/rinspace-renderer/api@|${directory}\nexample.org/module@v1.0.0|${moduleDir}\n`);
    await assert.rejects(collectGoNotices(list, join(directory, 'out')), /missing Go module license/);
    await writeFile(join(moduleDir, 'LICENSE'), 'Example license\n');
    assert.equal(await collectGoNotices(list, join(directory, 'out')), 1);
    const index = JSON.parse(await readFile(join(directory, 'out', 'index.json')));
    assert.equal(index.entries[0].notices.length, 1);
    assert.match(index.entries[0].notices[0].sha256, /^[a-f0-9]{64}$/);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
