import { createHash } from 'node:crypto';
import { readdir, readFile, stat } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const fixtureDirectory = fileURLToPath(
  new URL('./fixtures/current-behavior/', import.meta.url),
);
const manifestPath = path.join(fixtureDirectory, 'manifest.json');
const manifest = JSON.parse(await readFile(manifestPath, 'utf8'));

const allowedClassifications = new Set([
  'commonmark-gfm',
  'rin-plugin',
  'compatibility-only',
  'deprecated',
]);
const allowedDispositions = new Set(['retain', 'migrate', 'deprecate']);
const requiredSurfaces = new Set([
  'article',
  'book',
  'editor',
  'reader',
  'repository-import',
  'storage',
]);
const requiredBehaviors = new Set([
  'article-storage-source-only',
  'legacy-markdown-html-marker',
  'commonmark-blocks',
  'gfm-tables',
  'gfm-autolink',
  'gfm-task-list',
  'gfm-strikethrough',
  'footnotes',
  'stable-heading-ids',
  'admonition',
  'repository-import-parser',
  'math-source',
  'fenced-code-metadata',
  'client-katex-fallback',
  'client-shiki',
  'images-captions',
  'safe-links',
  'data-image',
  'quiver-image',
  'legacy-quiver-comment',
  'diagram-directive',
  'raw-latex-diagram-recovery',
  'standalone-break',
  'raw-html',
  'book-storage-version-0.1',
  'book-page-per-file',
]);
const behaviorFixturePaths = new Map([
  ['article-storage-source-only', 'article-storage.txt'],
  ['legacy-markdown-html-marker', 'article-storage.txt'],
  ...[
    'commonmark-blocks',
    'gfm-tables',
    'gfm-autolink',
    'gfm-task-list',
    'gfm-strikethrough',
    'footnotes',
    'stable-heading-ids',
    'admonition',
    'repository-import-parser',
  ].map((behavior) => [behavior, 'article-gfm.md']),
  ...[
    'math-source',
    'fenced-code-metadata',
    'client-katex-fallback',
    'client-shiki',
  ].map((behavior) => [behavior, 'math-and-code.md']),
  ...['images-captions', 'safe-links', 'data-image']
    .map((behavior) => [behavior, 'media-links.md']),
  ...[
    'quiver-image',
    'legacy-quiver-comment',
    'diagram-directive',
    'raw-latex-diagram-recovery',
  ].map((behavior) => [behavior, 'quiver-and-directives.md']),
  ...['standalone-break', 'raw-html'].map((behavior) => [behavior, 'raw-html.md']),
  ...['book-storage-version-0.1', 'book-page-per-file']
    .map((behavior) => [behavior, 'book-project.json']),
]);
const requiredSourceFragments = new Map([
  ['article-storage.txt', [
    '[[RIN_MARKDOWN_SOURCE]]',
    '[[/RIN_MARKDOWN_SOURCE]]',
    '[[RIN_MARKDOWN_FILE]]',
    '[[/RIN_MARKDOWN_FILE]]',
  ]],
  ['article-gfm.md', [
    '## Repeated heading',
    '- [x] completed task',
    '~~strikethrough~~',
    '> [!NOTE]',
    '| :--- | ---: | :---: |',
    '<https://example.test/reference>',
    '[^note]:',
  ]],
  ['math-and-code.md', [
    String.raw`$\not\exists k\in\mathbb R$`,
    String.raw`\left\{x\in X \mid x\notin A\right\}`,
    '```C++ {1,3} title="sample.cpp"',
    '```unknown-language',
  ]],
  ['media-links.md', [
    '"Explicit caption wins"',
    '../chapter/two.md#result',
    'javascript:alert(1)',
    'data:image/png;base64,AAAA',
  ]],
  ['quiver-and-directives.md', [
    '<!-- rin-quiver',
    '/rin/api/diagrams/tikzcd/',
    ':::note[',
    ':::diagram{type="tikzcd"}',
    String.raw`\begin{tikzcd}`,
  ]],
  ['raw-html.md', [
    '<br />',
    '<script>',
    'onerror=',
    '<iframe ',
  ]],
  ['book-project.json', [
    '"version": "0.1"',
    '"parentId": "md-001-introduction"',
    '\\"rin-deferred-math\\"',
  ]],
]);
const maximumFixtureBytes = 256 * 1024;

function invariant(condition, message) {
  if (!condition) {
    throw new Error(`current-behavior fixture invariant failed: ${message}`);
  }
}

function isSlug(value) {
  return typeof value === 'string' && /^[a-z0-9]+(?:[.-][a-z0-9]+)*$/.test(value);
}

function sorted(values) {
  return [...values].sort((left, right) => left.localeCompare(right));
}

function sameMembers(actual, expected) {
  const actualValues = sorted(actual);
  const expectedValues = sorted(expected);
  return actualValues.length === expectedValues.length
    && actualValues.every((value, index) => value === expectedValues[index]);
}

function verifySyntheticContent(fileName, source) {
  const forbiddenPatterns = [
    [/-----BEGIN (?:OPENSSH|RSA|EC) PRIVATE KEY-----/i, 'private key'],
    [/\bAKIA[0-9A-Z]{16}\b/, 'AWS access key'],
    [/\b(?:password|passwd|secret|access[_-]?token|api[_-]?key)\s*[:=]\s*[^\s"']+/i, 'credential-shaped assignment'],
    [/\bBearer\s+[A-Za-z0-9._~-]{16,}/i, 'bearer token'],
    [/(?:^|[\s"'])\/(?:home|Users)\/[^/\s"']+\//m, 'local user path'],
    [/\b(?:book260|production|prod-db)\b/i, 'production/private identifier'],
  ];
  for (const [pattern, label] of forbiddenPatterns) {
    invariant(!pattern.test(source), `${fileName} contains a ${label}`);
  }

  for (const match of source.matchAll(/https?:\/\/[^\s)<>"']+/g)) {
    const url = new URL(match[0]);
    invariant(
      url.hostname === 'example.test' || url.hostname.endsWith('.example.test'),
      `${fileName} contains non-synthetic host ${url.hostname}`,
    );
  }
}

invariant(
  manifest.schemaVersion === 'rin-markdown-current-behavior-fixtures/v1',
  'unexpected schemaVersion',
);
invariant(
  manifest.provenance?.sourcePolicy === 'synthetic-derived-from-public-regression-shapes',
  'fixtures must declare the synthetic public-regression source policy',
);
invariant(
  manifest.provenance?.containsPrivateSource === false,
  'containsPrivateSource must be explicitly false',
);
invariant(
  typeof manifest.provenance?.notes === 'string' && manifest.provenance.notes.length >= 40,
  'provenance notes must explain how private prose was excluded',
);
invariant(Array.isArray(manifest.cases) && manifest.cases.length > 0, 'cases must be non-empty');

const caseIds = new Set();
const fixturePaths = new Set();
const observedBehaviors = new Set();
const observedClassifications = new Set();
const observedSurfaces = new Set();
let expectationCount = 0;

for (const fixtureCase of manifest.cases) {
  invariant(isSlug(fixtureCase.id), `invalid case id ${JSON.stringify(fixtureCase.id)}`);
  invariant(!caseIds.has(fixtureCase.id), `duplicate case id ${fixtureCase.id}`);
  caseIds.add(fixtureCase.id);

  invariant(
    typeof fixtureCase.path === 'string'
      && fixtureCase.path === path.posix.basename(fixtureCase.path)
      && /^[a-z0-9][a-z0-9.-]*\.(?:md|json|txt)$/.test(fixtureCase.path),
    `${fixtureCase.id} has unsafe or unsupported path`,
  );
  invariant(!fixturePaths.has(fixtureCase.path), `duplicate fixture path ${fixtureCase.path}`);
  fixturePaths.add(fixtureCase.path);
  invariant(/^[a-f0-9]{64}$/.test(fixtureCase.sha256), `${fixtureCase.id} has invalid sha256`);

  invariant(Array.isArray(fixtureCase.surfaces) && fixtureCase.surfaces.length > 0, `${fixtureCase.id} has no surfaces`);
  invariant(
    new Set(fixtureCase.surfaces).size === fixtureCase.surfaces.length,
    `${fixtureCase.id} repeats a surface`,
  );
  for (const surface of fixtureCase.surfaces) {
    invariant(requiredSurfaces.has(surface), `${fixtureCase.id} has unknown surface ${surface}`);
    observedSurfaces.add(surface);
  }

  invariant(
    Array.isArray(fixtureCase.expectations) && fixtureCase.expectations.length > 0,
    `${fixtureCase.id} has no expectations`,
  );
  for (const expectation of fixtureCase.expectations) {
    invariant(isSlug(expectation.behavior), `${fixtureCase.id} has invalid behavior id`);
    invariant(
      !observedBehaviors.has(expectation.behavior),
      `duplicate behavior ${expectation.behavior}`,
    );
    observedBehaviors.add(expectation.behavior);
    invariant(
      behaviorFixturePaths.get(expectation.behavior) === fixtureCase.path,
      `${expectation.behavior} is assigned to the wrong fixture`,
    );
    invariant(
      allowedClassifications.has(expectation.classification),
      `${expectation.behavior} has unknown classification ${expectation.classification}`,
    );
    observedClassifications.add(expectation.classification);
    invariant(
      allowedDispositions.has(expectation.disposition),
      `${expectation.behavior} has unknown disposition ${expectation.disposition}`,
    );
    invariant(
      typeof expectation.note === 'string' && expectation.note.length >= 24,
      `${expectation.behavior} needs a concrete migration note`,
    );
    expectationCount += 1;
  }

  const fixturePath = path.join(fixtureDirectory, fixtureCase.path);
  const fixtureStat = await stat(fixturePath);
  invariant(fixtureStat.isFile(), `${fixtureCase.path} is not a regular file`);
  invariant(
    fixtureStat.size > 0 && fixtureStat.size <= maximumFixtureBytes,
    `${fixtureCase.path} must be between 1 and ${maximumFixtureBytes} bytes`,
  );
  const fixtureBuffer = await readFile(fixturePath);
  const digest = createHash('sha256').update(fixtureBuffer).digest('hex');
  invariant(digest === fixtureCase.sha256, `${fixtureCase.path} sha256 mismatch`);
  const source = fixtureBuffer.toString('utf8');
  invariant(!source.includes('\u0000'), `${fixtureCase.path} contains NUL`);
  verifySyntheticContent(fixtureCase.path, source);
  for (const fragment of requiredSourceFragments.get(fixtureCase.path) || []) {
    invariant(source.includes(fragment), `${fixtureCase.path} is missing syntax probe ${JSON.stringify(fragment)}`);
  }

  if (fixtureCase.path.endsWith('.json')) {
    const value = JSON.parse(source);
    invariant(value && typeof value === 'object' && !Array.isArray(value), `${fixtureCase.path} must contain an object`);
    if (fixtureCase.path === 'book-project.json') {
      invariant(value.version === '0.1', 'book-project.json must preserve storage version 0.1');
      invariant(Array.isArray(value.files) && value.files.length === 2, 'book-project.json must have two source files');
      invariant(Array.isArray(value.pages) && value.pages.length === value.files.length, 'book-project.json must keep one page per file');
      invariant(Array.isArray(value.toc) && value.toc.length === value.files.length, 'book-project.json must keep one TOC item per file');
      invariant(value.files.some((file) => file.level === 3 && file.parentId), 'book-project.json must cover nested level-3 files');
    }
  }
}

const directoryEntries = await readdir(fixtureDirectory, { withFileTypes: true });
const actualFixtureFiles = directoryEntries
  .filter((entry) => entry.isFile() && entry.name !== 'manifest.json')
  .map((entry) => entry.name);
invariant(
  sameMembers(actualFixtureFiles, fixturePaths),
  `manifest/file mismatch: manifest=${sorted(fixturePaths).join(',')} files=${sorted(actualFixtureFiles).join(',')}`,
);
invariant(
  sameMembers(observedBehaviors, requiredBehaviors),
  `behavior coverage mismatch: expected=${sorted(requiredBehaviors).join(',')} actual=${sorted(observedBehaviors).join(',')}`,
);
invariant(
  sameMembers(observedClassifications, allowedClassifications),
  'all four migration classifications must have evidence',
);
invariant(
  sameMembers(observedSurfaces, requiredSurfaces),
  'article, book, editor, reader, repository-import and storage must all have evidence',
);

console.log(
  `Current Markdown behavior fixtures ok: ${manifest.cases.length} cases, ${expectationCount} behaviors, ${observedSurfaces.size} surfaces, synthetic provenance verified`,
);
