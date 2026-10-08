#!/usr/bin/env node
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { createZip, htmlSemantics, sourceFeatures } from './run-article-shadow.mjs';

const scriptPath = fileURLToPath(import.meta.url);
const terminal = new Set(['succeeded', 'failed', 'canceled', 'expired']);

export async function readBookShadowManifest(filename) {
  const root = path.dirname(path.resolve(filename));
  const manifest = JSON.parse(fs.readFileSync(filename, 'utf8'));
  if (manifest?.schemaVersion !== 'rin-markdown-book-shadow-manifest/v1' || typeof manifest?.provenance?.sourcePolicy !== 'string' ||
      typeof manifest?.provenance?.containsPrivateSource !== 'boolean' || !Array.isArray(manifest.books) || manifest.books.length < 2 || manifest.books.length > 20) {
    throw new Error('invalid Markdown Book shadow manifest contract');
  }
  const sizes = new Set();
  const books = [];
  for (const book of manifest.books) {
    const hasBookURL = typeof book?.sourceUrl === 'string' && book.sourceUrl !== '';
    const hasPages = Array.isArray(book?.pages);
    if (!book || !/^[a-z0-9][a-z0-9._-]{0,127}$/i.test(book.id || '') || typeof book.title !== 'string' || hasBookURL === hasPages || !['small', 'large'].includes(book.sizeClass)) {
      throw new Error('invalid Markdown Book shadow entry');
    }
    if (hasBookURL && manifest.provenance.containsPrivateSource) throw new Error('private manifests cannot fetch public Book URLs');
    sizes.add(book.sizeClass);
    const ids = new Set(); const paths = new Set(); let changeProbeCount = 0;
    const pageEntries = hasBookURL ? await publicMarkdownBookPages(book) : book.pages;
    if (pageEntries.length < 2 || pageEntries.length > 512) throw new Error(`Book ${book.id} page count is invalid`);
    const pages = [];
    for (const page of pageEntries) {
      const hasSource = typeof page?.source === 'string' && page.source !== '';
      const hasSourceURL = typeof page?.sourceUrl === 'string' && page.sourceUrl !== '';
      const hasSourceText = typeof page?.sourceText === 'string' && page.sourceText !== '';
      if (!page || typeof page.id !== 'string' || typeof page.path !== 'string' || typeof page.title !== 'string' || Number(hasSource) + Number(hasSourceURL) + Number(hasSourceText) !== 1 || ids.has(page.id) || paths.has(page.path) || !page.path.endsWith('.md')) {
        throw new Error(`invalid or duplicate page in Book ${book.id}`);
      }
      if (hasSourceURL && manifest.provenance.containsPrivateSource) throw new Error('private manifests cannot fetch public page URLs');
      ids.add(page.id); paths.add(page.path);
      const source = (hasSource ? fs.readFileSync(safePath(root, page.source), 'utf8')
        : hasSourceURL ? await publicMarkdownArticleSource(page.sourceUrl, page.expectedSourceSha256)
          : page.sourceText).replace(/\r\n?/g, '\n');
      if (!source.trim()) throw new Error(`empty page source in Book ${book.id}`);
      if (page.changeProbe === true) changeProbeCount++;
      pages.push({ id: page.id, path: page.path, title: page.title, source, sha256: sha256(source), changeProbe: page.changeProbe === true });
    }
    if (changeProbeCount !== 1) throw new Error(`Book ${book.id} requires exactly one changeProbe page`);
    const assets = Array.isArray(book.assets) ? book.assets.map((asset) => {
      if (!asset || typeof asset.path !== 'string' || typeof asset.source !== 'string' || paths.has(asset.path)) throw new Error(`invalid asset in Book ${book.id}`);
      paths.add(asset.path);
      const data = fs.readFileSync(safePath(root, asset.source));
      return { path: asset.path, data, sha256: sha256(data) };
    }) : [];
    books.push({ id: book.id, title: book.title.trim(), sizeClass: book.sizeClass, pages, assets,
      maxOutputBytes: positiveInt(book.maxOutputBytes || 16 * 1024 * 1024), maxElapsedMs: positiveInt(book.maxElapsedMs || 900000),
      maxReaderMs: positiveNumber(book.maxReaderMs || 250) });
  }
  if (!sizes.has('small') || !sizes.has('large')) throw new Error('Book shadow manifest requires small and large representatives');
  return { manifest, books };
}

export async function main(argv = process.argv.slice(2)) {
  const args = parseArgs(argv);
  if (!args.manifest) throw new Error('--manifest is required');
  const { manifest, books } = await readBookShadowManifest(path.resolve(args.manifest));
  if (args.validateOnly === 'true') {
    console.log(`Markdown Book shadow manifest valid: ${books.length} representative books`);
    return;
  }
  const endpoint = String(args.endpoint || process.env.RIN_RENDERER_MARKDOWN_SHADOW_ENDPOINT || process.env.RIN_RENDERER_SHADOW_ENDPOINT || '').replace(/\/+$/, '');
  const token = String(args.token || process.env.RIN_RENDERER_MARKDOWN_SHADOW_TOKEN || process.env.RIN_RENDERER_SHADOW_TOKEN || process.env.RIN_RENDERER_SERVICE_TOKEN || '');
  if (!endpoint) throw new Error('Book shadow endpoint is required');
  const results = [];
  for (const book of books) results.push(await shadowBook(endpoint, token, book));
  const report = {
    schemaVersion: 'rin-markdown-book-shadow-report/v1', generatedAt: new Date().toISOString(),
    manifestSha256: sha256(fs.readFileSync(args.manifest)),
    sourcePolicy: manifest.provenance?.sourcePolicy || 'synthetic-public',
    summary: { total: results.length, passed: results.filter((item) => item.status === 'passed').length, failed: results.filter((item) => item.status !== 'passed').length },
    books: results,
  };
  const out = path.resolve(args.out || '.rin-markdown-book-shadow/report.json');
  fs.mkdirSync(path.dirname(out), { recursive: true });
  fs.writeFileSync(out, `${JSON.stringify(report, null, 2)}\n`);
  for (const result of results) {
    console.log(`Book shadow result: ${JSON.stringify(result)}`);
  }
  console.log(`Markdown Book shadow: ${report.summary.passed}/${report.summary.total} passed; report=${out}`);
  if (report.summary.failed) process.exitCode = 1;
}

async function shadowBook(endpoint, token, book) {
  const started = Date.now();
  const archive = bookArchive(book);
  const full = await render(endpoint, token, book, archive, 'base-full', false);
  const changed = { ...book, pages: book.pages.map((page) => page.changeProbe
    ? { ...page, source: `${page.source}\n\nIncremental change probe.`, sha256: sha256(`${page.source}\n\nIncremental change probe.`) }
    : page) };
  const changedArchive = bookArchive(changed);
  const warm = await render(endpoint, token, changed, changedArchive, 'changed-incremental', true);
  const clean = await render(endpoint, token, changed, changedArchive, 'changed-clean-full', false);
  const failures = [];
  validateBundle(full.result, book, failures, 'full');
  validateBundle(warm.result, changed, failures, 'warm changed');
  validateBundle(clean.result, changed, failures, 'clean changed');
  const equivalence = compareIncrementalResult(warm.result, clean.result);
  if (!equivalence.equal) {
    failures.push('changed incremental output differs from equivalent clean full output');
  }
  if (!warm.result.cache?.hit || !warm.result.cache?.reusedStages?.includes('page-finalizer')) failures.push('warm run did not prove page-finalizer reuse');
  if (full.result.cache?.hit || clean.result.cache?.hit) failures.push('forced full render unexpectedly reported incremental reuse');
  const htmlBytes = clean.result.inline.pages.reduce((sum, page) => sum + Buffer.byteLength(page.fragment), 0);
  const resultBytes = Buffer.byteLength(JSON.stringify(clean.result));
  const declaredAssetBytes = (clean.result.inline.assets || []).reduce((sum, asset) => sum + Number(asset.bytes || 0), 0);
  const elapsedMs = Date.now() - started;
  const readerMedianMs = readerProbe(clean.result.inline);
  if (resultBytes + declaredAssetBytes > book.maxOutputBytes) failures.push('Book result plus assets exceeded declared byte budget');
  if (elapsedMs > book.maxElapsedMs) failures.push('Book shadow exceeded declared latency budget');
  if (readerMedianMs > book.maxReaderMs) failures.push('Book reader hydration probe exceeded declared latency budget');
  return {
    id: book.id, sizeClass: book.sizeClass, status: failures.length ? 'failed' : 'passed', pageCount: book.pages.length,
    sourceBytes: book.pages.reduce((sum, page) => sum + Buffer.byteLength(page.source), 0) + book.assets.reduce((sum, asset) => sum + asset.data.length, 0),
    htmlBytes, resultBytes, declaredAssetBytes, elapsedMs, readerMedianMs,
    fullElapsedMs: full.elapsedMs, warmElapsedMs: warm.elapsedMs, cleanChangedElapsedMs: clean.elapsedMs,
    projectHash: clean.result.projectHash, bundleHash: clean.result.inline.bundleHash, cache: warm.result.cache,
    ...(!equivalence.equal ? { equivalence } : {}), failures,
  };
}

function compareIncrementalResult(incremental, clean) {
  const incrementalPages = new Map((incremental?.inline?.pages || []).map((page) => [page.id, page]));
  const pageDiffs = [];
  for (const cleanPage of clean?.inline?.pages || []) {
    const incrementalPage = incrementalPages.get(cleanPage.id);
    if (JSON.stringify(incrementalPage) === JSON.stringify(cleanPage)) continue;
    const incrementalMetadata = incrementalPage ? { ...incrementalPage, fragment: '' } : null;
    const cleanMetadata = { ...cleanPage, fragment: '' };
    pageDiffs.push({
      id: cleanPage.id,
      incrementalPageSha256: sha256(JSON.stringify(incrementalPage)),
      cleanPageSha256: sha256(JSON.stringify(cleanPage)),
      incrementalFragmentSha256: sha256(incrementalPage?.fragment || ''),
      cleanFragmentSha256: sha256(cleanPage.fragment || ''),
      metadataEqual: JSON.stringify(incrementalMetadata) === JSON.stringify(cleanMetadata),
    });
  }
  const equal = incremental?.resultHash === clean?.resultHash &&
    incremental?.inline?.bundleHash === clean?.inline?.bundleHash && pageDiffs.length === 0;
  const bundleFieldDiffs = [];
  const bundleFields = new Set([...Object.keys(incremental?.inline || {}), ...Object.keys(clean?.inline || {})]);
  for (const field of [...bundleFields].sort()) {
    if (field === 'bundleHash' || field === 'pages') continue;
    const incrementalValue = JSON.stringify(incremental?.inline?.[field]);
    const cleanValue = JSON.stringify(clean?.inline?.[field]);
    if (incrementalValue !== cleanValue) bundleFieldDiffs.push({
      field,
      incrementalSha256: sha256(incrementalValue),
      cleanSha256: sha256(cleanValue),
    });
  }
  return {
    equal,
    resultHashEqual: incremental?.resultHash === clean?.resultHash,
    bundleHashEqual: incremental?.inline?.bundleHash === clean?.inline?.bundleHash,
    pageDiffs, bundleFieldDiffs,
  };
}

function bookArchive(book) {
  return createZip([
    ...book.pages.map((page) => ({ path: page.path, data: page.source })),
    ...book.assets.map((asset) => ({ path: asset.path, data: asset.data })),
  ]);
}

async function render(endpoint, token, book, archive, phase, incrementalReuse) {
  const form = new FormData();
  form.append('source', new Blob([archive], { type: 'application/zip' }), `${book.id}.zip`);
  const pageManifest = book.pages.map(({ path, id, title }) => ({ path, id, title }));
  const options = JSON.stringify({ documentMode: 'book', incrementalReuse, publish: true });
  for (const [key, value] of Object.entries({ contentKind: 'markdown', documentEngine: 'unified', documentMode: 'book', bookPages: JSON.stringify(pageManifest), entrypoint: book.pages[0].path, priorityIntent: 'rebuild', title: book.title, projectStatus: 'published-preview', options })) form.append(key, value);
  const headers = authHeaders(token);
  headers['Idempotency-Key'] = `markdown-book-shadow-${book.id}-${phase}-${sha256(archive)}`;
  const started = Date.now();
  let response = await fetch(`${endpoint}/api/render/jobs`, { method: 'POST', headers, body: form });
  let payload = await json(response);
  if (!response.ok || !payload?.job?.jobId) throw new Error(`Book job submission failed: HTTP ${response.status}`);
  let job = payload.job;
  const deadline = Date.now() + book.maxElapsedMs;
  while (!terminal.has(job.state)) {
    if (Date.now() > deadline) throw new Error('Book job timed out');
    await new Promise((resolve) => setTimeout(resolve, 500));
    response = await fetch(`${endpoint}/api/render/jobs/${encodeURIComponent(job.jobId)}`, { headers });
    job = await json(response);
  }
  if (job.state !== 'succeeded') throw new Error(`Book job ended in ${job.state}`);
  response = await fetch(`${endpoint}/api/render/jobs/${encodeURIComponent(job.jobId)}/result`, { headers });
  const result = await json(response);
  if (!response.ok) throw new Error(`Book result failed: HTTP ${response.status}`);
  return { result, elapsedMs: Date.now() - started };
}

function validateBundle(result, book, failures, label) {
  const bundle = result?.inline;
  if (result?.schemaVersion !== 'rin-render-result/v1' || !['rin-document-bundle/v1', 'rin-document-bundle/v2'].includes(bundle?.schemaVersion) || bundle.state !== 'final' || !Array.isArray(bundle.pages) || bundle.pages.length !== book.pages.length) {
    failures.push(`${label} final Bundle identity is invalid`); return;
  }
  bundle.pages.forEach((page, index) => {
    const source = book.pages[index];
    if (page.id !== source.id || page.sourcePath !== source.path || !page.dependencyHashes?.includes(source.sha256)) failures.push(`${label} page identity/order mismatch at ${index}`);
    const semantics = htmlSemantics(page.fragment || '');
    if (Object.values(semantics.safety).some(Boolean)) failures.push(`${label} page ${source.id} violated safety invariants`);
    const features = sourceFeatures(source.source);
    if (features.math && !semantics.counts.math) failures.push(`${label} page ${source.id} lost math`);
    if (features.code && !semantics.counts.code) failures.push(`${label} page ${source.id} lost code`);
    if (requiresShikiHighlighting(source.source) && !semantics.counts.shiki) failures.push(`${label} page ${source.id} lost Shiki highlighting`);
    if (features.notExists && !page.fragment.includes('\\not\\exists')) failures.push(`${label} page ${source.id} lost exact \\not\\exists metadata`);
    if (/^\s*:{3}diagram\b/m.test(source.source) && !semantics.counts.diagrams) failures.push(`${label} page ${source.id} lost diagram output`);
    if (/!\[[^\]]*\]\([^)]+\)/.test(source.source) && !semantics.counts.images) failures.push(`${label} page ${source.id} lost image asset output`);
    for (const target of markdownPageLinks(source.source)) {
      if (!navigationTargetVariants(target).some((value) => page.fragment.includes(`href="${value}"`) || page.fragment.includes(`href='${value}'`))) {
        failures.push(`${label} page ${source.id} lost navigation link ${target}`);
      }
    }
  });
  for (const asset of book.assets) {
    if (!bundle.assets?.some((item) => item.projectPath === asset.path && item.sha256 === asset.sha256)) failures.push(`${label} Bundle lost asset ${asset.path}`);
  }
}

function markdownPageLinks(source) {
  return Array.from(source.matchAll(/(?<!!)\[[^\]]+\]\(([^)]+\.md(?:#[^)]+)?)\)/g), (match) => match[1]);
}

export function requiresShikiHighlighting(source) {
  const plain = new Set(['text', 'txt', 'plain', 'plaintext']);
  return Array.from(String(source).matchAll(/^\s*```([^\s`]*)/gim), (match) => match[1].toLowerCase())
    .some((language) => language && !plain.has(language));
}

export function navigationTargetVariants(target) {
  const value = String(target);
  return value.startsWith('./') ? [value, value.slice(2)] : [value];
}

function readerProbe(bundle) {
  const serialized = JSON.stringify({ title: bundle.title, pages: bundle.pages.map((page) => ({ id: page.id, title: page.title, fragment: page.fragment, toc: page.toc })) });
  const samples = [];
  for (let sample = 0; sample < 15; sample++) {
    const started = performance.now();
    const reader = JSON.parse(serialized);
    const byID = new Map(reader.pages.map((page, index) => [page.id, index]));
    for (const page of reader.pages) {
      if (!byID.has(page.id) || typeof page.fragment !== 'string') throw new Error('reader probe lost page identity');
      htmlSemantics(page.fragment);
    }
    samples.push(performance.now() - started);
  }
  samples.sort((left, right) => left - right);
  return Number(samples[Math.floor(samples.length / 2)].toFixed(3));
}

async function publicMarkdownBookPages(book) {
  const payload = await fetchPublicContent(book.sourceUrl);
  if (payload?.type !== 'book' || payload?.book?.kind !== 'markdown' || payload?.sourceVisibility !== 'open') {
    throw new Error(`Book ${book.id} public source is not an open Markdown Book`);
  }
  const body = String(payload.body || '');
  if (!sha256Matches(body, book.expectedBodySha256)) throw new Error(`Book ${book.id} public body identity changed`);
  const project = markedJSON(body, 'RIN_MARKDOWN_BOOK');
  if (!project || !Array.isArray(project.files)) throw new Error(`Book ${book.id} public project is invalid`);
  return project.files.map((page) => ({
    id: String(page.id || ''), path: String(page.path || ''), title: String(page.title || ''),
    sourceText: String(page.body || ''),
    changeProbe: String(page.id || '') === String(book.changeProbePageId || ''),
  }));
}

async function publicMarkdownArticleSource(rawURL, expectedSourceSha256) {
  const payload = await fetchPublicContent(rawURL);
  if (payload?.type !== 'blog' || payload?.editor !== 'markdown' || payload?.sourceVisibility !== 'open') {
    throw new Error('public page source is not an open Markdown article');
  }
  const source = markedText(String(payload.body || ''), 'RIN_MARKDOWN_SOURCE');
  if (!source || !sha256Matches(source, expectedSourceSha256)) throw new Error('public Markdown article source identity changed');
  return source;
}

async function fetchPublicContent(rawURL) {
  const target = new URL(rawURL);
  if (target.protocol !== 'https:' || target.hostname !== 'rinspace.com' || target.username || target.password || target.search || target.hash || !/^\/api\/content\/[0-9]+$/.test(target.pathname)) {
    throw new Error('public Book shadow URL is not allowlisted');
  }
  const response = await fetch(target, { redirect: 'error', headers: { Accept: 'application/json' } });
  const text = await response.text();
  if (!response.ok || Buffer.byteLength(text) > 16 * 1024 * 1024) throw new Error(`public Book shadow fetch failed: HTTP ${response.status}`);
  return JSON.parse(text);
}

function markedText(body, marker) {
  const normalized = body.replace(/\r\n?/g, '\n');
  const match = normalized.match(new RegExp(`\\[\\[${marker}\\]\\]\\n([\\s\\S]*?)\\n\\[\\[\\/${marker}\\]\\]`));
  return match ? match[1] : '';
}

function markedJSON(body, marker) {
  try { return JSON.parse(markedText(body, marker)); } catch { return null; }
}

function sha256Matches(value, expected) {
  return /^[0-9a-f]{64}$/.test(String(expected || '')) && sha256(value) === expected;
}

function authHeaders(token) { return { ...(token ? { Authorization: `Bearer ${token}`, 'X-Rin-Renderer-Token': token } : {}), 'X-Rin-Renderer-Owner-Scope': 'markdown-book-shadow' }; }
async function json(response) { const text = await response.text(); return JSON.parse(text); }
function sha256(value) { return createHash('sha256').update(value).digest('hex'); }
function positiveInt(value) { const result = Number(value); if (!Number.isSafeInteger(result) || result <= 0) throw new Error('expected positive integer'); return result; }
function positiveNumber(value) { const result = Number(value); if (!Number.isFinite(result) || result <= 0) throw new Error('expected positive number'); return result; }
function safePath(root, relative) { const target = path.resolve(root, relative); if (path.isAbsolute(relative) || (target !== root && !target.startsWith(`${root}${path.sep}`))) throw new Error('manifest path escapes root'); return target; }
function parseArgs(args) { const result = {}; for (let index = 0; index < args.length; index++) { const key = args[index].replace(/^--/, '').replace(/-([a-z])/g, (_, value) => value.toUpperCase()); result[key] = !args[index + 1] || args[index + 1].startsWith('--') ? 'true' : args[++index]; } return result; }

if (process.argv[1] && path.resolve(process.argv[1]) === scriptPath) main().catch((error) => { console.error(error.stack || error); process.exit(1); });
