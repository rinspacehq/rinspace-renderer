#!/usr/bin/env node
import process from 'node:process';
import fs from 'node:fs';
import { createRequire } from 'node:module';
import { mathjax } from '@mathjax/src/js/mathjax.js';
import { TeX } from '@mathjax/src/js/input/tex.js';
import { MathML } from '@mathjax/src/js/input/mathml.js';
import { CHTML } from '@mathjax/src/js/output/chtml.js';
import { liteAdaptor } from '@mathjax/src/js/adaptors/liteAdaptor.js';
import { RegisterHTMLHandler } from '@mathjax/src/js/handlers/html.js';
import { AssistiveMmlHandler } from '@mathjax/src/js/a11y/assistive-mml.js';
import '@mathjax/src/js/util/asyncLoad/esm.js';

import '@mathjax/src/js/input/tex/base/BaseConfiguration.js';
import '@mathjax/src/js/input/tex/ams/AmsConfiguration.js';
import '@mathjax/src/js/input/tex/color/ColorConfiguration.js';
import '@mathjax/src/js/input/tex/extpfeil/ExtpfeilConfiguration.js';
import '@mathjax/src/js/input/tex/newcommand/NewcommandConfiguration.js';
import '@mathjax/src/js/input/tex/configmacros/ConfigMacrosConfiguration.js';
import '@mathjax/src/js/input/tex/boldsymbol/BoldsymbolConfiguration.js';

const WORKER_CONTRACT = 'rin-node-worker/v1';
const MAX_WORKER_REQUEST_BYTES = positiveInteger(
  process.env.RIN_NODE_WORKER_MAX_REQUEST_BYTES,
  4 * 1024 * 1024,
);
const require = createRequire(import.meta.url);
const mathjaxPackagePath = require.resolve('@mathjax/src/package.json');
const mathjaxPackage = JSON.parse(fs.readFileSync(mathjaxPackagePath, 'utf8'));

const EM = Number(process.env.RIN_RENDERER_MATHJAX_EM || 16);
const EX = Number(process.env.RIN_RENDERER_MATHJAX_EX || 8);
const WIDTH = Number(process.env.RIN_RENDERER_MATHJAX_CONTAINER_WIDTH || 80 * EM);
const FONT_URL = process.env.RIN_RENDERER_MATHJAX_FONT_URL || '/fonts/mathjax-newcm/woff2';

const defaultMacros = {
  ket: ['\\left|#1\\right\\rangle', 1],
  bra: ['\\left\\langle#1\\right|', 1],
  braket: ['\\left\\langle#1\\,\\middle|\\,#2\\right\\rangle', 2],
  norm: ['\\left\\lVert#1\\right\\rVert', 1],
  abs: ['\\left\\lvert#1\\right\\rvert', 1],
  innerproduct: ['\\left\\langle#1,#2\\right\\rangle', 2],
  coloneqq: '\\mathrel{\\vcenter{:}}=',
  xlongrightarrow: ['\\xrightarrow{#1}', 1],
};

const adaptor = liteAdaptor({ fontSize: EM });
// CHTML is only a visual layer. Typst publishes the semantic MathML it compiled,
// so the MathML input path also mirrors every formula back as accessible MathML
// inside MathJax's own hidden `mjx-assistive-mml` node. The LaTeX input path
// keeps its existing contract of replacing source MathML outright, so the mirror
// is enabled per document rather than globally.
AssistiveMmlHandler(RegisterHTMLHandler(adaptor));

if (process.argv.includes('--ndjson-worker')) {
  await runNDJSONWorker();
} else {
  await runOneShot();
}

async function runOneShot() {
  try {
    const input = await readStdin();
    const request = JSON.parse(input || '{}');
    const result = await renderRequest(request);
    writeJSON(result);
    if (!result.ok) process.exitCode = 2;
  } catch (error) {
    writeJSON(renderFailure(error));
    process.exitCode = 2;
  }
}

async function runNDJSONWorker() {
  process.stdin.setEncoding('utf8');
  let buffered = '';
  let tasks = 0;

  for await (const chunk of process.stdin) {
    buffered += chunk;
    if (Buffer.byteLength(buffered, 'utf8') > MAX_WORKER_REQUEST_BYTES && !buffered.includes('\n')) {
      throw new Error('node worker request exceeds byte limit');
    }
    while (true) {
      const newline = buffered.indexOf('\n');
      if (newline < 0) break;
      const line = buffered.slice(0, newline);
      buffered = buffered.slice(newline + 1);
      if (!line.trim()) continue;
      if (Buffer.byteLength(line, 'utf8') > MAX_WORKER_REQUEST_BYTES) {
        throw new Error('node worker request exceeds byte limit');
      }
      const response = await handleWorkerMessage(line);
      tasks += 1;
      response.metrics = { tasks, rssBytes: process.memoryUsage().rss };
      writeJSON(response);
    }
  }
  if (buffered.trim()) throw new Error('node worker received unterminated NDJSON');
}

async function handleWorkerMessage(line) {
  let request;
  try {
    request = JSON.parse(line);
  } catch (error) {
    throw new Error(`invalid worker JSON: ${error.message}`);
  }
  const id = typeof request?.id === 'string' ? request.id : '';
  const base = { contractVersion: WORKER_CONTRACT, id };
  if (request?.contractVersion !== WORKER_CONTRACT || !id) {
    return { ...base, ok: false, error: { code: 'nodeworker.contract.invalid', message: 'invalid worker contract or request id' } };
  }
  if (Number.isFinite(request.deadlineUnixMs) && Date.now() >= request.deadlineUnixMs) {
    return { ...base, ok: false, error: { code: 'nodeworker.deadline.exceeded', message: 'request deadline expired before execution' } };
  }
  if (request.operation === 'health') {
    return {
      ...base,
      ok: true,
      result: { ready: true, engine: 'mathjax-chtml', version: mathjaxPackage.version, contractVersion: WORKER_CONTRACT },
    };
  }
  if (request.operation !== 'mathjax.render_batch' && request.operation !== 'mathjax.render') {
    return { ...base, ok: false, error: { code: 'nodeworker.operation.unsupported', message: 'unsupported worker operation' } };
  }
  try {
    return { ...base, ok: true, result: await renderRequest(request.payload || {}) };
  } catch (error) {
    return { ...base, ok: false, error: { code: error?.code || 'mathjax.worker.failed', message: error?.message || String(error) } };
  }
}

async function renderRequest(request) {
  if (Array.isArray(request.requests)) {
    const batchMacros = normalizeMacros(isPlainObject(request.macros) ? request.macros : {});
    const groups = new Map();
    request.requests.forEach((item, index) => {
      const macros = {
        ...batchMacros,
        ...normalizeMacros(isPlainObject(item?.macros) ? item.macros : {}),
      };
      const inputFormat = item?.inputFormat === 'mathml' ? 'mathml' : 'tex';
      const key = `${inputFormat}:${stableMacrosKey(macros)}`;
      if (!groups.has(key)) groups.set(key, { runtime: createRuntime(macros, inputFormat), items: [] });
      groups.get(key).items.push({ index, item });
    });
    const results = new Array(request.requests.length);
    const css = [];
    for (const group of groups.values()) {
      const rendered = await Promise.all(group.items.map(({ item }) => renderOne(group.runtime.document, item)));
      group.items.forEach(({ index }, resultIndex) => { results[index] = rendered[resultIndex]; });
      css.push(adaptor.cssText(group.runtime.chtml.styleSheet(group.runtime.document)));
    }
    return {
      ok: true,
      engine: 'mathjax-chtml',
      version: mathjaxPackage.version,
      output: 'chtml',
      results,
      css: css.join('\n'),
    };
  }
  const runtime = createRuntime(normalizeMacros(isPlainObject(request.macros) ? request.macros : {}), request.inputFormat === 'mathml' ? 'mathml' : 'tex');
  const rendered = await renderOne(runtime.document, request);
  if (!rendered.ok) {
    throw Object.assign(new Error(rendered.message), { code: rendered.code });
  }
  return {
    ok: true,
    engine: 'mathjax-chtml',
    version: mathjaxPackage.version,
    output: 'chtml',
    html: rendered.html,
    css: adaptor.cssText(runtime.chtml.styleSheet(runtime.document)),
  };
}

// LaTeX works are resolved by LaTeXML, which expands the author's preamble and
// packages before this engine ever sees a formula. The macro vocabulary is
// therefore owned by the source and by LaTeXML; this engine deliberately keeps
// no recovery pack, because silently defining a command the document never
// declared would publish math the source cannot compile and would disagree with
// the TeX SVG fallback and the reader. An unexpanded command that reaches this
// engine is a source defect and must surface as one.
function createRuntime(customMacros, inputFormat = 'tex') {
  const input = inputFormat === 'mathml'
    ? new MathML({ allowHtmlInTokenNodes: false })
    : createTexInput({ ...defaultMacros, ...customMacros });
  const chtml = new CHTML({
    fontURL: FONT_URL,
    displayOverflow: 'overflow',
    linebreaks: { inline: false },
  });
  return {
    input,
    chtml,
    document: mathjax.document('', {
      InputJax: input,
      OutputJax: chtml,
      enableAssistiveMml: inputFormat === 'mathml',
    }),
  };
}

function stableMacrosKey(macros) {
  return JSON.stringify(Object.entries(macros).sort(([left], [right]) => left.localeCompare(right)));
}

function renderFailure(error) {
  return {
    ok: false,
    engine: 'mathjax-chtml',
    version: mathjaxPackage.version,
    output: 'chtml',
    code: error?.code || error?.name || 'mathjax.render.failed',
    message: error?.message || String(error),
  };
}

function createTexInput(macros) {
  return new TeX({
    packages: ['base', 'ams', 'color', 'extpfeil', 'newcommand', 'configmacros', 'boldsymbol'],
    macros,
    formatError(_jax, error) {
      throw error;
    },
  });
}

async function renderOne(document, item) {
  const source = typeof item?.source === 'string' ? item.source.trim() : '';
  const displayMode = Boolean(item?.displayMode);
  try {
    if (!source) {
      throw Object.assign(new Error('math source is empty'), { code: 'math.source.empty' });
    }
    if (item?.inputFormat !== undefined && item.inputFormat !== 'tex' && item.inputFormat !== 'mathml') {
      throw Object.assign(new Error('math input format is invalid'), { code: 'math.input_format.invalid' });
    }
    if (item?.inputFormat === 'mathml' && (!source.startsWith('<math') || source.length > 32768)) {
      throw Object.assign(new Error('MathML source is invalid'), { code: 'mathml.source.invalid' });
    }
    const node = await document.convertPromise(source, {
      display: displayMode,
      em: EM,
      ex: EX,
      containerWidth: WIDTH,
    });
    const html = adaptor.outerHTML(node);
    if (!html.trim()) {
      throw Object.assign(new Error('MathJax returned empty HTML'), { code: 'mathjax.output.empty' });
    }
    return { ok: true, html };
  } catch (error) {
    return {
      ok: false,
      code: error?.code || error?.name || 'mathjax.render.failed',
      message: error?.message || String(error),
    };
  }
}

function normalizeMacros(value) {
  const out = {};
  for (const [rawName, rawDefinition] of Object.entries(value || {})) {
    const name = rawName.replace(/^\\+/, '');
    if (!name) continue;
    if (typeof rawDefinition === 'string') {
      out[name] = rawDefinition;
    } else if (
      Array.isArray(rawDefinition) &&
      typeof rawDefinition[0] === 'string' &&
      Number.isInteger(rawDefinition[1])
    ) {
      out[name] = [rawDefinition[0], rawDefinition[1]];
    }
  }
  return out;
}

async function readStdin() {
  let body = '';
  process.stdin.setEncoding('utf8');
  for await (const chunk of process.stdin) body += chunk;
  return body;
}

function writeJSON(value) {
  process.stdout.write(`${JSON.stringify(value)}\n`);
}

function isPlainObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function positiveInteger(value, fallback) {
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : fallback;
}
