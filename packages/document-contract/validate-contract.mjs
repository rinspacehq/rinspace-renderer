#!/usr/bin/env node
import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.dirname(fileURLToPath(import.meta.url));
const schemaVersion = 'rin-project-graph/v1';
const bundleSchemaVersions = new Set(['rin-document-bundle/v1', 'rin-document-bundle/v2']);
const validContentKinds = new Set(['latex', 'markdown', 'typst']);
const validEntrypointRoles = new Set(['document', 'book-page']);
const validFileRoles = new Set(['source', 'asset', 'bibliography', 'generated']);

export function validateDocumentContract() {
  const required = [
    'README.md',
    'package.json',
    'schemas/document-bundle.schema.json',
    'schemas/document-bundle-v2.schema.json',
    'schemas/project-graph.schema.json',
    'src/index.ts',
  ];
  for (const relative of required) {
    if (!fs.existsSync(path.join(root, relative))) {
      throw new Error(`Missing document contract file: ${relative}`);
    }
  }
  const schema = readJSON(path.join(root, 'schemas/project-graph.schema.json'));
  if (schema.$id !== 'https://rinspace.com/schemas/rin-renderer/rin-project-graph-v1.schema.json') {
    throw new Error('Project Graph schema has an unexpected $id');
  }
  for (const field of ['schemaVersion', 'projectHash', 'contentKind', 'entrypoints', 'files', 'references', 'options']) {
    if (!schema.required?.includes(field)) {
      throw new Error(`Project Graph schema is missing required field: ${field}`);
    }
  }

  const fixtureDir = path.join(root, 'fixtures/project-graph');
  const fixtureFiles = fs.readdirSync(fixtureDir).filter((name) => name.endsWith('.json')).sort();
  const expectedFixtures = ['latex-article.json', 'latex-book.json', 'markdown-article.json', 'markdown-book.json'];
  if (JSON.stringify(fixtureFiles) !== JSON.stringify(expectedFixtures)) {
    throw new Error(`Unexpected Project Graph fixtures: ${fixtureFiles.join(', ')}`);
  }
  for (const name of fixtureFiles) {
    validateProjectGraph(readJSON(path.join(fixtureDir, name)), name);
  }

  const bundleSchema = readJSON(path.join(root, 'schemas/document-bundle.schema.json'));
  if (bundleSchema.$id !== 'https://rinspace.com/schemas/rin-renderer/rin-document-bundle-v1.schema.json') {
    throw new Error('Document Bundle schema has an unexpected $id');
  }
  for (const field of ['schemaVersion', 'projectHash', 'state', 'contentKind', 'documentEngine', 'title', 'pages', 'workUnits', 'assets', 'diagnostics', 'provenance']) {
    if (!bundleSchema.required?.includes(field)) {
      throw new Error(`Document Bundle schema is missing required field: ${field}`);
    }
  }
  const bundleV2Schema = readJSON(path.join(root, 'schemas/document-bundle-v2.schema.json'));
  if (bundleV2Schema.$id !== 'https://rinspace.com/schemas/rin-renderer/rin-document-bundle-v2.schema.json') {
    throw new Error('Document Bundle v2 schema has an unexpected $id');
  }
  for (const field of ['schemaVersion', 'projectHash', 'state', 'contentKind', 'documentEngine', 'title', 'pages', 'workUnits', 'assets', 'diagnostics', 'provenance']) {
    if (!bundleV2Schema.required?.includes(field)) {
      throw new Error(`Document Bundle v2 schema is missing required field: ${field}`);
    }
  }
  if (!bundleV2Schema.$defs?.page?.required?.includes('blocks')) {
    throw new Error('Document Bundle v2 pages must require semantic blocks');
  }
  const bundleFixtureDir = path.join(root, 'fixtures/document-bundle');
  const bundleFixtureFiles = fs.readdirSync(bundleFixtureDir).filter((name) => name.endsWith('.json')).sort();
  const expectedBundleFixtures = ['latex-draft.json', 'markdown-draft.json', 'markdown-final-v2.json', 'typst-final.json'];
  if (JSON.stringify(bundleFixtureFiles) !== JSON.stringify(expectedBundleFixtures)) {
    throw new Error(`Unexpected Document Bundle fixtures: ${bundleFixtureFiles.join(', ')}`);
  }
  for (const name of bundleFixtureFiles) {
    validateDocumentBundle(readJSON(path.join(bundleFixtureDir, name)), name);
  }

  const ts = fs.readFileSync(path.join(root, 'src/index.ts'), 'utf8');
  for (const symbol of ['RinProjectGraphV1', 'RinProjectEntrypoint', 'RinProjectGraphFile', 'RinProjectReference', 'RinDocumentBundleV1', 'RinDocumentBundleV2', 'RinPage', 'RinPageV2', 'RinDocumentBlock', 'RinMathUnit', 'RinDiagramUnit', 'RinCodeUnit', 'RinAssetReference', 'RinDiagnostic', 'RinProvenance']) {
    if (!ts.includes(`interface ${symbol}`)) {
      throw new Error(`Document contract TypeScript is missing ${symbol}`);
    }
  }
  console.log(`Rin Renderer document contracts are consistent (${fixtureFiles.length} Project Graph fixtures, ${bundleFixtureFiles.length} Document Bundle fixtures).`);
}

export function validateDocumentBundle(bundle, label) {
  if (!bundle || typeof bundle !== 'object' || Array.isArray(bundle)) throw new Error(`${label}: bundle must be an object`);
  if (!bundleSchemaVersions.has(bundle.schemaVersion)) throw new Error(`${label}: unexpected schemaVersion`);
  assertSHA256(bundle.projectHash, `${label}: projectHash`);
  if (bundle.bundleHash !== undefined) assertSHA256(bundle.bundleHash, `${label}: bundleHash`);
  if (!['draft', 'final'].includes(bundle.state)) throw new Error(`${label}: invalid bundle state`);
  if (!validContentKinds.has(bundle.contentKind)) throw new Error(`${label}: invalid contentKind`);
  assertNonEmpty(bundle.documentEngine, `${label}: documentEngine`);
  if (typeof bundle.title !== 'string') throw new Error(`${label}: title must be a string`);
  if (!Array.isArray(bundle.pages) || bundle.pages.length === 0) throw new Error(`${label}: pages are required`);
  if (!Array.isArray(bundle.workUnits) || !Array.isArray(bundle.assets) || !Array.isArray(bundle.diagnostics)) {
    throw new Error(`${label}: workUnits, assets, and diagnostics must be arrays`);
  }

  const workIDs = new Set();
  for (const unit of bundle.workUnits) {
    validateWorkUnit(unit, label);
    if (workIDs.has(unit.id)) throw new Error(`${label}: duplicate work id ${unit.id}`);
    workIDs.add(unit.id);
  }
  const placeholderCounts = new Map();
  const pageIDs = new Set();
  for (const page of bundle.pages) {
    assertPageIdentifier(page.id, `${label}: page id`);
    if (pageIDs.has(page.id)) throw new Error(`${label}: duplicate page id ${page.id}`);
    pageIDs.add(page.id);
    assertCanonicalPath(page.sourcePath, `${label}: page sourcePath`);
    const expectedFormat = bundle.state === 'draft' ? 'html-with-rin-placeholders' : 'html';
    if (typeof page.fragment !== 'string' || page.fragmentFormat !== expectedFormat) {
      throw new Error(`${label}: invalid page fragment`);
    }
    if (!Array.isArray(page.toc) || !Array.isArray(page.dependencyHashes)) throw new Error(`${label}: invalid page arrays`);
    for (const entry of page.toc) {
      assertIdentifier(entry.id, `${label}: toc id`);
      if (!Number.isSafeInteger(entry.depth) || entry.depth < 1 || entry.depth > 6 || typeof entry.text !== 'string') {
        throw new Error(`${label}: invalid toc entry`);
      }
    }
    for (const dependency of page.dependencyHashes) assertSHA256(dependency, `${label}: dependency hash`);
    if (bundle.schemaVersion === 'rin-document-bundle/v1') {
      if (page.blocks !== undefined) throw new Error(`${label}: v1 page cannot contain blocks`);
    } else {
      validateDocumentBlocks(page, bundle.state, label);
    }
    const canonical = /<rin-work data-id="(rw_[a-f0-9]{32})"><\/rin-work>/g;
    let match;
    while ((match = canonical.exec(page.fragment)) !== null) {
      if (bundle.state === 'final') throw new Error(`${label}: final page contains rin-work element`);
      if (!workIDs.has(match[1])) throw new Error(`${label}: unknown work id ${match[1]}`);
      placeholderCounts.set(match[1], (placeholderCounts.get(match[1]) || 0) + 1);
    }
    if (page.fragment.replace(canonical, '').match(/<\s*\/?\s*rin-work\b/i)) {
      throw new Error(`${label}: malformed or source-created rin-work element`);
    }
  }
  if (bundle.state === 'draft') {
    for (const id of workIDs) {
      if (placeholderCounts.get(id) !== 1) throw new Error(`${label}: work id ${id} must have exactly one placeholder`);
    }
  }

  const assetIDs = new Set();
  for (const asset of bundle.assets) {
    assertIdentifier(asset.id, `${label}: asset id`);
    if (assetIDs.has(asset.id)) throw new Error(`${label}: duplicate asset id ${asset.id}`);
    assetIDs.add(asset.id);
    assertSHA256(asset.sha256, `${label}: asset sha256`);
    if (!Number.isSafeInteger(asset.bytes) || asset.bytes < 0) throw new Error(`${label}: invalid asset bytes`);
    assertNonEmpty(asset.mediaType, `${label}: asset mediaType`);
    if (asset.kind === 'project-file') {
      assertCanonicalPath(asset.projectPath, `${label}: project asset path`);
      if (asset.artifactKey !== undefined) throw new Error(`${label}: project asset has artifactKey`);
    } else if (asset.kind === 'generated') {
      assertCanonicalPath(asset.artifactKey, `${label}: generated artifact key`);
      if (asset.projectPath !== undefined) throw new Error(`${label}: generated asset has projectPath`);
    } else throw new Error(`${label}: invalid asset kind`);
  }
  for (const diagnostic of bundle.diagnostics) {
    assertNonEmpty(diagnostic.code, `${label}: diagnostic code`);
    assertNonEmpty(diagnostic.stage, `${label}: diagnostic stage`);
    if (!['info', 'warning', 'error'].includes(diagnostic.severity) || typeof diagnostic.message !== 'string') {
      throw new Error(`${label}: invalid diagnostic`);
    }
    if (diagnostic.sourceLocation) validateSourceLocation(diagnostic.sourceLocation, label);
  }
  const provenance = bundle.provenance;
  if (!provenance || typeof provenance !== 'object' || provenance.projectGraphSchemaVersion !== schemaVersion) {
    throw new Error(`${label}: invalid provenance`);
  }
  for (const field of ['adapter', 'adapterVersion', 'engineVersion']) assertNonEmpty(provenance[field], `${label}: provenance ${field}`);
}

function validateDocumentBlocks(page, bundleState, label) {
  if (!Array.isArray(page.blocks) || page.blocks.length === 0) throw new Error(`${label}: v2 page blocks are required`);
  const blockIDs = new Set();
  for (const block of page.blocks) {
    if (!block || typeof block !== 'object' || !/^rb_[a-f0-9]{32}$/.test(block.id)) throw new Error(`${label}: invalid block id`);
    if (blockIDs.has(block.id)) throw new Error(`${label}: duplicate block id ${block.id}`);
    blockIDs.add(block.id);
    if (!['heading', 'paragraph', 'list-item', 'theorem', 'math', 'code', 'figure', 'table', 'quote'].includes(block.kind)) {
      throw new Error(`${label}: invalid block kind`);
    }
    if (typeof block.text !== 'string' || block.text !== normalizeBlockText(block.text)) throw new Error(`${label}: block text is not canonical`);
    const textHash = crypto.createHash('sha256').update(block.text, 'utf8').digest('hex');
    if (block.textHash !== textHash) throw new Error(`${label}: block text hash does not match canonical text`);
    if (!Array.isArray(block.headingPath) || block.headingPath.some((id) => typeof id !== 'string' || !/^[\p{L}\p{N}][\p{L}\p{N}\p{M}._:-]{0,127}$/u.test(id))) {
      throw new Error(`${label}: invalid block heading path`);
    }
    if (block.sourceLocation) validateSourceLocation(block.sourceLocation, label);
  }
  if (bundleState !== 'final') return;
  const matches = [...page.fragment.matchAll(/<[a-z][^>]*\bdata-rin-block-id\s*=\s*["'](rb_[a-f0-9]{32})["'][^>]*>/gi)];
  if (matches.length !== page.blocks.length) throw new Error(`${label}: final v2 fragment block count does not match manifest`);
  const fragmentIDs = new Map();
  for (const match of matches) fragmentIDs.set(match[1], (fragmentIDs.get(match[1]) || 0) + 1);
  for (const id of blockIDs) {
    if (fragmentIDs.get(id) !== 1) throw new Error(`${label}: final v2 block id ${id} must occur exactly once`);
  }
	const openingTags = page.fragment.match(/<[a-z][^>]*>/gi) || [];
	if (openingTags.some((tag) => /\bdata-rin-block-id\b/i.test(tag) &&
	  !/\bdata-rin-block-id\s*=\s*["']rb_[a-f0-9]{32}["']/i.test(tag))) {
    throw new Error(`${label}: malformed block id attribute`);
  }
}

function normalizeBlockText(value) {
  return value.trim().replace(/\s+/gu, ' ');
}

function validateWorkUnit(unit, label) {
  if (!unit || typeof unit !== 'object' || !/^rw_[a-f0-9]{32}$/.test(unit.id)) throw new Error(`${label}: invalid work unit id`);
  if (typeof unit.source !== 'string') throw new Error(`${label}: work unit source must be a string`);
  if (unit.sourceLocation) validateSourceLocation(unit.sourceLocation, label);
  if (unit.kind === 'math') {
    if (typeof unit.display !== 'boolean') throw new Error(`${label}: math display is required`);
    assertSHA256(unit.macroContextHash, `${label}: macro context hash`);
  } else if (unit.kind === 'diagram') {
    assertNonEmpty(unit.diagramType, `${label}: diagram type`);
    if (unit.layout?.alignment && !['center', 'flushleft', 'flushright'].includes(unit.layout.alignment)) throw new Error(`${label}: invalid diagram alignment`);
  } else if (unit.kind !== 'code') throw new Error(`${label}: invalid work unit kind`);
}

function validateSourceLocation(location, label) {
  assertCanonicalPath(location.path, `${label}: source location path`);
  validatePosition(location.start, label);
  if (location.end) validatePosition(location.end, label);
}

function validatePosition(position, label) {
  if (!position || !Number.isSafeInteger(position.line) || position.line < 1 || !Number.isSafeInteger(position.column) || position.column < 1) {
    throw new Error(`${label}: invalid source position`);
  }
}

function assertSHA256(value, label) {
  if (typeof value !== 'string' || !/^[a-f0-9]{64}$/.test(value)) throw new Error(`${label} is not a sha256`);
}

function assertNonEmpty(value, label) {
  if (typeof value !== 'string' || !value.trim()) throw new Error(`${label} must be a non-empty string`);
}

function assertIdentifier(value, label) {
  if (typeof value !== 'string' || !/^[A-Za-z][A-Za-z0-9._:-]{0,127}$/.test(value)) throw new Error(`${label} is invalid`);
}

function assertPageIdentifier(value, label) {
  if (typeof value !== 'string' || !/^\p{L}[\p{L}\p{N}\p{M}._:-]{0,127}$/u.test(value)) {
    throw new Error(`${label} is invalid`);
  }
}

function validateProjectGraph(graph, label) {
  if (!graph || typeof graph !== 'object' || Array.isArray(graph)) throw new Error(`${label}: graph must be an object`);
  if (graph.schemaVersion !== schemaVersion) throw new Error(`${label}: unexpected schemaVersion`);
  if (!validContentKinds.has(graph.contentKind)) throw new Error(`${label}: invalid contentKind`);
  if (!Array.isArray(graph.files) || graph.files.length === 0) throw new Error(`${label}: files are required`);
  const paths = new Set();
  for (const file of graph.files) {
    assertCanonicalPath(file.path, `${label}: file path`);
    if (paths.has(file.path)) throw new Error(`${label}: duplicate file path ${file.path}`);
    paths.add(file.path);
    if (!/^[a-f0-9]{64}$/.test(file.sha256)) throw new Error(`${label}: invalid file sha256`);
    if (!Number.isSafeInteger(file.bytes) || file.bytes < 0) throw new Error(`${label}: invalid file bytes`);
    if (!validFileRoles.has(file.role)) throw new Error(`${label}: invalid file role`);
  }
  if (graph.projectHash !== computeProjectHash(graph.contentKind, graph.files)) {
    throw new Error(`${label}: projectHash does not match canonical files`);
  }
  if (!Array.isArray(graph.entrypoints) || graph.entrypoints.length === 0) throw new Error(`${label}: entrypoints are required`);
  for (const entrypoint of graph.entrypoints) {
    assertCanonicalPath(entrypoint.path, `${label}: entrypoint path`);
    if (!paths.has(entrypoint.path)) throw new Error(`${label}: missing entrypoint file ${entrypoint.path}`);
    if (!validEntrypointRoles.has(entrypoint.role)) throw new Error(`${label}: invalid entrypoint role`);
  }
  if (!Array.isArray(graph.references)) throw new Error(`${label}: references must be an array`);
  for (const reference of graph.references) {
    if (!paths.has(reference.from)) throw new Error(`${label}: missing reference source ${reference.from}`);
    if (reference.resolved && !paths.has(reference.to)) throw new Error(`${label}: missing reference target ${reference.to}`);
    if (!reference.resolved && reference.to) assertCanonicalPath(reference.to, `${label}: unresolved target`);
    if (typeof reference.kind !== 'string' || !reference.kind.trim()) throw new Error(`${label}: empty reference kind`);
  }
  if (!graph.options || typeof graph.options !== 'object' || Array.isArray(graph.options)) throw new Error(`${label}: options must be an object`);
}

function computeProjectHash(contentKind, files) {
  const hash = crypto.createHash('sha256');
  writeHashPart(hash, schemaVersion);
  writeHashPart(hash, contentKind);
  for (const file of [...files].sort((left, right) => left.path < right.path ? -1 : left.path > right.path ? 1 : 0)) {
    writeHashPart(hash, file.path);
    writeHashPart(hash, file.sha256);
  }
  return hash.digest('hex');
}

function writeHashPart(hash, value) {
  hash.update(value, 'utf8');
  hash.update(Buffer.from([0]));
}

function assertCanonicalPath(value, label) {
  if (typeof value !== 'string' || !value || value.includes('\\') || value.startsWith('/') || /^[A-Za-z]:/.test(value)) {
    throw new Error(`${label} is not canonical: ${JSON.stringify(value)}`);
  }
  const parts = value.split('/');
  if (parts.some((part) => !part || part === '.' || part === '..')) {
    throw new Error(`${label} is not canonical: ${JSON.stringify(value)}`);
  }
}

function readJSON(target) {
  return JSON.parse(fs.readFileSync(target, 'utf8'));
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  validateDocumentContract();
}
