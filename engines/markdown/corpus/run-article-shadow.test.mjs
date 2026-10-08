import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { htmlSemantics, main, sourceFeatures } from './run-article-shadow.mjs';

const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'rin-markdown-shadow-'));
try {
  fs.writeFileSync(path.join(temp, 'article.md'), '# Title\n\n$\\not\\exists x$\n\n```js\nconst x = 1\n```\n');
  fs.writeFileSync(path.join(temp, 'manifest.json'), JSON.stringify({
    schemaVersion: 'rin-markdown-article-shadow-manifest/v1',
    provenance: { sourcePolicy: 'synthetic-test', containsPrivateSource: false, legacyBaseline: {
      schemaVersion: 'rin-markdown-legacy-baseline/v1', implementation: 'ui/src/utils/blogBody.ts',
      implementationGitBlob: 'a'.repeat(40), runtime: 'node-v22.0.0', katex: '0.16.0',
      warmupIterations: 1, measuredIterations: 3, latencyStatistic: 'median-wall-clock-ms',
      collectedAt: '2026-08-10T00:00:00Z',
    } },
    articles: [{ id: 'article-1', title: 'Article', source: 'article.md', legacyRenderMs: 2.5,
      legacyHtmlBytes: 100, expectedFeatures: ['math', 'code'] }],
  }));
  await main(['--manifest', path.join(temp, 'manifest.json'), '--validate-only']);

  const source = sourceFeatures('$\\not\\exists x$\n\n```js\nx\n```\n![a](https://example.invalid/a.png)');
  assert.equal(source.notExists, 1);
  assert.ok(source.math > 0);
  assert.ok(source.code > 0);
  assert.equal(source.images, 1);

  const fenced = sourceFeatures('```sh\necho "$HOME"\n![not prose](bad)\n```');
  assert.equal(fenced.math, 0);
  assert.equal(fenced.images, 0);
  assert.equal(sourceFeatures('`$inlineCode` and $actual$').math, 1);
  assert.equal(sourceFeatures('    echo $currentProgram').math, 0);

  fs.writeFileSync(path.join(temp, 'public.json'), JSON.stringify({
    schemaVersion: 'rin-markdown-article-shadow-manifest/v1',
    provenance: { sourcePolicy: 'public-test', containsPrivateSource: false },
    articles: [{ id: 'public-1', title: 'Public', sourceUrl: 'https://rinspace.com/api/content/1',
      expectedSourceSha256: createHash('sha256').update('# Public').digest('hex') }],
  }));
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response(JSON.stringify({
    type: 'blog', editor: 'markdown', sourceVisibility: 'open',
    body: '[[RIN_MARKDOWN_SOURCE]]\n# Public\n[[/RIN_MARKDOWN_SOURCE]]',
  }), { status: 200, headers: { 'content-type': 'application/json' } });
  try {
    await main(['--manifest', path.join(temp, 'public.json'), '--validate-only']);
  } finally {
    globalThis.fetch = originalFetch;
  }

  const changed = JSON.parse(fs.readFileSync(path.join(temp, 'public.json'), 'utf8'));
  changed.articles[0].expectedSourceSha256 = '0'.repeat(64);
  fs.writeFileSync(path.join(temp, 'public-changed.json'), JSON.stringify(changed));
  globalThis.fetch = async () => new Response(JSON.stringify({
    type: 'blog', editor: 'markdown', sourceVisibility: 'open',
    body: '[[RIN_MARKDOWN_SOURCE]]\n# Public\n[[/RIN_MARKDOWN_SOURCE]]',
  }), { status: 200, headers: { 'content-type': 'application/json' } });
  try {
    await assert.rejects(() => main(['--manifest', path.join(temp, 'public-changed.json'), '--validate-only']), /public source identity changed/);
  } finally {
    globalThis.fetch = originalFetch;
  }

  fs.writeFileSync(path.join(temp, 'private-url.json'), JSON.stringify({
    schemaVersion: 'rin-markdown-article-shadow-manifest/v1',
    provenance: { sourcePolicy: 'private-test', containsPrivateSource: true },
    articles: [{ id: 'private-1', title: 'Private', sourceUrl: 'https://rinspace.com/api/content/1' }],
  }));
  await assert.rejects(() => main(['--manifest', path.join(temp, 'private-url.json'), '--validate-only']), /allowed source location/);

  fs.writeFileSync(path.join(temp, 'unsafe-url.json'), JSON.stringify({
    schemaVersion: 'rin-markdown-article-shadow-manifest/v1',
    provenance: { sourcePolicy: 'public-test', containsPrivateSource: false },
    articles: [{ id: 'unsafe-1', title: 'Unsafe', sourceUrl: 'https://rinspace.com/api/content/1?preview=true' }],
  }));
  await assert.rejects(() => main(['--manifest', path.join(temp, 'unsafe-url.json'), '--validate-only']), /not allowlisted/);

  const safe = htmlSemantics('<h2>Title</h2><span class="rin-math rin-math-inline"></span><pre class="shiki"><code>x</code></pre>');
  assert.equal(safe.counts.headings, 1);
  assert.equal(safe.counts.math, 1);
  assert.equal(safe.counts.shiki, 1);
  assert.deepEqual(safe.safety, { blockedElements: false, eventHandlers: false, unsafeURLs: false, unresolvedWork: false });

  fs.writeFileSync(path.join(temp, 'invalid.json'), JSON.stringify({
    schemaVersion: 'rin-markdown-article-shadow-manifest/v1',
    provenance: { sourcePolicy: 'synthetic-test', containsPrivateSource: false },
    articles: [{ id: 'escape', title: 'Escape', source: '../outside.md' }],
  }));
  await assert.rejects(() => main(['--manifest', path.join(temp, 'invalid.json'), '--validate-only']), /escapes its directory|not found/);
} finally {
  fs.rmSync(temp, { recursive: true, force: true });
}

console.log('Markdown article shadow contract ok');
