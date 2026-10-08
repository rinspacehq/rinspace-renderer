#!/usr/bin/env node
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import rehypeParse from 'rehype-parse';
import { unified } from 'unified';

import { compileMarkdownDraft } from '../compiler.mjs';
import { MARKDOWN_SANITIZER_VERSION, finalizeMarkdownBundle } from '../finalizer.mjs';
import { pipelineCapabilities, RIN_MDAST_PLUGINS } from '../pipeline.mjs';
import { APPROVED_SHIKI_THEME, resolveShikiBatch } from '../shiki.mjs';

const corpusDirectory = path.dirname(fileURLToPath(import.meta.url));
const engineDirectory = path.dirname(corpusDirectory);
const requiredFeatures = Object.freeze([
  'article', 'book', 'multi-page', 'cjk', 'gfm', 'directives', 'footnotes', 'links',
  'images', 'code', 'math', 'diagrams', 'malformed-input', 'security',
]);
const countClasses = new Set([
  'contains-task-list', 'data-footnote-backref', 'footnotes', 'rin-admonition',
  'rin-markdown-caption', 'rin-markdown-figure', 'rin-markdown-image', 'rin-math',
  'rin-math-display', 'rin-math-inline', 'rin-reader-diagram', 'shiki', 'task-list-item',
]);

await main();

async function main() {
  const options = parseArgs(process.argv.slice(2));
  const fixturesDirectory = path.resolve(options.fixtures || path.join(corpusDirectory, 'fixtures'));
  const baselinePath = path.resolve(options.baseline || path.join(corpusDirectory, 'baseline.json'));
  const outPath = path.resolve(options.out || '.rin-markdown-corpus/report.json');
  const deltasPath = options.deltasOut ? path.resolve(options.deltasOut) : '';
  const baseline = JSON.parse(fs.readFileSync(baselinePath, 'utf8'));
  const fixtures = readFixtureSpecs(fixturesDirectory);
  const coverage = coverageFor(fixtures);
  const results = [];

  for (const fixture of fixtures) {
    try {
      const first = await renderFixture(fixture);
      const second = await renderFixture(fixture);
      if (JSON.stringify(first) !== JSON.stringify(second)) {
        const difference = firstSemanticDifference(first, second);
        throw new Error(`two identical renders produced different semantic records${difference ? ` (${difference})` : ''}`);
      }
      const invariantErrors = evaluateInvariants(fixture, first);
      results.push({
        id: fixture.id, kind: fixture.kind, features: fixture.features,
        status: invariantErrors.length ? 'failed' : 'passed', invariantErrors, semantic: first,
      });
    } catch (error) {
      results.push({
        id: fixture.id, kind: fixture.kind, features: fixture.features, status: 'failed',
        invariantErrors: [error instanceof Error ? error.message : String(error)], semantic: null,
      });
    }
  }

  const current = {
    schemaVersion: 'rin-markdown-corpus-report/v1',
    generatedAt: new Date().toISOString(),
    profile: corpusProfile(),
    coverage,
    summary: summarize(results, coverage),
    fixtures: results,
  };
  current.deltas = compareBaseline(baseline, current);
  writeJSON(outPath, current);
  if (deltasPath) writeJSON(deltasPath, current.deltas);
  console.log(`Markdown corpus summary: ${current.summary.passed} passed, ${current.summary.failed} failed; deltas=${current.deltas.summary.total}`);
  console.log(`Markdown corpus delta hash: ${current.deltas.hash}`);
  if (options.printBaseline === 'true') {
    console.log(JSON.stringify(comparableReport(current), null, 2));
  }
  if (current.summary.failed || coverage.missing.length) process.exitCode = 1;
}

function firstSemanticDifference(left, right, currentPath = '') {
  if (JSON.stringify(left) === JSON.stringify(right)) return '';
  if (Array.isArray(left) && Array.isArray(right)) {
    const length = Math.max(left.length, right.length);
    for (let index = 0; index < length; index += 1) {
      const difference = firstSemanticDifference(left[index], right[index], `${currentPath}[${index}]`);
      if (difference) return difference;
    }
  }
  if (isRecord(left) && isRecord(right)) {
    for (const key of [...new Set([...Object.keys(left), ...Object.keys(right)])].sort()) {
      const difference = firstSemanticDifference(left[key], right[key], currentPath ? `${currentPath}.${key}` : key);
      if (difference) return difference;
    }
  }
  return `${currentPath || '<root>'}: ${JSON.stringify(left)} != ${JSON.stringify(right)}`;
}

async function renderFixture(fixture) {
  const projectFiles = readProjectFiles(fixture.projectDirectory);
  const projectHash = hashProject(projectFiles);
  const assets = projectFiles.filter((file) => !/\.md$/i.test(file.projectPath)).map(assetFromFile);
  const pages = [];
  for (const sourcePath of fixture.entrypoints) {
    const sourceFile = projectFiles.find((file) => file.projectPath === sourcePath);
    if (!sourceFile) throw new Error(`missing entrypoint ${sourcePath}`);
    const dependencyHashes = projectFiles.map((file) => file.sha256).sort();
    const draft = await compileMarkdownDraft({
      source: sourceFile.body.toString('utf8'), sourcePath, projectHash,
      language: fixture.language, assets, dependencyHashes,
    }, { adapterVersion: pipelineCapabilities().engineVersion, engineVersion: pipelineCapabilities().pipelineVersion });
    const work = workSemantics(draft.bundle.workUnits);
    const resolvedWork = await resolveCorpusWork(draft.bundle.workUnits);
    const final = await finalizeMarkdownBundle({ bundle: draft.bundle, resolvedWork }, {
      adapterVersion: pipelineCapabilities().engineVersion,
      engineVersion: pipelineCapabilities().pipelineVersion,
    });
    pages.push(pageSemantics(sourcePath, draft.bundle, final, work));
  }
  return {
    pageCount: pages.length,
    pages,
    projectAssets: assets.map(({ projectPath, sha256, bytes, mediaType }) => ({ projectPath, sha256, bytes, mediaType })),
  };
}

async function resolveCorpusWork(workUnits) {
  const units = {};
  const codeUnits = workUnits.filter((unit) => unit.kind === 'code');
  const codeResults = codeUnits.length ? await resolveShikiBatch({
    contractVersion: 'rin-shiki-batch/v1', theme: APPROVED_SHIKI_THEME,
    items: codeUnits.map(({ id, source, language }) => ({ id, source, language: language || '' })),
  }) : { items: [] };
  const codeByID = new Map(codeResults.items.map((item) => [item.id, item]));
  for (const unit of workUnits) {
    if (unit.kind === 'code') {
      const result = codeByID.get(unit.id);
      units[unit.id] = {
        id: unit.id, kind: unit.kind, html: result.html, css: [],
        diagnostics: result.diagnostics.map((diagnostic) => ({ ...diagnostic, stage: 'code' })),
      };
      continue;
    }
    if (unit.kind === 'math') {
      const display = unit.display ? ' display="true"' : '';
      units[unit.id] = {
        id: unit.id, kind: unit.kind,
        html: `<span class="rin-math ${unit.display ? 'rin-math-display' : 'rin-math-inline'} rin-math-mathjax" data-rin-math-engine="mathjax-chtml" data-rin-math-source="${escapeAttribute(unit.source)}" data-rin-math-version="4.1.3"><mjx-container class="MathJax" jax="CHTML" data-latex="${escapeAttribute(unit.source)}"${display}><mjx-math><mjx-mi><mjx-c class="mjx-c2204"></mjx-c></mjx-mi></mjx-math></mjx-container></span>`,
        css: ['mjx-container[jax="CHTML"]{display:inline-block}\nmjx-c.mjx-c2204{padding:0.5em}'],
        diagnostics: [],
      };
      continue;
    }
    const artifactHash = sha256(JSON.stringify(canonicalize({
      type: unit.diagramType, source: unit.source, options: unit.options || '', layout: unit.layout || null,
    })));
    const artifactId = `diagrams/v1/svg-sha256/${artifactHash.slice(0, 2)}/${artifactHash}.svg`;
    const diagramClass = safeClass(unit.diagramType);
    const figure = `<figure class="rin-reader-diagram rin-reader-diagram-${diagramClass}" data-diagram-id="diagram-${artifactHash.slice(0, 16)}"><img src="https://assets.rinspace.invalid/${artifactId}" alt="${escapeAttribute(unit.diagramType)} diagram" loading="lazy" decoding="async" data-rin-diagram-object-id="${artifactId}"></figure>`;
    const alignment = unit.layout?.alignment ? safeClass(unit.layout.alignment) : '';
    units[unit.id] = {
      id: unit.id, kind: unit.kind,
      html: alignment ? `<div class="rin-align-block rin-align-${alignment}" data-rin-align="${alignment}">${figure}</div>` : figure,
      css: [], diagnostics: [],
      artifact: {
        artifactId, sha256: artifactHash, bytes: 256,
        mediaType: 'image/svg+xml; charset=utf-8', visibility: 'public',
      },
    };
  }
  return { units };
}

function pageSemantics(sourcePath, draft, final, work) {
  const page = final.bundle.pages[0];
  const tree = unified().use(rehypeParse, { fragment: true }).parse(page.fragment);
  const elementCounts = {};
  const classCounts = {};
  const dataAttributeCounts = {};
  const ids = [];
  const internalReferences = [];
  const safety = { scripts: false, handlers: false, unsafeURLs: false, placeholders: false };
  walk(tree, (node) => {
    if (node.type !== 'element') return;
    elementCounts[node.tagName] = (elementCounts[node.tagName] || 0) + 1;
    for (const className of classList(node)) {
      if (countClasses.has(className) || className.startsWith('language-')) {
        classCounts[className] = (classCounts[className] || 0) + 1;
      }
    }
    for (const name of Object.keys(node.properties || {})) {
      if (name.startsWith('data')) dataAttributeCounts[name] = (dataAttributeCounts[name] || 0) + 1;
      if (/^on/i.test(name)) safety.handlers = true;
    }
    if (['script', 'iframe', 'object', 'embed', 'form', 'template', 'svg', 'math'].includes(node.tagName)) safety.scripts = true;
    if (node.tagName === 'rin-work') safety.placeholders = true;
    if (node.properties?.id) ids.push(String(node.properties.id));
    const href = stringProperty(node.properties?.href);
    if (href.startsWith('#')) internalReferences.push(href);
    for (const name of ['href', 'src', 'cite']) {
      if (/^(?:javascript|vbscript|data|file):/i.test(stringProperty(node.properties?.[name]).trim())) safety.unsafeURLs = true;
    }
  });
  return {
    sourcePath,
    htmlSha256: sha256(page.fragment),
    htmlBytes: Buffer.byteLength(page.fragment),
    bundleHash: final.bundle.bundleHash,
    toc: page.toc,
    workUnits: work,
    diagnostics: final.bundle.diagnostics.map((item) => `${item.severity}:${item.stage}:${item.code}`).sort(),
    assets: final.bundle.assets.map(({ projectPath, sha256, bytes, mediaType }) => ({ projectPath, sha256, bytes, mediaType })),
    artifacts: final.artifacts.map(({ artifactId, sha256, bytes, mediaType, visibility }) => ({ artifactId, sha256, bytes, mediaType, visibility })),
    elementCounts: sortObject(elementCounts),
    classCounts: sortObject(classCounts),
    dataAttributeCounts: sortObject(dataAttributeCounts),
    ids: ids.sort(),
    internalReferences: internalReferences.sort(),
    safety,
  };
}

function workSemantics(units) {
  const counts = { math: 0, diagram: 0, code: 0, mathInline: 0, mathDisplay: 0 };
  const codeLanguages = [];
  const diagramTypes = [];
  const mathSourceHashes = [];
  for (const unit of units) {
    counts[unit.kind] += 1;
    if (unit.kind === 'math') {
      counts[unit.display ? 'mathDisplay' : 'mathInline'] += 1;
      mathSourceHashes.push(sha256(unit.source));
    }
    if (unit.kind === 'code') codeLanguages.push(unit.language || '');
    if (unit.kind === 'diagram') diagramTypes.push(unit.diagramType);
  }
  return {
    counts, codeLanguages: codeLanguages.sort(), diagramTypes: diagramTypes.sort(),
    mathSourceHashes: mathSourceHashes.sort(),
  };
}

function evaluateInvariants(fixture, semantic) {
  const errors = [];
  const invariant = fixture.invariants;
  if (semantic.pageCount !== fixture.entrypoints.length) errors.push('page count does not match entrypoints');
  const allPages = semantic.pages;
  const totalWork = sumRecords(allPages.map((page) => page.workUnits.counts));
  for (const [kind, minimum] of Object.entries(invariant.workUnitsAtLeast || {})) {
    if ((totalWork[kind] || 0) < minimum) errors.push(`work unit ${kind} count is below ${minimum}`);
  }
  const totalClasses = sumRecords(allPages.map((page) => page.classCounts));
  for (const [name, minimum] of Object.entries(invariant.classCountsAtLeast || {})) {
    if ((totalClasses[name] || 0) < minimum) errors.push(`class ${name} count is below ${minimum}`);
  }
  const diagnostics = new Set(allPages.flatMap((page) => page.diagnostics.map((item) => item.split(':').slice(2).join(':'))));
  for (const code of invariant.diagnosticCodesInclude || []) {
    if (!diagnostics.has(code)) errors.push(`missing diagnostic ${code}`);
  }
  const assetPaths = semantic.projectAssets.map((asset) => asset.projectPath).sort();
  if (JSON.stringify(assetPaths) !== JSON.stringify([...(invariant.assetPaths || [])].sort())) {
    errors.push('project asset paths do not match the fixture contract');
  }
  for (const page of allPages) {
    if (Object.values(page.safety).some(Boolean)) errors.push(`${page.sourcePath} failed final safety invariants`);
    if (page.ids.some((id) => !id.startsWith('rin-md-'))) errors.push(`${page.sourcePath} has an unnamespaced id`);
  }
  return errors;
}

function corpusProfile() {
  const capabilities = pipelineCapabilities();
  const mathJaxManifest = JSON.parse(fs.readFileSync(path.join(engineDirectory, '..', 'mathjax', 'package.json'), 'utf8'));
  return {
    pipelineVersion: capabilities.pipelineVersion,
    adapterVersion: capabilities.engineVersion,
    sanitizerVersion: MARKDOWN_SANITIZER_VERSION,
    plugins: [...RIN_MDAST_PLUGINS],
    shiki: { version: capabilities.shiki.version, theme: APPROVED_SHIKI_THEME },
    sharedMath: {
      engineVersion: mathJaxManifest.dependencies['@mathjax/src'],
      fontContract: `mathjax-newcm-${mathJaxManifest.dependencies['@mathjax/src']}`,
      fontURL: '/fonts/mathjax-newcm/woff2',
    },
    contractHashes: Object.fromEntries([
      'compiler.mjs', 'finalizer.mjs', 'pipeline.mjs', 'shiki.mjs', 'package-lock.json',
      'corpus/run-corpus.mjs', 'corpus/check-upgrade-gate.mjs', 'corpus/manifest.json',
      '../mathjax/package.json', '../mathjax/package-lock.json', '../mathjax/render-mathjax.mjs',
      '../../deploy/Dockerfile.latexml', '../../deploy/rin-renderer.env.example',
    ].map((relative) => [relative, sha256(fs.readFileSync(path.resolve(engineDirectory, relative)))])),
    sharedServiceHashes: Object.fromEntries([
      '../../api/internal/mathservice/service.go',
      '../../api/internal/diagramservice/directive.go', '../../api/internal/diagramservice/service.go',
      '../../api/internal/diagramservice/types.go',
      '../../api/internal/codeservice/service.go', '../../api/internal/codeservice/warm_shiki.go',
      '../../api/internal/orchestration/math.go', '../../api/internal/orchestration/diagram.go',
      '../../api/internal/orchestration/code.go', '../../api/internal/orchestration/work_resolver.go',
    ].map((relative) => [relative, sha256(fs.readFileSync(path.resolve(engineDirectory, relative)))])),
  };
}

function readFixtureSpecs(fixturesDirectory) {
  return fs.readdirSync(fixturesDirectory, { withFileTypes: true })
    .filter((entry) => entry.isDirectory()).map((entry) => entry.name).sort().map((directoryName) => {
      const directory = path.join(fixturesDirectory, directoryName);
      const spec = JSON.parse(fs.readFileSync(path.join(directory, 'corpus.json'), 'utf8'));
      if (spec.schemaVersion !== 'rin-markdown-corpus-fixture/v1' || spec.id !== directoryName ||
          !['article', 'book'].includes(spec.kind) || !Array.isArray(spec.entrypoints) || !spec.entrypoints.length ||
          !Array.isArray(spec.features) || !spec.features.length || typeof spec.invariants !== 'object') {
        throw new Error(`invalid Markdown corpus fixture ${directoryName}`);
      }
      return {
        ...spec, language: spec.language || 'en', directory,
        projectDirectory: path.join(directory, 'project'),
      };
    });
}

function readProjectFiles(directory) {
  const files = [];
  const visit = (current) => {
    for (const entry of fs.readdirSync(current, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
      const target = path.join(current, entry.name);
      if (entry.isDirectory()) visit(target);
      else if (entry.isFile()) {
        const body = fs.readFileSync(target);
        files.push({ projectPath: path.relative(directory, target).replaceAll(path.sep, '/'), body, sha256: sha256(body) });
      }
    }
  };
  visit(directory);
  return files.sort((left, right) => left.projectPath.localeCompare(right.projectPath));
}

function assetFromFile(file) {
  const extension = path.extname(file.projectPath).toLowerCase();
  const mediaTypes = { '.svg': 'image/svg+xml', '.png': 'image/png', '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg', '.webp': 'image/webp' };
  if (!mediaTypes[extension] || !file.body.length) throw new Error(`unsupported or empty corpus asset ${file.projectPath}`);
  return {
    id: `asset-${file.sha256.slice(0, 16)}`, kind: 'project-file', projectPath: file.projectPath,
    sha256: file.sha256, bytes: file.body.length, mediaType: mediaTypes[extension],
  };
}

function hashProject(files) {
  return sha256(JSON.stringify(files.map((file) => [file.projectPath, file.sha256, file.body.length])));
}

function coverageFor(fixtures) {
  const covered = [...new Set(fixtures.flatMap((fixture) => fixture.features))].sort();
  return { required: [...requiredFeatures], covered, missing: requiredFeatures.filter((feature) => !covered.includes(feature)) };
}

function compareBaseline(baseline, current) {
  if (baseline.schemaVersion !== 'rin-markdown-corpus-baseline/v1') throw new Error('invalid Markdown corpus baseline');
  const before = baseline.accepted;
  const after = comparableReport(current);
  const changes = [];
  compareValue(before, after, '', changes);
  const summary = {
    added: changes.filter((item) => item.type === 'added').length,
    removed: changes.filter((item) => item.type === 'removed').length,
    changed: changes.filter((item) => item.type === 'changed').length,
    total: changes.length,
  };
  return { schemaVersion: 'rin-markdown-corpus-deltas/v1', summary, hash: sha256(JSON.stringify(changes)), changes };
}

function comparableReport(report) {
  return {
    profile: report.profile, coverage: report.coverage,
    fixtures: report.fixtures.map(({ id, kind, features, status, invariantErrors, semantic }) => ({
      id, kind, features, status, invariantErrors, semantic,
    })),
  };
}

function compareValue(before, after, currentPath, changes) {
  if (JSON.stringify(before) === JSON.stringify(after)) return;
  if (before === undefined) return changes.push({ path: currentPath, type: 'added', before: null, after });
  if (after === undefined) return changes.push({ path: currentPath, type: 'removed', before, after: null });
  if (Array.isArray(before) || Array.isArray(after) || !isRecord(before) || !isRecord(after)) {
    changes.push({ path: currentPath, type: 'changed', before, after });
    return;
  }
  for (const key of [...new Set([...Object.keys(before), ...Object.keys(after)])].sort()) {
    compareValue(before[key], after[key], currentPath ? `${currentPath}.${key}` : key, changes);
  }
}

function summarize(results, coverage) {
  const failedFixtures = results.filter((item) => item.status !== 'passed').length;
  return { passed: results.length - failedFixtures, failed: failedFixtures + (coverage.missing.length ? 1 : 0), deterministic: failedFixtures === 0 };
}

function parseArgs(args) {
  const options = {};
  for (let index = 0; index < args.length; index += 1) {
    const argument = args[index];
    if (!argument.startsWith('--')) throw new Error(`unexpected argument ${argument}`);
    const [raw, inline] = argument.slice(2).split('=', 2);
    const key = raw.replace(/-([a-z])/g, (_match, character) => character.toUpperCase());
    if (inline !== undefined) options[key] = inline;
    else if (args[index + 1] && !args[index + 1].startsWith('--')) options[key] = args[++index];
    else options[key] = 'true';
  }
  return options;
}

function writeJSON(target, value) {
  fs.mkdirSync(path.dirname(target), { recursive: true });
  fs.writeFileSync(target, `${JSON.stringify(value, null, 2)}\n`);
}

function walk(node, visitor) {
  visitor(node);
  for (const child of node.children || []) walk(child, visitor);
}

function classList(node) {
  const value = node.properties?.className;
  return Array.isArray(value) ? value.map(String) : typeof value === 'string' ? value.split(/\s+/) : [];
}

function stringProperty(value) {
  if (Array.isArray(value)) return value.join(' ');
  return typeof value === 'string' || typeof value === 'number' ? String(value) : '';
}

function sumRecords(records) {
  const sum = {};
  for (const record of records) for (const [key, value] of Object.entries(record)) sum[key] = (sum[key] || 0) + value;
  return sum;
}

function sortObject(value) {
  return Object.fromEntries(Object.entries(value).sort(([left], [right]) => left.localeCompare(right)));
}

function safeClass(value) {
  return String(value).toLowerCase().replace(/[^a-z0-9_-]+/g, '-').replace(/^-+|-+$/g, '') || 'unknown';
}

function escapeAttribute(value) {
  return String(value).replaceAll('&', '&amp;').replaceAll('"', '&quot;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');
}

function sha256(value) {
  return createHash('sha256').update(value).digest('hex');
}

function canonicalize(value) {
  if (Array.isArray(value)) return value.map(canonicalize);
  if (isRecord(value)) return Object.fromEntries(Object.entries(value).sort(([a], [b]) => a.localeCompare(b)).map(([key, child]) => [key, canonicalize(child)]));
  return value;
}

function isRecord(value) {
  return value && typeof value === 'object' && !Array.isArray(value);
}
