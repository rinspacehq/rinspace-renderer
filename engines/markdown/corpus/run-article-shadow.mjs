#!/usr/bin/env node
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import rehypeParse from 'rehype-parse';
import remarkMath from 'remark-math';
import remarkParse from 'remark-parse';
import { unified } from 'unified';

const scriptPath = fileURLToPath(import.meta.url);
const maxArticles = 50;
const maxSourceBytes = 8 * 1024 * 1024;
const maxLegacyHTMLBytes = 16 * 1024 * 1024;
const terminalStates = new Set(['succeeded', 'failed', 'canceled', 'expired']);

export async function main(argv = process.argv.slice(2)) {
  const options = parseArgs(argv);
  if (!options.manifest) throw new Error('--manifest is required');
  const manifestPath = path.resolve(options.manifest);
  const manifest = readManifest(manifestPath);
  const legacyBaseline = normalizedLegacyBaseline(manifest.provenance.legacyBaseline);
  if (manifest.articles.some((entry) => entry.legacyRenderMs !== undefined || entry.legacyHtmlBytes !== undefined) && !legacyBaseline) {
    throw new Error('numeric legacy baselines require documented provenance');
  }
  const loaded = await loadArticles(manifestPath, manifest);
  if (options.validateOnly === 'true') {
    console.log(`Markdown article shadow manifest valid: ${loaded.length} bounded articles`);
    return;
  }

  const endpoint = trimSlash(options.endpoint || process.env.RIN_RENDERER_MARKDOWN_SHADOW_ENDPOINT || process.env.RIN_RENDERER_SHADOW_ENDPOINT || '');
  if (!endpoint) throw new Error('--endpoint or RIN_RENDERER_MARKDOWN_SHADOW_ENDPOINT is required');
  const token = options.token || process.env.RIN_RENDERER_MARKDOWN_SHADOW_TOKEN || process.env.RIN_RENDERER_SHADOW_TOKEN || process.env.RIN_RENDERER_SERVICE_TOKEN || '';
  const timeoutMs = positiveInt(options.timeoutMs || process.env.RIN_RENDERER_MARKDOWN_SHADOW_TIMEOUT_MS || '900000');
  const pollMs = positiveInt(options.pollMs || '1000');
  const outPath = path.resolve(options.out || '.rin-markdown-shadow/report.json');
  const snapshotDir = options.snapshotDir ? path.resolve(options.snapshotDir) : '';
  const allowFailures = booleanValue(options.allowFailures || process.env.RIN_RENDERER_MARKDOWN_SHADOW_ALLOW_FAILURES || '');
  const results = [];

  console.log(`Markdown article shadow: ${loaded.length} articles; endpoint=${redactEndpoint(endpoint)}`);
  for (const article of loaded) {
    console.log(`Shadow article id=${article.id} sourceBytes=${article.sourceBytes} sourceSha256=${article.sourceSha256}`);
    const result = await renderAndCompareArticle({ article, endpoint, token, timeoutMs, pollMs, snapshotDir });
    results.push(result);
    console.log(`  status=${result.status} elapsedMs=${result.elapsedMs} htmlBytes=${result.final?.htmlBytes || 0} diagnostics=${result.diagnostics.length}`);
    if (result.status === 'passed') console.log(`  evidence=${JSON.stringify(shadowEvidence(result))}`);
    if (result.failures.length) console.log(`  failures=${result.failures.join(';')}`);
  }

  const failed = results.filter((item) => item.status !== 'passed').length;
  const report = {
    schemaVersion: 'rin-markdown-article-shadow-report/v1',
    generatedAt: new Date().toISOString(),
    manifest: {
      schemaVersion: manifest.schemaVersion,
      sourcePolicy: manifest.provenance.sourcePolicy,
      containsPrivateSource: manifest.provenance.containsPrivateSource,
      legacyBaseline,
      articleCount: loaded.length,
      manifestSha256: sha256(fs.readFileSync(manifestPath)),
    },
    rendererEndpointConfigured: true,
    summary: { passed: results.length - failed, failed, total: results.length },
    articles: results,
  };
  fs.mkdirSync(path.dirname(outPath), { recursive: true });
  fs.writeFileSync(outPath, `${JSON.stringify(report, null, 2)}\n`);
  console.log(`Markdown shadow summary: ${report.summary.passed} passed, ${report.summary.failed} failed; report=${outPath}`);
  if (failed && !allowFailures) process.exitCode = 1;
}

function shadowEvidence(result) {
  const diagnosticCodes = {};
  for (const item of result.diagnostics) {
    const code = String(item?.code || 'unknown');
    diagnosticCodes[code] = (diagnosticCodes[code] || 0) + 1;
  }
  return {
    sourceFeatures: result.sourceFeatures,
    expectedFeatures: result.expectedFeatures,
    counts: result.final?.counts || {},
    safety: result.final?.safety || {},
    diagnosticCodes,
    versions: result.versions || {},
    legacyRenderMs: result.legacyRenderMs,
    latencyDeltaMs: result.latencyDeltaMs,
    legacyHtmlBytes: result.legacyHtmlBytes,
    outputSizeDeltaBytes: result.outputSizeDeltaBytes,
  };
}

function readManifest(manifestPath) {
  const value = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
  if (!isRecord(value) || value.schemaVersion !== 'rin-markdown-article-shadow-manifest/v1' ||
      !isRecord(value.provenance) || typeof value.provenance.sourcePolicy !== 'string' ||
      typeof value.provenance.containsPrivateSource !== 'boolean' || !Array.isArray(value.articles) ||
      value.articles.length < 1 || value.articles.length > maxArticles) {
    throw new Error('invalid Markdown article shadow manifest contract');
  }
  return value;
}

function normalizedLegacyBaseline(value) {
  if (value === undefined) return null;
  if (!isRecord(value) || value.schemaVersion !== 'rin-markdown-legacy-baseline/v1' ||
      value.implementation !== 'ui/src/utils/blogBody.ts' || !/^[0-9a-f]{40}$/.test(value.implementationGitBlob || '') ||
      !/^node-v[0-9]+(?:\.[0-9]+){2}$/.test(value.runtime || '') || !/^[0-9]+(?:\.[0-9]+){2}$/.test(value.katex || '') ||
      !Number.isSafeInteger(value.warmupIterations) || value.warmupIterations < 1 ||
      !Number.isSafeInteger(value.measuredIterations) || value.measuredIterations < 3 ||
      value.latencyStatistic !== 'median-wall-clock-ms' || typeof value.collectedAt !== 'string' ||
      !Number.isFinite(Date.parse(value.collectedAt))) {
    throw new Error('invalid Markdown legacy latency baseline provenance');
  }
  return {
    schemaVersion: value.schemaVersion, implementation: value.implementation,
    implementationGitBlob: value.implementationGitBlob, runtime: value.runtime, katex: value.katex,
    warmupIterations: value.warmupIterations, measuredIterations: value.measuredIterations,
    latencyStatistic: value.latencyStatistic, collectedAt: value.collectedAt,
  };
}

async function loadArticles(manifestPath, manifest) {
  const root = path.dirname(manifestPath);
  const seen = new Set();
  const loaded = [];
  for (const entry of manifest.articles) {
    if (!isRecord(entry) || !/^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$/.test(entry.id || '') || seen.has(entry.id) ||
        typeof entry.title !== 'string' || !entry.title.trim()) {
      throw new Error('invalid or duplicate Markdown shadow article entry');
    }
    seen.add(entry.id);
    const hasSourcePath = typeof entry.source === 'string' && entry.source !== '';
    const hasSourceURL = typeof entry.sourceUrl === 'string' && entry.sourceUrl !== '';
    if (hasSourcePath === hasSourceURL || (hasSourceURL && manifest.provenance.containsPrivateSource)) {
      throw new Error(`article ${entry.id} must have exactly one allowed source location`);
    }
    const rawSource = hasSourcePath
      ? fs.readFileSync(safeManifestPath(root, entry.source), 'utf8')
      : await fetchPublicMarkdownSource(entry.sourceUrl);
    const source = rawSource.replace(/\r\n?/g, '\n').trim();
    const sourceBytes = Buffer.byteLength(source);
    if (!source || sourceBytes > maxSourceBytes) throw new Error(`article ${entry.id} source is empty or oversized`);
    if (hasSourceURL && (!/^[0-9a-f]{64}$/.test(String(entry.expectedSourceSha256 || '')) ||
        sha256(source) !== entry.expectedSourceSha256)) {
      throw new Error(`article ${entry.id} public source identity changed`);
    }
    let legacyHTML = '';
    if (entry.legacyHtml !== undefined) {
      if (typeof entry.legacyHtml !== 'string' || !entry.legacyHtml) throw new Error(`article ${entry.id} legacyHtml is invalid`);
      legacyHTML = fs.readFileSync(safeManifestPath(root, entry.legacyHtml), 'utf8');
      if (Buffer.byteLength(legacyHTML) > maxLegacyHTMLBytes) throw new Error(`article ${entry.id} legacy HTML is oversized`);
    }
    const legacyRenderMs = entry.legacyRenderMs === undefined ? null : Number(entry.legacyRenderMs);
    if (legacyRenderMs !== null && (!Number.isFinite(legacyRenderMs) || legacyRenderMs < 0)) {
      throw new Error(`article ${entry.id} legacyRenderMs is invalid`);
    }
    const legacyHtmlBytes = entry.legacyHtmlBytes === undefined ? null : Number(entry.legacyHtmlBytes);
    if (legacyHtmlBytes !== null && (!Number.isSafeInteger(legacyHtmlBytes) || legacyHtmlBytes < 0)) {
      throw new Error(`article ${entry.id} legacyHtmlBytes is invalid`);
    }
    loaded.push({
      id: entry.id,
      title: entry.title.trim().slice(0, 300),
      source,
      sourceBytes,
      sourceSha256: sha256(source),
      sourceFeatures: sourceFeatures(source),
      legacyHTML,
      legacyRenderMs,
      legacyHtmlBytes: legacyHTML ? Buffer.byteLength(legacyHTML) : legacyHtmlBytes,
      expectedFeatures: Array.isArray(entry.expectedFeatures) ? entry.expectedFeatures.map(String) : [],
    });
  }
  return loaded;
}

async function fetchPublicMarkdownSource(rawURL) {
  const target = new URL(rawURL);
  if (!isAllowedPublicArticleURL(target)) {
    throw new Error('public shadow source URL is not allowlisted');
  }
  const response = await fetch(target, {
    headers: { Accept: 'application/json' }, redirect: 'error', signal: AbortSignal.timeout(30000),
  });
  if (!response.ok) throw new Error(`public shadow source returned HTTP ${response.status}`);
  const text = await boundedResponseText(response, 32 * 1024 * 1024);
  const payload = JSON.parse(text);
  if (!isRecord(payload) || payload.type !== 'blog' || payload.editor !== 'markdown' ||
      payload.sourceVisibility !== 'open' || typeof payload.body !== 'string') {
    throw new Error('public shadow source is not an open Markdown article');
  }
  const source = markedSection(payload.body, 'RIN_MARKDOWN_SOURCE');
  if (!source) throw new Error('public article has no Markdown source marker');
  return source;
}

function isAllowedPublicArticleURL(target) {
  const allowedHosts = new Set(['rinspace.com', 'www.rinspace.com', 'blog.taskfirst.cn']);
  return target.protocol === 'https:' && allowedHosts.has(target.hostname.toLowerCase()) &&
    !target.username && !target.password && !target.search && !target.hash && /^\/api\/content\/[1-9][0-9]*$/.test(target.pathname);
}

async function boundedResponseText(response, limit) {
  const declared = Number(response.headers.get('content-length'));
  if (Number.isFinite(declared) && declared > limit) throw new Error('public shadow source response is oversized');
  if (!response.body) return '';
  const reader = response.body.getReader();
  const chunks = []; let size = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > limit) throw new Error('public shadow source response is oversized');
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  return Buffer.concat(chunks.map((chunk) => Buffer.from(chunk)), size).toString('utf8');
}

function markedSection(body, marker) {
  const startMarker = `[[${marker}]]`; const endMarker = `[[/${marker}]]`;
  const start = body.indexOf(startMarker); if (start < 0) return '';
  const end = body.indexOf(endMarker, start + startMarker.length); if (end < 0) return '';
  return body.slice(start + startMarker.length, end).trim();
}

async function renderAndCompareArticle({ article, endpoint, token, timeoutMs, pollMs, snapshotDir }) {
  const started = Date.now();
  try {
    const archive = createZip([{ path: 'article.md', data: Buffer.from(article.source) }]);
    const submission = await submitJob({ endpoint, token, archive, article });
    const job = await waitForJob({ endpoint, token, job: submission.job, timeoutMs, pollMs });
    if (job.state !== 'succeeded') {
      const support = await requestJSON(
        resolveURL(endpoint, `/api/render/jobs/${encodeURIComponent(job.jobId)}/support`), token,
      ).catch(() => null);
      const codes = Array.isArray(support?.attempts)
        ? support.attempts.map((attempt) => String(attempt?.errorCode || '')).filter(Boolean)
        : [];
      throw new Error(`render job ended in ${job.state}${codes.length ? ` (${[...new Set(codes)].join(',')})` : ''}`);
    }
    const result = await requestJSON(resolveURL(endpoint, `/api/render/jobs/${encodeURIComponent(job.jobId)}/result`), token);
    const page = validateResult(result, job.jobId, article.sourceSha256);
    const final = htmlSemantics(page.fragment);
    const legacy = article.legacyHTML ? htmlSemantics(article.legacyHTML) : null;
    const elapsedMs = Date.now() - started;
    const failures = evaluateArticle(article, final, result);
    if (snapshotDir) {
      const directory = path.join(snapshotDir, safeName(article.id));
      fs.mkdirSync(directory, { recursive: true });
      fs.writeFileSync(path.join(directory, 'final.html'), page.fragment);
      fs.writeFileSync(path.join(directory, 'result-summary.json'), `${JSON.stringify({
        jobId: result.jobId, projectHash: result.projectHash, resultHash: result.resultHash,
        versions: result.versions, diagnostics: normalizedDiagnostics(result.diagnostics),
      }, null, 2)}\n`);
    }
    return {
      id: article.id,
      status: failures.length ? 'failed' : 'passed',
      sourceSha256: article.sourceSha256,
      sourceBytes: article.sourceBytes,
      sourceFeatures: article.sourceFeatures,
      expectedFeatures: article.expectedFeatures,
      elapsedMs,
      legacyRenderMs: article.legacyRenderMs,
      latencyDeltaMs: article.legacyRenderMs === null ? null : elapsedMs - article.legacyRenderMs,
      legacyHtmlBytes: article.legacyHtmlBytes,
      outputSizeDeltaBytes: article.legacyHtmlBytes === null ? null : final.htmlBytes - article.legacyHtmlBytes,
      projectHash: result.projectHash,
      resultHash: result.resultHash,
      bundleHash: result.inline.bundleHash,
      versions: result.versions,
      diagnostics: normalizedDiagnostics(result.diagnostics),
      final,
      legacy,
      deltas: legacy ? semanticDeltas(legacy, final) : null,
      failures,
    };
  } catch (error) {
    return {
      id: article.id, status: 'failed', sourceSha256: article.sourceSha256,
      sourceBytes: article.sourceBytes, sourceFeatures: article.sourceFeatures,
      expectedFeatures: article.expectedFeatures, elapsedMs: Date.now() - started,
      diagnostics: [], final: null, legacy: null, deltas: null,
      failures: [error instanceof Error ? error.message : String(error)],
    };
  }
}

async function submitJob({ endpoint, token, archive, article }) {
  const form = new FormData();
  form.append('source', new Blob([archive], { type: 'application/zip' }), `${safeName(article.id)}.zip`);
  for (const [key, value] of Object.entries({
    contentKind: 'markdown', documentEngine: 'unified', entrypoint: 'article.md',
    priorityIntent: 'rebuild', title: article.title, projectStatus: 'published-preview',
    options: '{"documentMode":"article","publish":true}',
  })) form.append(key, value);
  const headers = authHeaders(token);
  headers['X-Rin-Renderer-Owner-Scope'] = 'markdown-article-shadow';
  headers['Idempotency-Key'] = `markdown-shadow-${article.sourceSha256}`;
  const response = await fetch(resolveURL(endpoint, '/api/render/jobs'), { method: 'POST', headers, body: form });
  const payload = await responseJSON(response);
  if (!response.ok || !isRecord(payload.job) || typeof payload.job.jobId !== 'string') {
    throw new Error(`job submission failed: HTTP ${response.status} ${errorMessage(payload)}`.trim());
  }
  return payload;
}

async function waitForJob({ endpoint, token, job, timeoutMs, pollMs }) {
  const deadline = Date.now() + timeoutMs;
  let current = job;
  while (!terminalStates.has(current.state)) {
    if (Date.now() >= deadline) throw new Error(`render job timeout after ${timeoutMs}ms`);
    await delay(pollMs);
    current = await requestJSON(resolveURL(endpoint, `/api/render/jobs/${encodeURIComponent(current.jobId)}`), token);
  }
  return current;
}

function validateResult(result, jobID, sourceSha256) {
  if (!isRecord(result) || result.schemaVersion !== 'rin-render-result/v1' || result.jobId !== jobID ||
      result.requestId !== jobID || result.contentKind !== 'markdown' || result.engine !== 'rin-markdown' ||
	  !isRecord(result.inline) || !['rin-document-bundle/v1', 'rin-document-bundle/v2'].includes(result.inline.schemaVersion) ||
      result.inline.state !== 'final' || result.inline.contentKind !== 'markdown' ||
      result.inline.documentEngine !== 'rin-markdown' || !Array.isArray(result.inline.pages) ||
      result.inline.pages.length !== 1 || !Array.isArray(result.inline.workUnits) || result.inline.workUnits.length !== 0) {
    throw new Error('durable Markdown result identity or final Bundle is invalid');
  }
  const page = result.inline.pages[0];
  if (!isRecord(page) || page.sourcePath !== 'article.md' || page.fragmentFormat !== 'html' ||
      typeof page.fragment !== 'string' || !page.fragment || !Array.isArray(page.dependencyHashes) ||
      !page.dependencyHashes.includes(sourceSha256)) {
    throw new Error('durable Markdown page does not match shadow source');
  }
  return page;
}

export function htmlSemantics(html) {
  const counts = { headings: 0, paragraphs: 0, lists: 0, tables: 0, blockquotes: 0, footnotes: 0,
    math: 0, mathDisplay: 0, code: 0, shiki: 0, links: 0, images: 0, directives: 0, diagrams: 0 };
  const safety = { blockedElements: false, eventHandlers: false, unsafeURLs: false, unresolvedWork: false };
  const tree = unified().use(rehypeParse, { fragment: true }).parse(html);
  walk(tree, (node) => {
    if (node.type !== 'element') return;
    const classes = classList(node);
    if (/^h[1-6]$/.test(node.tagName)) counts.headings++;
    if (node.tagName === 'p') counts.paragraphs++;
    if (node.tagName === 'ul' || node.tagName === 'ol') counts.lists++;
    if (node.tagName === 'table') counts.tables++;
    if (node.tagName === 'blockquote') counts.blockquotes++;
    if (node.tagName === 'a') counts.links++;
    if (node.tagName === 'img') counts.images++;
    if (node.tagName === 'pre') counts.code++;
    if (classes.includes('shiki')) counts.shiki++;
    if (classes.includes('footnotes')) counts.footnotes++;
    if (classes.includes('rin-math')) counts.math++;
    if (classes.includes('rin-math-display')) counts.mathDisplay++;
    if (classes.includes('rin-admonition')) counts.directives++;
    if (classes.includes('rin-reader-diagram')) counts.diagrams++;
    if (['script', 'iframe', 'object', 'embed', 'form', 'template', 'svg', 'math'].includes(node.tagName)) safety.blockedElements = true;
    for (const [name, value] of Object.entries(node.properties || {})) {
      if (/^on/i.test(name)) safety.eventHandlers = true;
      if (['href', 'src', 'cite'].includes(name) && /^(?:javascript|vbscript|data|file):/i.test(String(value || '').trim())) safety.unsafeURLs = true;
    }
    if (node.tagName === 'rin-work') safety.unresolvedWork = true;
  });
  return { htmlBytes: Buffer.byteLength(html), htmlSha256: sha256(html), counts, safety };
}

export function sourceFeatures(source) {
  const prose = sourceWithoutFencedCode(source);
  return {
    math: markdownMathCount(source),
    notExists: countMatches(prose, /\\not\\exists/g),
    code: countMatches(source, /^\s{0,3}(?:```|~~~)/gm),
    links: countMatches(prose, /(?<!!)\[[^\]]+\]\([^)]+\)/g),
    images: countMatches(prose, /!\[[^\]]*\]\([^)]+\)/g),
    directives: countMatches(prose, /^\s*:{2,3}[a-zA-Z]/gm),
    footnotes: countMatches(prose, /\[\^[^\]]+\]/g),
    tables: countMatches(prose, /^\s*\|.*\|\s*$/gm),
  };
}

function markdownMathCount(source) {
  const tree = unified().use(remarkParse).use(remarkMath).parse(source);
  let count = 0;
  walk(tree, (node) => {
    if (node.type === 'math' || node.type === 'inlineMath') count++;
  });
  return count;
}

function sourceWithoutFencedCode(source) {
  let fence = '';
  return source.split('\n').map((line) => {
    const match = line.match(/^\s{0,3}(```+|~~~+)/);
    if (!fence && match) { fence = match[1][0]; return ''; }
    if (fence && match?.[1]?.[0] === fence) { fence = ''; return ''; }
    return fence ? '' : line;
  }).join('\n');
}

function evaluateArticle(article, final, result) {
  const failures = [];
  if (Object.values(final.safety).some(Boolean)) failures.push('final HTML violated safety invariants');
  const expected = new Set(article.expectedFeatures);
  for (const feature of ['math', 'code', 'links', 'images', 'directives', 'footnotes', 'tables']) {
    if (article.sourceFeatures[feature] > 0) expected.add(feature);
  }
  const outputCount = { ...final.counts, code: final.counts.code, directives: final.counts.directives };
  for (const feature of expected) {
    if (!Object.hasOwn(outputCount, feature)) failures.push(`unknown expected feature ${feature}`);
    else if (outputCount[feature] < 1) failures.push(`expected feature missing from final HTML: ${feature}`);
  }
  if (article.sourceFeatures.notExists > 0 && !result.inline.pages[0].fragment.includes('\\not\\exists')) {
    failures.push('exact \\not\\exists source metadata was not preserved');
  }
  return failures;
}

function semanticDeltas(before, after) {
  const counts = {};
  for (const key of Object.keys(after.counts)) counts[key] = after.counts[key] - before.counts[key];
  return { htmlBytes: after.htmlBytes - before.htmlBytes, counts };
}

function normalizedDiagnostics(value) {
  if (!Array.isArray(value)) return [];
  return value.map((item) => ({
    severity: String(item?.severity || 'error'), stage: String(item?.stage || ''), code: String(item?.code || ''),
  })).sort((a, b) => `${a.severity}:${a.stage}:${a.code}`.localeCompare(`${b.severity}:${b.stage}:${b.code}`));
}

async function requestJSON(url, token) {
  const response = await fetch(url, { headers: authHeaders(token) });
  const payload = await responseJSON(response);
  if (!response.ok) throw new Error(`HTTP ${response.status} ${errorMessage(payload)}`.trim());
  return payload;
}

async function responseJSON(response) {
  const text = await response.text();
  if (Buffer.byteLength(text) > 32 * 1024 * 1024) throw new Error('Renderer response exceeded shadow limit');
  try { return JSON.parse(text); } catch { return { message: text.slice(0, 300) }; }
}

function authHeaders(token) {
  return {
    ...(token ? { Authorization: `Bearer ${token}`, 'X-Rin-Renderer-Token': token } : {}),
    'X-Rin-Renderer-Owner-Scope': 'markdown-article-shadow',
  };
}

export function createZip(files) {
  const localParts = []; const centralParts = []; let offset = 0;
  for (const file of files) {
    const name = Buffer.from(file.path); const data = Buffer.from(file.data); const crc = crc32(data);
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0); local.writeUInt16LE(20, 4); local.writeUInt32LE(crc, 14);
    local.writeUInt32LE(data.length, 18); local.writeUInt32LE(data.length, 22); local.writeUInt16LE(name.length, 26);
    localParts.push(local, name, data);
    const central = Buffer.alloc(46);
    central.writeUInt32LE(0x02014b50, 0); central.writeUInt16LE(20, 4); central.writeUInt16LE(20, 6);
    central.writeUInt32LE(crc, 16); central.writeUInt32LE(data.length, 20); central.writeUInt32LE(data.length, 24);
    central.writeUInt16LE(name.length, 28); central.writeUInt32LE(offset, 42);
    centralParts.push(central, name); offset += local.length + name.length + data.length;
  }
  const centralSize = centralParts.reduce((sum, item) => sum + item.length, 0);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0); end.writeUInt16LE(files.length, 8); end.writeUInt16LE(files.length, 10);
  end.writeUInt32LE(centralSize, 12); end.writeUInt32LE(offset, 16);
  return Buffer.concat([...localParts, ...centralParts, end]);
}

const crcTable = Array.from({ length: 256 }, (_, index) => {
  let value = index;
  for (let bit = 0; bit < 8; bit++) value = value & 1 ? 0xedb88320 ^ (value >>> 1) : value >>> 1;
  return value >>> 0;
});
function crc32(buffer) {
  let crc = 0xffffffff;
  for (const byte of buffer) crc = (crc >>> 8) ^ crcTable[(crc ^ byte) & 0xff];
  return (crc ^ 0xffffffff) >>> 0;
}

function safeManifestPath(root, relative) {
  if (path.isAbsolute(relative)) throw new Error('manifest paths must be relative');
  const target = path.resolve(root, relative);
  if (target !== root && !target.startsWith(`${root}${path.sep}`)) throw new Error('manifest path escapes its directory');
  if (!fs.existsSync(target) || !fs.statSync(target).isFile()) throw new Error(`manifest file not found: ${relative}`);
  return target;
}

function walk(node, visit) { visit(node); for (const child of node.children || []) walk(child, visit); }
function classList(node) { const value = node.properties?.className; return Array.isArray(value) ? value.map(String) : typeof value === 'string' ? value.split(/\s+/) : []; }
function countMatches(value, pattern) { return Array.from(value.matchAll(pattern)).length; }
function sha256(value) { return createHash('sha256').update(value).digest('hex'); }
function isRecord(value) { return Boolean(value) && typeof value === 'object' && !Array.isArray(value); }
function errorMessage(value) { return isRecord(value) ? String(value.message || value.error || '') : ''; }
function delay(ms) { return new Promise((resolve) => setTimeout(resolve, ms)); }
function positiveInt(value) { const parsed = Number.parseInt(String(value), 10); if (!Number.isFinite(parsed) || parsed <= 0) throw new Error('expected positive integer option'); return parsed; }
function booleanValue(value) { return ['1', 'true', 'yes', 'on'].includes(String(value).toLowerCase()); }
function trimSlash(value) { return String(value).replace(/\/+$/, ''); }
function resolveURL(endpoint, pathname) { return new URL(pathname, `${trimSlash(endpoint)}/`).toString(); }
function safeName(value) { return String(value).toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '') || 'article'; }
function redactEndpoint(value) { return value.replace(/([?&]token=)[^&]+/gi, '$1<redacted>'); }
function parseArgs(args) {
  const options = {};
  for (let index = 0; index < args.length; index++) {
    const arg = args[index]; if (!arg.startsWith('--')) throw new Error(`unexpected argument ${arg}`);
    const [raw, inline] = arg.slice(2).split('=', 2); const key = raw.replace(/-([a-z])/g, (_, char) => char.toUpperCase());
    if (inline !== undefined) options[key] = inline;
    else if (!args[index + 1] || args[index + 1].startsWith('--')) options[key] = 'true';
    else options[key] = args[++index];
  }
  return options;
}

if (process.argv[1] && path.resolve(process.argv[1]) === scriptPath) {
  main().catch((error) => { console.error(error instanceof Error ? error.stack || error.message : String(error)); process.exit(1); });
}
