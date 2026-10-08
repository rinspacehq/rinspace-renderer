import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import process from 'node:process';

import { compileMarkdownBookDraft } from './compiler.mjs';
import { finalizeMarkdownBundle, planMarkdownBookCache } from './finalizer.mjs';

const projectHash = 'a'.repeat(64);
const imageHash = 'b'.repeat(64);
const draft = await compileMarkdownBookDraft({
  projectHash,
  title: 'Linear Algebra',
  pages: [
    {
      pageId: 'md-introduction',
      sourcePath: 'chapters/introduction.md',
      title: 'Introduction',
      source: '# Introduction\n\n[Continue](basis.md#Vector%20Basis)\n\n![Plot](../assets/plot.png)',
      dependencyHashes: [],
    },
    {
      pageId: 'md-vector-basis',
      sourcePath: 'chapters/basis.md',
      title: 'Vector Basis',
      source: '# Vector Basis\n\n[Back](introduction.md#Introduction)\n\n```text\nx\n```',
      dependencyHashes: [],
    },
  ],
  assets: [{
    id: 'asset-plot', kind: 'project-file', sha256: imageHash, bytes: 123,
    mediaType: 'image/png', projectPath: 'assets/plot.png',
  }],
});

assert.equal(draft.bundle.pages.length, 2);
assert.deepEqual(draft.bundle.pages.map((page) => page.id), ['md-introduction', 'md-vector-basis']);
assert.deepEqual(draft.bundle.pages.map((page) => page.sourcePath), ['chapters/introduction.md', 'chapters/basis.md']);
assert.match(draft.bundle.pages[0].fragment, /href="chapters\/basis\.md#vector-basis"/);
assert.match(draft.bundle.pages[1].fragment, /href="chapters\/introduction\.md#introduction"/);
assert.match(draft.bundle.pages[0].fragment, /rin-book-navigation-next/);
assert.match(draft.bundle.pages[1].fragment, /rin-book-navigation-previous/);
assert.equal(draft.bundle.assets.length, 1);
assert.ok(draft.bundle.pages[0].dependencyHashes.includes(imageHash));
assert.ok(!draft.bundle.pages[1].dependencyHashes.includes(imageHash));

const resolvedWork = resolveCode(draft.bundle);
const final = await finalizeMarkdownBundle({ bundle: draft.bundle, resolvedWork });
assert.equal(final.bundle.pages.length, 2);
assert.match(final.bundle.pages[0].fragment, /href="chapters\/basis\.md#rin-md-vector-basis"/);
assert.match(final.bundle.pages[1].fragment, /href="chapters\/introduction\.md#rin-md-introduction"/);
assert.deepEqual(final.bundle.pages.map((page) => page.toc[0].id), [
  'rin-md-introduction', 'rin-md-vector-basis',
]);
assert.match(final.bundle.bundleHash, /^[a-f0-9]{64}$/);
assert.equal(final.pageCache.records.length, 2);
assert.match(final.pageCache.projectDependencyHash, /^[a-f0-9]{64}$/);
for (const record of final.pageCache.records) assert.match(record.pageHash, /^[a-f0-9]{64}$/);

const repeatedDraft = await compileMarkdownBookDraft({
  projectHash,
  title: 'Linear Algebra',
  pages: [
    {
      pageId: 'md-introduction', sourcePath: 'chapters/introduction.md', title: 'Introduction',
      source: '# Introduction\n\n[Continue](basis.md#Vector%20Basis)\n\n![Plot](../assets/plot.png)', dependencyHashes: [],
    },
    {
      pageId: 'md-vector-basis', sourcePath: 'chapters/basis.md', title: 'Vector Basis',
      source: '# Vector Basis\n\n[Back](introduction.md#Introduction)\n\n```text\nx\n```', dependencyHashes: [],
    },
  ],
  assets: [{ id: 'asset-plot', kind: 'project-file', sha256: imageHash, bytes: 123, mediaType: 'image/png', projectPath: 'assets/plot.png' }],
});
const repeatedResolved = resolveCode(repeatedDraft.bundle);
const firstPlan = planMarkdownBookCache({ bundle: draft.bundle, resolvedWork });
const repeatedPlan = planMarkdownBookCache({ bundle: repeatedDraft.bundle, resolvedWork: repeatedResolved });
assert.deepEqual(repeatedPlan, firstPlan, 'opaque random work IDs changed Book cache identities');
const pageCache = Object.fromEntries(final.pageCache.records.map((record) => [record.page.id, record]));
const repeatedFinal = await finalizeMarkdownBundle({
  bundle: repeatedDraft.bundle, resolvedWork: repeatedResolved, pageCache,
});
assert.deepEqual(repeatedFinal.bundle, final.bundle, 'all-cache finalization differs from clean full finalization');
assert.deepEqual(repeatedFinal.pageCache.reusedPageIds, ['md-introduction', 'md-vector-basis']);
const corruptPageCache = structuredClone(pageCache);
corruptPageCache['md-vector-basis'].page.fragment = '<article><script>alert(1)</script></article>';
const corruptFallback = await finalizeMarkdownBundle({
  bundle: repeatedDraft.bundle, resolvedWork: repeatedResolved, pageCache: corruptPageCache,
});
assert.deepEqual(corruptFallback.bundle, final.bundle, 'corrupt page cache did not fall back to clean finalization');
assert.deepEqual(corruptFallback.pageCache.reusedPageIds, ['md-introduction']);

const changedDraft = await compileMarkdownBookDraft({
  projectHash: 'd'.repeat(64),
  title: 'Linear Algebra',
  pages: [
    {
      pageId: 'md-introduction', sourcePath: 'chapters/introduction.md', title: 'Introduction',
      source: '# Introduction\n\n[Continue](basis.md#Vector%20Basis)\n\n![Plot](../assets/plot.png)', dependencyHashes: [],
    },
    {
      pageId: 'md-vector-basis', sourcePath: 'chapters/basis.md', title: 'Vector Basis',
      source: '# Vector Basis\n\nChanged body.\n\n[Back](introduction.md#Introduction)\n\n```text\nx\n```', dependencyHashes: [],
    },
  ],
  assets: [{ id: 'asset-plot', kind: 'project-file', sha256: imageHash, bytes: 123, mediaType: 'image/png', projectPath: 'assets/plot.png' }],
});
const changedResolved = resolveCode(changedDraft.bundle);
const changedPlan = planMarkdownBookCache({ bundle: changedDraft.bundle, resolvedWork: changedResolved });
assert.equal(changedPlan.pages[0].pageSemanticHash, firstPlan.pages[0].pageSemanticHash);
assert.notEqual(changedPlan.pages[1].pageSemanticHash, firstPlan.pages[1].pageSemanticHash);
const changedClean = await finalizeMarkdownBundle({ bundle: changedDraft.bundle, resolvedWork: changedResolved });
const changedIncremental = await finalizeMarkdownBundle({
  bundle: changedDraft.bundle,
  resolvedWork: changedResolved,
  pageCache: { 'md-introduction': pageCache['md-introduction'] },
});
assert.deepEqual(changedIncremental.bundle, changedClean.bundle, 'incremental Book differs from clean full render');
assert.deepEqual(changedIncremental.pageCache.reusedPageIds, ['md-introduction']);

const diagnosticDraft = await compileMarkdownBookDraft({
  projectHash: '9'.repeat(64),
  pages: [
    { pageId: 'diagnostic-one', sourcePath: 'one.md', source: '# One\n\n```js\none()\n```', dependencyHashes: [] },
    { pageId: 'diagnostic-two', sourcePath: 'two.md', source: '# Two\n\n```js\ntwo()\n```', dependencyHashes: [] },
  ],
  assets: [],
});
const diagnosticUnits = diagnosticDraft.bundle.workUnits.map((unit, index) => [unit.id, {
  id: unit.id, kind: unit.kind, html: `<pre class="rin-code-pre"><code>${index}</code></pre>`, css: [],
  diagnostics: [{ code: `code.order.${index}`, severity: 'warning', message: `order ${index}`, stage: 'code' }],
}]);
const diagnosticForward = await finalizeMarkdownBundle({
  bundle: diagnosticDraft.bundle, resolvedWork: { units: Object.fromEntries(diagnosticUnits) },
});
const diagnosticReverse = await finalizeMarkdownBundle({
  bundle: diagnosticDraft.bundle, resolvedWork: { units: Object.fromEntries([...diagnosticUnits].reverse()) },
});
assert.deepEqual(diagnosticReverse.bundle, diagnosticForward.bundle,
  'resolved-work object order changed final diagnostic order or Bundle hash');

const headingChangedDraft = await compileMarkdownBookDraft({
  projectHash: 'e'.repeat(64), title: 'Linear Algebra',
  pages: [
    {
      pageId: 'md-introduction', sourcePath: 'chapters/introduction.md', title: 'Introduction',
      source: '# Introduction\n\n[Continue](basis.md#Vector%20Basis)\n\n![Plot](../assets/plot.png)', dependencyHashes: [],
    },
    {
      pageId: 'md-vector-basis', sourcePath: 'chapters/basis.md', title: 'Vector Basis',
      source: '# Spanning Basis\n\n[Back](introduction.md#Introduction)\n\n```text\nx\n```', dependencyHashes: [],
    },
  ],
  assets: [{ id: 'asset-plot', kind: 'project-file', sha256: imageHash, bytes: 123, mediaType: 'image/png', projectPath: 'assets/plot.png' }],
});
const headingChangedPlan = planMarkdownBookCache({
  bundle: headingChangedDraft.bundle, resolvedWork: resolveCode(headingChangedDraft.bundle),
});
assert.notEqual(headingChangedPlan.pages[0].pageSemanticHash, firstPlan.pages[0].pageSemanticHash,
  'changed cross-page heading did not invalidate the referring page');
assert.notEqual(headingChangedPlan.pages[1].pageSemanticHash, firstPlan.pages[1].pageSemanticHash);
assert.notEqual(headingChangedPlan.globalMetadataHash, firstPlan.globalMetadataHash);

const reorderedDraft = await compileMarkdownBookDraft({
  projectHash, title: 'Linear Algebra',
  pages: [
    {
      pageId: 'md-vector-basis', sourcePath: 'chapters/basis.md', title: 'Vector Basis',
      source: '# Vector Basis\n\n[Back](introduction.md#Introduction)\n\n```text\nx\n```', dependencyHashes: [],
    },
    {
      pageId: 'md-introduction', sourcePath: 'chapters/introduction.md', title: 'Introduction',
      source: '# Introduction\n\n[Continue](basis.md#Vector%20Basis)\n\n![Plot](../assets/plot.png)', dependencyHashes: [],
    },
  ],
  assets: [{ id: 'asset-plot', kind: 'project-file', sha256: imageHash, bytes: 123, mediaType: 'image/png', projectPath: 'assets/plot.png' }],
});
const reorderedPlan = planMarkdownBookCache({ bundle: reorderedDraft.bundle, resolvedWork: resolveCode(reorderedDraft.bundle) });
assert.notEqual(reorderedPlan.globalMetadataHash, firstPlan.globalMetadataHash);
assert.notEqual(reorderedPlan.pages.find((page) => page.id === 'md-introduction').pageSemanticHash, firstPlan.pages[0].pageSemanticHash,
  'navigation order change did not invalidate the affected page');

const worker = JSON.parse(execFileSync(process.execPath, ['worker.mjs', '--ndjson-worker'], {
  cwd: new URL('.', import.meta.url),
  input: `${JSON.stringify({
    contractVersion: 'rin-node-worker/v1', id: 'book-operation', operation: 'markdown.compile-book-draft',
    payload: {
      projectHash: 'c'.repeat(64),
      pages: [
        { pageId: 'page-one', sourcePath: 'one.md', source: '# One', dependencyHashes: [] },
        { pageId: 'page-two', sourcePath: 'two.md', source: '# Two', dependencyHashes: [] },
      ],
    },
  })}\n`,
  encoding: 'utf8',
  maxBuffer: 16 * 1024 * 1024,
  env: process.env,
}).trim());
assert.equal(worker.ok, true);
assert.equal(worker.result.bundle.pages.length, 2);
assert.ok(worker.result.capabilities.operations.includes('markdown.compile-book-draft'));
const planWorker = JSON.parse(execFileSync(process.execPath, ['worker.mjs', '--ndjson-worker'], {
  cwd: new URL('.', import.meta.url),
  input: `${JSON.stringify({
    contractVersion: 'rin-node-worker/v1', id: 'book-cache-plan', operation: 'markdown.plan-book-cache',
    payload: { bundle: draft.bundle, resolvedWork },
  })}\n`,
  encoding: 'utf8', maxBuffer: 16 * 1024 * 1024, env: process.env,
}).trim());
assert.equal(planWorker.ok, true);
assert.deepEqual(planWorker.result, firstPlan);

const unicodeDraft = await compileMarkdownBookDraft({
  projectHash: 'f'.repeat(64),
  title: '测试 markdown 书籍',
  pages: [
    { pageId: 'md-003-啊啊啊', sourcePath: '03-啊啊啊.md', title: '啊啊啊', source: '# 啊啊啊', dependencyHashes: [] },
    { pageId: 'md-001-可爱捏-sec-001-非常可爱', sourcePath: '01-可爱捏/01-非常可爱.md', title: '非常可爱', source: '# 非常可爱', dependencyHashes: [] },
  ],
  assets: [],
});
const unicodeFinal = await finalizeMarkdownBundle({ bundle: unicodeDraft.bundle, resolvedWork: resolveCode(unicodeDraft.bundle) });
assert.deepEqual(unicodeDraft.bundle.pages.map((page) => page.id), ['md-003-啊啊啊', 'md-001-可爱捏-sec-001-非常可爱']);
assert.deepEqual(unicodeFinal.bundle.pages.map((page) => page.id), ['md-003-啊啊啊', 'md-001-可爱捏-sec-001-非常可爱']);

console.log('Markdown Book Bundle ok: ordered stable pages, navigation, TOC, assets, and cross-page links');

function resolveCode(bundle) {
  return {
    units: Object.fromEntries(bundle.workUnits.map((unit) => [unit.id, {
      id: unit.id,
      kind: unit.kind,
      html: '<pre class="rin-code-pre"><code>x</code></pre>',
      css: [],
      diagnostics: [],
    }])),
  };
}
