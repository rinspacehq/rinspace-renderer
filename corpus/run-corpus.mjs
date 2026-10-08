#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import crypto from 'node:crypto';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const REQUIRED_FEATURES = [
  'article',
  'cjk',
  'references',
  'citations',
  'bibliography',
  'theorem',
  'equations',
  'mathjax-reader-contract',
  'figures',
  'tables',
  'diagrams',
  'large-boundary',
];

const severityRank = new Map([
  ['info', 0],
  ['warning', 1],
  ['error', 2],
]);

async function main() {
  const options = parseArgs(process.argv.slice(2));
  const root = path.resolve(options.root || __dirname);
  const fixturesDir = path.resolve(options.fixtures || path.join(root, 'fixtures'));
  const out = path.resolve(options.out || path.join(root, 'reports', 'corpus-report.json'));
  const endpoint = trimTrailingSlash(options.endpoint || process.env.RIN_RENDERER_CORPUS_ENDPOINT || '');
  const token = options.token || process.env.RIN_RENDERER_CORPUS_TOKEN || process.env.RIN_RENDERER_SERVICE_TOKEN || '';
  const configuredEngines = splitList(options.engines || process.env.RIN_RENDERER_CORPUS_ENGINES || '');
  const snapshotDir = options.snapshotDir || process.env.RIN_RENDERER_CORPUS_SNAPSHOT_DIR || '';
  const baselinePath = options.baseline || process.env.RIN_RENDERER_CORPUS_BASELINE || '';
  const deltasOut = options.deltasOut || process.env.RIN_RENDERER_CORPUS_DELTAS_OUT || '';
  const mode = options.mode || (endpoint ? 'render' : 'offline');
  if (!['offline', 'render'].includes(mode)) {
    throw new Error(`Unsupported corpus mode: ${mode}`);
  }

  const fixtures = readFixtures(fixturesDir);
  const coverage = corpusCoverage(fixtures);
  const results = [];

  for (const fixture of fixtures) {
    const validation = validateFixture(fixture);
    if (mode === 'offline' || validation.errors.length > 0) {
      results.push({
        id: fixture.id,
        title: fixture.title,
        mode: 'offline',
        status: validation.errors.length > 0 ? 'failed' : 'validated',
        features: fixture.features,
        validation,
        runs: [],
      });
      continue;
    }

    const engines = configuredEngines.length > 0 ? configuredEngines : fixture.engines;
    const runs = [];
    for (const engine of engines) {
      runs.push(await renderFixture({ fixture, endpoint, token, engine, snapshotDir }));
    }
    results.push({
      id: fixture.id,
      title: fixture.title,
      mode: 'render',
      status: runs.some((run) => run.status === 'failed') ? 'failed' : 'passed',
      features: fixture.features,
      validation,
      runs,
    });
  }

  const summary = summarize(results, coverage, mode);
  const report = {
    version: 1,
    generatedAt: new Date().toISOString(),
    mode,
    endpointConfigured: endpoint !== '',
    engines: configuredEngines,
    coverage,
    summary,
    fixtures: results,
  };
  if (baselinePath) {
    const baseline = JSON.parse(fs.readFileSync(baselinePath, 'utf8'));
    report.deltas = compareReports(baseline, report);
    if (deltasOut) {
      fs.mkdirSync(path.dirname(path.resolve(deltasOut)), { recursive: true });
      fs.writeFileSync(path.resolve(deltasOut), `${JSON.stringify(report.deltas, null, 2)}\n`);
    }
  }

  fs.mkdirSync(path.dirname(out), { recursive: true });
  fs.writeFileSync(out, `${JSON.stringify(report, null, 2)}\n`);
  console.log(`Wrote corpus report: ${out}`);
  console.log(`Corpus summary: ${summary.passed} passed, ${summary.validated} validated, ${summary.failed} failed, ${summary.skipped} skipped`);

  if (summary.failed > 0 || coverage.missing.length > 0) {
    process.exit(1);
  }
}

function parseArgs(args) {
  const options = {};
  for (let index = 0; index < args.length; index++) {
    const arg = args[index];
    if (!arg.startsWith('--')) {
      throw new Error(`Unexpected argument: ${arg}`);
    }
    const [rawKey, inlineValue] = arg.slice(2).split('=', 2);
    const key = rawKey.replace(/-([a-z])/g, (_, ch) => ch.toUpperCase());
    if (inlineValue !== undefined) {
      options[key] = inlineValue;
    } else {
      const next = args[index + 1];
      if (!next || next.startsWith('--')) {
        options[key] = 'true';
      } else {
        options[key] = next;
        index++;
      }
    }
  }
  return options;
}

function readFixtures(fixturesDir) {
  const entries = fs.readdirSync(fixturesDir, { withFileTypes: true })
    .filter((entry) => entry.isDirectory())
    .map((entry) => entry.name)
    .sort();
  return entries.flatMap((entry) => {
    const dir = path.join(fixturesDir, entry);
    const specPath = path.join(dir, 'corpus.json');
    if (!fs.existsSync(specPath)) {
      // Non-LaTeX fixture trees (for example fixtures/typst) share this parent
      // directory but are exercised by their own workflows, not by this corpus.
      console.log(`Skipping ${entry}: ${dir} has no corpus.json`);
      return [];
    }
    const projectDir = path.join(dir, 'project');
    const spec = JSON.parse(fs.readFileSync(specPath, 'utf8'));
    return [{
      dir,
      projectDir,
      id: spec.id || entry,
      title: spec.title || spec.id || entry,
      mainFile: spec.mainFile || 'main.tex',
      engines: Array.isArray(spec.engines) && spec.engines.length > 0 ? spec.engines : ['latexml'],
      features: Array.isArray(spec.features) ? spec.features : [],
      invariants: spec.invariants || {},
    }];
  });
}

function validateFixture(fixture) {
  const errors = [];
  const warnings = [];
  if (path.basename(fixture.dir) !== fixture.id) {
    errors.push(`fixture id ${fixture.id} must match directory ${path.basename(fixture.dir)}`);
  }
  if (!fs.existsSync(fixture.projectDir) || !fs.statSync(fixture.projectDir).isDirectory()) {
    errors.push('missing project directory');
  }
  if (!fs.existsSync(path.join(fixture.projectDir, fixture.mainFile))) {
    errors.push(`missing mainFile: ${fixture.mainFile}`);
  }
  if (fixture.features.length === 0) {
    errors.push('features must not be empty');
  }
  if (!fixture.invariants || typeof fixture.invariants !== 'object') {
    errors.push('invariants must be an object');
  }
  if (!Array.isArray(fixture.engines) || fixture.engines.length === 0) {
    errors.push('engines must not be empty');
  }
  for (const engine of fixture.engines) {
    if (!['latexml', 'auto'].includes(engine)) {
      warnings.push(`non-standard engine: ${engine}`);
    }
  }
  return { errors, warnings };
}

function corpusCoverage(fixtures) {
  const covered = new Set();
  for (const fixture of fixtures) {
    for (const feature of fixture.features) {
      covered.add(feature);
    }
  }
  const missing = REQUIRED_FEATURES.filter((feature) => !covered.has(feature));
  return {
    required: REQUIRED_FEATURES,
    covered: [...covered].sort(),
    missing,
  };
}

async function renderFixture({ fixture, endpoint, token, engine, snapshotDir }) {
  const zip = createZip(readProjectFiles(fixture.projectDir));
  const form = new FormData();
  form.append('source', new Blob([zip], { type: 'application/zip' }), `${fixture.id}.zip`);
  form.append('engine', engine);
  form.append('mainFile', fixture.mainFile);
  form.append('title', fixture.title);

  const headers = { Accept: 'application/json' };
  if (token) {
    headers.Authorization = `Bearer ${token}`;
    headers['X-Rin-Renderer-Token'] = token;
  }

  const startedAt = Date.now();
  let response;
  let payload;
  try {
    response = await fetch(projectEndpoint(endpoint), {
      method: 'POST',
      headers,
      body: form,
    });
    payload = await response.json();
  } catch (error) {
    return {
      engine,
      status: 'failed',
      elapsedMs: Date.now() - startedAt,
      error: error instanceof Error ? error.message : String(error),
      invariants: [],
    };
  }

  const html = typeof payload.html === 'string' ? payload.html : '';
  const diagnostics = Array.isArray(payload.diagnostics) ? payload.diagnostics : [];
  const diagrams = Array.isArray(payload.diagrams) ? payload.diagrams : [];
  const math = mathSummary(payload);
  if (snapshotDir) {
    writeSnapshots({ snapshotDir, fixture, engine, html, diagnostics, diagrams });
  }
  const invariants = evaluateInvariants(fixture, payload);
  const failed = !response.ok || invariants.some((item) => item.status === 'failed');
  return {
    engine,
    status: failed ? 'failed' : 'passed',
    httpStatus: response.status,
    elapsedMs: Date.now() - startedAt,
    rendererEngine: payload.engine,
    fallback: Boolean(payload.fallback),
    primaryEngine: payload.primaryEngine || '',
    fallbackEngine: payload.fallbackEngine || '',
    htmlBytes: Buffer.byteLength(html),
    htmlSha256: sha256Hex(html),
    htmlDiagnosticFingerprints: diagnosticHTMLFingerprints(html),
    htmlTextNodes: diagnosticTextNodes(html),
    diagnosticCounts: diagnosticCounts(diagnostics),
    diagnosticCodes: diagnosticCodes(diagnostics),
    diagramCount: diagrams.length,
    math,
    mathHtmlCounts: mathHtmlCounts(html),
    cloudbaseObjectIds: diagrams.map((diagram) => diagram.objectId).filter(Boolean),
    error: response.ok ? '' : payload.error || `HTTP ${response.status}`,
    invariants,
  };
}

function diagnosticTextNodes(html) {
  const voidElements = new Set(['area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input', 'link', 'meta', 'source', 'track', 'wbr']);
  const stack = [];
  const nodes = [];
  for (const token of html.match(/<[^>]*>|[^<]+/g) || []) {
    if (!token.startsWith('<')) {
      if (token.length > 0) nodes.push({parent: stack.at(-1) || '#document', sha256: sha256Hex(token)});
      continue;
    }
    const close = token.match(/^<\/\s*([-:\w]+)/);
    if (close) {
      const tag = close[1].toLowerCase();
      while (stack.length > 0 && stack.pop() !== tag) {}
      continue;
    }
    const open = token.match(/^<\s*([-:\w]+)/);
    if (!open || token.startsWith('<!--') || token.startsWith('<!') || token.startsWith('<?')) continue;
    const tag = open[1].toLowerCase();
    if (!token.endsWith('/>') && !voidElements.has(tag)) stack.push(tag);
  }
  return nodes;
}

function diagnosticHTMLFingerprints(html) {
  const requestNeutral = html.replace(/rnd_[a-f0-9]{16}|rnd_[0-9]+/g, 'rnd_<request>');
  const urlQueryNeutral = html.replace(/(https?:\/\/[^\s"'<>?#]+)(?:\?[^\s"'<>#]*)?(?:#[^\s"'<>]*)?/g, '$1?<query>#<fragment>');
  const idHrefNeutral = html.replace(/\s(?:id|href)=(['"])[^'"]*\1/gi, (value, quote) => {
    const name = /^\s*([^=]+)/.exec(value)?.[1] || 'id';
    return ` ${name}=${quote}<value>${quote}`;
  });
  const srcHrefNeutral = html.replace(/\s(?:src|href)=(['"])[^'"]*\1/gi, (value, quote) => {
    const name = /^\s*([^=]+)/.exec(value)?.[1] || 'src';
    return ` ${name}=${quote}<value>${quote}`;
  });
  const attributeNeutral = html.replace(/(\s[-:\w]+)=(['"])[^'"]*\2/g, '$1=$2<value>$2');
  const styleTextNeutral = html.replace(/(<style\b[^>]*>)[\s\S]*?(<\/style>)/gi, '$1<style-text>$2');
  const allTextNeutral = html.replace(/(>)[^<]*(<)/g, '$1<text>$2');
  return {
    requestNeutralSha256: sha256Hex(requestNeutral),
    urlQueryNeutralSha256: sha256Hex(urlQueryNeutral),
    idHrefNeutralSha256: sha256Hex(idHrefNeutral),
    srcHrefNeutralSha256: sha256Hex(srcHrefNeutral),
    attributeNeutralSha256: sha256Hex(attributeNeutral),
    styleTextNeutralSha256: sha256Hex(styleTextNeutral),
    allTextNeutralSha256: sha256Hex(allTextNeutral),
  };
}

function evaluateInvariants(fixture, payload) {
  const invariants = fixture.invariants || {};
  const html = typeof payload.html === 'string' ? payload.html : '';
  const diagnostics = Array.isArray(payload.diagnostics) ? payload.diagnostics : [];
  const diagrams = Array.isArray(payload.diagrams) ? payload.diagrams : [];
  const results = [];

  for (const expected of invariants.htmlContains || []) {
    results.push(check(`html contains ${JSON.stringify(expected)}`, html.includes(expected)));
  }
  for (const unexpected of invariants.htmlNotContains || []) {
    results.push(check(`html omits ${JSON.stringify(unexpected)}`, !html.includes(unexpected)));
  }
  for (const [expected, min] of Object.entries(invariants.htmlContainsCountAtLeast || {})) {
    results.push(check(`html contains ${JSON.stringify(expected)} at least ${min}`, countOccurrences(html, expected) >= min));
  }
  if (invariants.noLocalDiagramUrls !== false) {
    results.push(check('html has no local diagram URLs', !/(\/rin\/api\/diagrams|\/rendered\/|localhost:\d+)/i.test(html)));
  }
  if (invariants.noLateXMLMathML) {
    results.push(check('html has no LaTeXML MathML elements', !/<math\b/i.test(html)));
  }
  if (invariants.noLateXMLEquationTables) {
    results.push(check('html has no LaTeXML equation tables', !/\bltx_(?:equationgroup|eqn_table|eqn_row|eqn_cell)\b/i.test(html)));
  }
  if (invariants.noKatexErrors) {
    results.push(check('html has no KaTeX errors', !/\bkatex-error\b/i.test(html)));
  }
  if (Number.isInteger(invariants.minDiagramCount)) {
    results.push(check(`diagram count >= ${invariants.minDiagramCount}`, diagrams.length >= invariants.minDiagramCount));
  }
  if (Number.isInteger(invariants.maxDiagramCount)) {
    results.push(check(`diagram count <= ${invariants.maxDiagramCount}`, diagrams.length <= invariants.maxDiagramCount));
  }
  if (invariants.requireCloudBaseDiagrams || invariants.minDiagramCount > 0) {
    const ok = diagrams.every((diagram) =>
      typeof diagram.objectId === 'string' &&
      diagram.objectId.startsWith('diagrams/v1/svg-sha256/') &&
      typeof diagram.cloudbaseUrl === 'string' &&
      /^https?:\/\//.test(diagram.cloudbaseUrl));
    results.push(check('diagrams have CloudBase object IDs and URLs', ok));
  }
  const math = mathSummary(payload);
  if (Number.isInteger(invariants.minMathCount)) {
    results.push(check(`math count >= ${invariants.minMathCount}`, math.count >= invariants.minMathCount, `count=${math.count}`));
  }
  if (Number.isInteger(invariants.minKaTeXCount)) {
    results.push(check(`KaTeX math count >= ${invariants.minKaTeXCount}`, math.katexCount >= invariants.minKaTeXCount, `katexCount=${math.katexCount}`));
  }
  if (Number.isInteger(invariants.minMathJaxCount)) {
    results.push(check(`MathJax math count >= ${invariants.minMathJaxCount}`, math.mathJaxCount >= invariants.minMathJaxCount, `mathJaxCount=${math.mathJaxCount}`));
  }
  if (Number.isInteger(invariants.maxMathFailedCount)) {
    results.push(check(`failed math count <= ${invariants.maxMathFailedCount}`, math.failedCount <= invariants.maxMathFailedCount, `failedCount=${math.failedCount}`));
  }
  if (invariants.diagnosticMaxSeverity) {
    const max = severityRank.get(invariants.diagnosticMaxSeverity) ?? 2;
    const tooSevere = diagnostics.find((diagnostic) => (severityRank.get(diagnostic.severity) ?? 2) > max);
    results.push(check(`diagnostic max severity <= ${invariants.diagnosticMaxSeverity}`, !tooSevere, tooSevere ? `${tooSevere.code}: ${tooSevere.message}` : ''));
  }
  return results;
}

function countOccurrences(value, needle) {
  if (!needle) return 0;
  let count = 0;
  let cursor = 0;
  while (cursor < value.length) {
    const index = value.indexOf(needle, cursor);
    if (index < 0) break;
    count++;
    cursor = index + needle.length;
  }
  return count;
}

function mathSummary(payload) {
  const math = payload && typeof payload.math === 'object' && payload.math !== null ? payload.math : {};
  return {
    count: Number.isFinite(math.count) ? math.count : 0,
    mathJaxCount: Number.isFinite(math.mathJaxCount) ? math.mathJaxCount : 0,
    katexCount: Number.isFinite(math.katexCount) ? math.katexCount : 0,
    svgFallbackCount: Number.isFinite(math.svgFallbackCount) ? math.svgFallbackCount : 0,
    failedCount: Number.isFinite(math.failedCount) ? math.failedCount : 0,
  };
}

function mathHtmlCounts(html) {
  return {
    serverMathJax: countOccurrences(html, 'rin-math-mathjax'),
    serverKaTeX: countOccurrences(html, 'rin-math-katex'),
    serverSVG: countOccurrences(html, 'rin-math-svg'),
    latexmlMathML: (html.match(/<math\b/gi) || []).length,
    latexmlEquationTables: (html.match(/\bltx_(?:equationgroup|eqn_table|eqn_row|eqn_cell)\b/gi) || []).length,
    katexErrors: countOccurrences(html, 'katex-error'),
  };
}

function check(name, ok, detail = '') {
  return {
    name,
    status: ok ? 'passed' : 'failed',
    detail,
  };
}

function diagnosticCounts(diagnostics) {
  const counts = { info: 0, warning: 0, error: 0 };
  for (const diagnostic of diagnostics) {
    const severity = diagnostic.severity || 'error';
    counts[severity] = (counts[severity] || 0) + 1;
  }
  return counts;
}

function diagnosticCodes(diagnostics) {
  return diagnostics
    .map((diagnostic) => `${diagnostic.severity || 'error'}:${diagnostic.engine || ''}:${diagnostic.code || ''}`)
    .sort();
}

function writeSnapshots({ snapshotDir, fixture, engine, html, diagnostics, diagrams }) {
  const dir = path.join(path.resolve(snapshotDir), safeName(fixture.id), safeName(engine));
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, 'body.html'), html);
  fs.writeFileSync(path.join(dir, 'diagnostics.json'), `${JSON.stringify(diagnostics, null, 2)}\n`);
  fs.writeFileSync(path.join(dir, 'diagrams.json'), `${JSON.stringify(diagrams, null, 2)}\n`);
}

function compareReports(baseline, current) {
  const before = comparableRuns(baseline);
  const after = comparableRuns(current);
  const changes = [];
  for (const key of [...new Set([...Object.keys(before), ...Object.keys(after)])].sort()) {
    if (!before[key]) {
      changes.push({ key, type: 'added', before: null, after: after[key] });
      continue;
    }
    if (!after[key]) {
      changes.push({ key, type: 'removed', before: before[key], after: null });
      continue;
    }
    if (JSON.stringify(before[key]) !== JSON.stringify(after[key])) {
      changes.push({ key, type: 'changed', before: before[key], after: after[key] });
    }
  }
  const summary = {
    added: changes.filter((change) => change.type === 'added').length,
    removed: changes.filter((change) => change.type === 'removed').length,
    changed: changes.filter((change) => change.type === 'changed').length,
    total: changes.length,
  };
  const hash = sha256Hex(JSON.stringify(changes));
  return {
    version: 1,
    baselineGeneratedAt: baseline.generatedAt || '',
    currentGeneratedAt: current.generatedAt || '',
    summary,
    hash,
    changes,
  };
}

function comparableRuns(report) {
  const items = {};
  for (const fixture of report.fixtures || []) {
    for (const run of fixture.runs || []) {
      items[`${fixture.id}/${run.engine}`] = {
        status: run.status,
        rendererEngine: run.rendererEngine || '',
        fallback: Boolean(run.fallback),
        primaryEngine: run.primaryEngine || '',
        fallbackEngine: run.fallbackEngine || '',
        htmlSha256: run.htmlSha256 || '',
        htmlBytes: run.htmlBytes || 0,
        diagnosticCodes: run.diagnosticCodes || [],
        diagnosticCounts: run.diagnosticCounts || {},
        diagramCount: run.diagramCount || 0,
        cloudbaseObjectIds: run.cloudbaseObjectIds || [],
        invariants: (run.invariants || []).map((item) => `${item.status}:${item.name}:${item.detail || ''}`).sort(),
      };
    }
  }
  return items;
}

function summarize(results, coverage, mode) {
  const summary = { passed: 0, validated: 0, failed: 0, skipped: 0, mode };
  for (const result of results) {
    if (result.status === 'passed') {
      summary.passed++;
    } else if (result.status === 'validated') {
      summary.validated++;
    } else if (result.status === 'skipped') {
      summary.skipped++;
    } else {
      summary.failed++;
    }
  }
  if (coverage.missing.length > 0) {
    summary.failed++;
  }
  return summary;
}

function readProjectFiles(projectDir) {
  const files = [];
  const walk = (dir) => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const target = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        walk(target);
      } else if (entry.isFile()) {
        files.push({
          path: path.relative(projectDir, target).replaceAll(path.sep, '/'),
          data: fs.readFileSync(target),
        });
      }
    }
  };
  walk(projectDir);
  files.sort((a, b) => a.path.localeCompare(b.path));
  return files;
}

function createZip(files) {
  const localParts = [];
  const centralParts = [];
  let offset = 0;
  for (const file of files) {
    const name = Buffer.from(file.path, 'utf8');
    const data = Buffer.from(file.data);
    const crc = crc32(data);
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0);
    local.writeUInt16LE(20, 4);
    local.writeUInt16LE(0, 6);
    local.writeUInt16LE(0, 8);
    local.writeUInt16LE(0, 10);
    local.writeUInt16LE(0, 12);
    local.writeUInt32LE(crc, 14);
    local.writeUInt32LE(data.length, 18);
    local.writeUInt32LE(data.length, 22);
    local.writeUInt16LE(name.length, 26);
    local.writeUInt16LE(0, 28);
    localParts.push(local, name, data);

    const central = Buffer.alloc(46);
    central.writeUInt32LE(0x02014b50, 0);
    central.writeUInt16LE(20, 4);
    central.writeUInt16LE(20, 6);
    central.writeUInt16LE(0, 8);
    central.writeUInt16LE(0, 10);
    central.writeUInt16LE(0, 12);
    central.writeUInt16LE(0, 14);
    central.writeUInt32LE(crc, 16);
    central.writeUInt32LE(data.length, 20);
    central.writeUInt32LE(data.length, 24);
    central.writeUInt16LE(name.length, 28);
    central.writeUInt16LE(0, 30);
    central.writeUInt16LE(0, 32);
    central.writeUInt16LE(0, 34);
    central.writeUInt16LE(0, 36);
    central.writeUInt32LE(0, 38);
    central.writeUInt32LE(offset, 42);
    centralParts.push(central, name);
    offset += local.length + name.length + data.length;
  }

  const centralOffset = offset;
  const centralSize = centralParts.reduce((sum, part) => sum + part.length, 0);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(0, 4);
  end.writeUInt16LE(0, 6);
  end.writeUInt16LE(files.length, 8);
  end.writeUInt16LE(files.length, 10);
  end.writeUInt32LE(centralSize, 12);
  end.writeUInt32LE(centralOffset, 16);
  end.writeUInt16LE(0, 20);

  return Buffer.concat([...localParts, ...centralParts, end]);
}

function crc32(buffer) {
  let crc = 0xffffffff;
  for (const byte of buffer) {
    crc = (crc >>> 8) ^ crcTable[(crc ^ byte) & 0xff];
  }
  return (crc ^ 0xffffffff) >>> 0;
}

const crcTable = (() => {
  const table = new Uint32Array(256);
  for (let index = 0; index < 256; index++) {
    let value = index;
    for (let bit = 0; bit < 8; bit++) {
      value = value & 1 ? 0xedb88320 ^ (value >>> 1) : value >>> 1;
    }
    table[index] = value >>> 0;
  }
  return table;
})();

main().catch((error) => {
  console.error(error instanceof Error ? error.stack || error.message : String(error));
  process.exit(1);
});

function projectEndpoint(endpoint) {
  if (!endpoint) {
    throw new Error('RIN_RENDERER_CORPUS_ENDPOINT is required in render mode');
  }
  if (endpoint.endsWith('/api/render/projects')) {
    return endpoint;
  }
  return `${endpoint}/api/render/projects`;
}

function trimTrailingSlash(value) {
  return String(value || '').replace(/\/+$/, '');
}

function splitList(value) {
  return String(value || '').split(',').map((item) => item.trim()).filter(Boolean);
}

function sha256Hex(value) {
  return crypto.createHash('sha256').update(value).digest('hex');
}

function safeName(value) {
  return String(value || 'unknown').toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '') || 'unknown';
}
