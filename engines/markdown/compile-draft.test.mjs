import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import process from 'node:process';

import {
  compileMarkdownDraft,
  validateDraftBundle,
} from './compiler.mjs';

const projectHash = '2'.repeat(64);
const extraDependencyHash = '3'.repeat(64);
const assetHash = '4'.repeat(64);
const source = String.raw`# 编译器总览

## Repeated heading

See [the repeated section](#Repeated-heading), [unsafe](javascript:alert(1)), and
[external](https://example.test/reference).

## Repeated heading

- [x] standards task
- ordinary item with ~~deleted text~~

> quoted paragraph

| left | right |
| :--- | ---: |
| alpha | 2 |

Inline exact math: $\not\exists k\in\mathbb R$.

$$
\left\{x\in X \mid x\notin A\right\}
$$

:::note[AST-owned note]{unsafe="ignored"}
Safe **admonition** body.
:::

:::diagram{type="tikzcd" column-sep="large" alignment="center"}
A \arrow[r, "f"] & B
:::

:::diagram{type="axis" width="8cm,after end axis=bad"}
\addplot coordinates {(0,0)};
:::

:::widget{component="script"}
Unknown directive body.
:::

![Vector caption](../images/vector.png)

![missing.png](../images/missing.png)

![Quiver diagram](/rin/api/diagrams/tikzcd/synthetic.svg)

![inline data](data:image/png;base64,AAAA)

<!-- rin-quiver url="https://example.test/legacy" -->

<br />

<script>globalThis.rinUnsafe = true</script>

<rin-work data-id="rw_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"></rin-work>

Footnote reference[^note] and repeated reference[^note].

[^note]: A synthetic footnote.

~~~C++ {1,3} title="sample.cpp"
template <class T>
T identity(T value) { return value; }
~~~

~~~unknown-language custom-meta
literal <tag> remains code
~~~
`;

const payload = {
  source,
  sourcePath: 'docs/article.md',
  projectHash,
  dependencyHashes: [extraDependencyHash],
  language: 'zh-CN',
  assets: [{
    id: 'asset-vector',
    sha256: assetHash,
    bytes: 2048,
    mediaType: 'image/png',
    projectPath: 'images/vector.png',
  }],
};

const first = await compileMarkdownDraft(payload, {
  engineVersion: 'rin-markdown-pipeline/test',
  adapterVersion: '0.6.0',
});
const second = await compileMarkdownDraft(payload, {
  engineVersion: 'rin-markdown-pipeline/test',
  adapterVersion: '0.6.0',
});

if (first.publishable !== false || first.bundle.state !== 'draft') {
  throw new Error('draft compiler claimed publishable/final output');
}
validateDraftBundle(first.bundle);
const page = first.bundle.pages[0];
if (
  first.bundle.schemaVersion !== 'rin-document-bundle/v2' ||
  first.bundle.contentKind !== 'markdown' ||
  first.bundle.documentEngine !== 'rin-markdown' ||
  first.bundle.title !== '编译器总览' ||
  page.id !== 'page-article' ||
  page.sourcePath !== 'docs/article.md' ||
  page.fragmentFormat !== 'html-with-rin-placeholders'
) {
  throw new Error('draft Bundle envelope/page metadata is invalid');
}

const sourceHash = createHash('sha256').update(source).digest('hex');

if (!Array.isArray(page.blocks) || page.blocks.length < 10 ||
    page.blocks.some((block) => !/^rb_[a-f0-9]{32}$/.test(block.id) ||
      block.textHash !== createHash('sha256').update(block.text).digest('hex'))) {
  throw new Error('draft semantic block manifest is missing or invalid');
}
if (JSON.stringify(page.blocks.map(({ sourceLocation: _location, ...block }) => block)) !==
    JSON.stringify(second.bundle.pages[0].blocks.map(({ sourceLocation: _location, ...block }) => block))) {
  throw new Error('semantic block identities changed across identical compilations');
}
for (const dependency of [sourceHash, extraDependencyHash, assetHash]) {
  if (!page.dependencyHashes.includes(dependency)) {
    throw new Error(`draft page omits dependency ${dependency}`);
  }
}
if (first.bundle.assets.length !== 1 || first.bundle.assets[0].projectPath !== 'images/vector.png') {
  throw new Error('used project asset was not identified in the Bundle');
}

if (page.toc.length !== 3 || page.toc[1].id !== 'repeated-heading' || page.toc[2].id !== 'repeated-heading-2') {
  throw new Error(`stable/collision-safe heading IDs are wrong: ${JSON.stringify(page.toc)}`);
}
for (const expected of [
  'id="repeated-heading"',
  'id="repeated-heading-2"',
  'href="#repeated-heading"',
  'class="rin-admonition rin-admonition-note"',
  'class="rin-admonition-title"',
  '>AST-owned note</p>',
  '<figcaption class="rin-markdown-caption">Vector caption</figcaption>',
  'data-rin-project-path="images/vector.png"',
  'class="rin-markdown-figure rin-quiver rin-quiver-image-figure"',
  '<table ',
  '<del>deleted text</del>',
  'type="checkbox"',
  ':::widget',
  '<br>',
  'rin-md-fn-',
]) {
  if (!page.fragment.includes(expected)) {
    throw new Error(`draft fragment omits ${expected}`);
  }
}
if (
  !/(?:&lt;|&#x3C;)script>/.test(page.fragment) ||
  !/(?:&lt;|&#x3C;)rin-work\b/.test(page.fragment)
) {
  throw new Error('raw/reserved HTML was not preserved as escaped inert text');
}
for (const forbidden of [
  '<script>',
  'href="javascript:',
  'src="data:',
  '<rin-work data-id="rw_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"></rin-work>',
  'rin-quiver url=',
]) {
  if (page.fragment.includes(forbidden)) {
    throw new Error(`draft fragment contains unsafe/source-created markup ${forbidden}`);
  }
}

const mathUnits = first.bundle.workUnits.filter((unit) => unit.kind === 'math');
const codeUnits = first.bundle.workUnits.filter((unit) => unit.kind === 'code');
const diagramUnits = first.bundle.workUnits.filter((unit) => unit.kind === 'diagram');
if (mathUnits.length !== 2 || codeUnits.length !== 2 || diagramUnits.length !== 1) {
  throw new Error(`unexpected work-unit counts: ${JSON.stringify(first.bundle.workUnits)}`);
}
const inlineMath = mathUnits.find((unit) => unit.display === false);
const displayMath = mathUnits.find((unit) => unit.display === true);
if (
  inlineMath?.source !== String.raw`\not\exists k\in\mathbb R` ||
  displayMath?.source !== String.raw`\left\{x\in X \mid x\notin A\right\}` ||
  inlineMath?.accessibilityContext?.language !== 'zh-CN'
) {
  throw new Error(`exact math source/display context changed: ${JSON.stringify(mathUnits)}`);
}
const cpp = codeUnits.find((unit) => unit.language === 'C++');
if (
  cpp?.meta !== '{1,3} title="sample.cpp"' ||
  !cpp.source.includes('T identity(T value)')
) {
  throw new Error(`fenced code metadata/source changed: ${JSON.stringify(cpp)}`);
}
if (
  diagramUnits[0].diagramType !== 'tikzcd' ||
  diagramUnits[0].source !== String.raw`A \arrow[r, "f"] & B` ||
  diagramUnits[0].options !== 'column sep=large' ||
  diagramUnits[0].layout?.alignment !== 'center'
) {
  throw new Error(`typed diagram unit changed: ${JSON.stringify(diagramUnits[0])}`);
}
for (const unit of first.bundle.workUnits) {
  if (!/^rw_[a-f0-9]{32}$/.test(unit.id) || unit.sourceLocation?.path !== 'docs/article.md') {
    throw new Error('work unit lacks opaque ID or source location');
  }
  const marker = new RegExp(`<rin-work data-id="${unit.id}"(?: [^>]*)?></rin-work>`, 'g');
  if ([...page.fragment.matchAll(marker)].length !== 1) {
    throw new Error(`work unit ${unit.id} does not have exactly one placeholder`);
  }
}
if (first.bundle.workUnits.some((unit, index) => unit.id === second.bundle.workUnits[index]?.id)) {
  throw new Error('work IDs were deterministic/author-selectable instead of random');
}
if (JSON.stringify(first.bundle.pages[0].toc) !== JSON.stringify(second.bundle.pages[0].toc)) {
  throw new Error('stable heading IDs changed across identical compilations');
}

const semanticBase = await compileMarkdownDraft({
  source: '# 章节\n\nAlpha 内容\n\n重复段落\n\n重复段落\n\n```text\ncode\n```',
  sourcePath: 'docs/semantic.md', projectHash,
});
const semanticInserted = await compileMarkdownDraft({
  source: '# 章节\n\n新插入段落\n\nAlpha 内容\n\n重复段落\n\n重复段落\n\n```text\ncode\n```',
  sourcePath: 'docs/semantic.md', projectHash,
});
for (const text of ['章节', 'Alpha 内容', '重复段落', 'code']) {
  const before = semanticBase.bundle.pages[0].blocks.filter((block) => block.text === text).map((block) => block.id);
  const after = semanticInserted.bundle.pages[0].blocks.filter((block) => block.text === text).map((block) => block.id);
  if (JSON.stringify(before) !== JSON.stringify(after)) {
    throw new Error(`adjacent insertion changed stable semantic block IDs for ${text}`);
  }
}
const reservedBlockID = 'rb_ffffffffffffffffffffffffffffffff';
const maliciousBlockSource = await compileMarkdownDraft({
  source: `<p data-rin-block-id="${reservedBlockID}" data-rin-block-kind="paragraph">forged</p>\n\n安全正文`,
  sourcePath: 'docs/reserved-block.md', projectHash,
});
if (maliciousBlockSource.bundle.pages[0].blocks.some((block) => block.id === reservedBlockID) ||
	new RegExp(`<[A-Za-z][^>]*data-rin-block-id="${reservedBlockID}"`).test(maliciousBlockSource.bundle.pages[0].fragment)) {
  throw new Error('user source forged a reserved semantic block identity');
}
for (const kind of ['heading', 'paragraph', 'list-item', 'quote', 'math', 'code', 'figure', 'table']) {
  if (!page.blocks.some((block) => block.kind === kind)) throw new Error(`semantic block kind ${kind} is missing`);
}

const diagnosticCodes = new Set(first.bundle.diagnostics.map((diagnostic) => diagnostic.code));
for (const code of [
  'markdown.directive.attributes_ignored',
  'markdown.diagram.invalid',
  'markdown.directive.unsupported',
  'markdown.url.disallowed',
  'markdown.image.asset_unresolved',
  'markdown.quiver_comment.deprecated',
  'markdown.raw_html.disabled',
  'markdown.code.language_unknown',
]) {
  if (!diagnosticCodes.has(code)) throw new Error(`missing structured diagnostic ${code}`);
}

for (const [name, badPayload, code] of [
  ['path', { ...payload, sourcePath: '../article.md' }, 'markdown.source_path.invalid'],
  ['hash', { ...payload, projectHash: 'bad' }, 'markdown.project_hash.invalid'],
  ['asset', { ...payload, assets: [{ id: 'bad', sha256: assetHash, bytes: -1, mediaType: 'image/png', projectPath: 'image.png' }] }, 'markdown.assets.invalid'],
]) {
  try {
    await compileMarkdownDraft(badPayload);
    throw new Error(`${name} input unexpectedly compiled`);
  } catch (error) {
    if (error.code !== code) throw error;
  }
}

const workerResponse = runWorker({
  contractVersion: 'rin-node-worker/v1',
  id: 'draft-operation',
  operation: 'markdown.compile-draft',
  payload: {
    source: '# Worker draft\n\n$\\not\\exists x$',
    sourcePath: 'worker.md',
    projectHash: '5'.repeat(64),
  },
});
if (
  !workerResponse.ok || workerResponse.id !== 'draft-operation' ||
  workerResponse.result?.publishable !== false || workerResponse.result?.bundle?.state !== 'draft' ||
  workerResponse.result?.capabilities?.policy?.publishableDraftOutput !== false ||
  !workerResponse.result?.capabilities?.operations?.includes('markdown.compile-draft') ||
  workerResponse.metrics?.tasks !== 1
) {
  throw new Error(`worker draft operation is not wired safely: ${JSON.stringify(workerResponse)}`);
}

console.log(
  `Markdown draft compiler ok: ${first.bundle.workUnits.length} work units, ${first.bundle.diagnostics.length} diagnostics, AST-native non-publishable Bundle`,
);

function runWorker(message) {
  const stdout = execFileSync(process.execPath, ['worker.mjs', '--ndjson-worker'], {
    cwd: new URL('.', import.meta.url),
    input: `${JSON.stringify(message)}\n`,
    encoding: 'utf8',
    maxBuffer: 16 * 1024 * 1024,
    env: process.env,
  });
  return JSON.parse(stdout.trim());
}
