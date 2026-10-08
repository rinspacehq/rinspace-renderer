#!/usr/bin/env node
import process from 'node:process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import katex from 'katex';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const katexPackage = JSON.parse(fs.readFileSync(path.join(__dirname, 'node_modules/katex/package.json'), 'utf8'));
const defaultMacros = {
  '\\ket': '\\left|#1\\right\\rangle',
  '\\bra': '\\left\\langle#1\\right|',
  '\\braket': '\\left\\langle#1\\,\\middle|\\,#2\\right\\rangle',
  '\\norm': '\\left\\lVert#1\\right\\rVert',
  '\\abs': '\\left\\lvert#1\\right\\rvert',
  '\\innerproduct': '\\left\\langle#1,#2\\right\\rangle',
  '\\coloneqq': '\\mathrel{\\vcenter{:}}=',
  '\\xlongrightarrow': '\\xrightarrow{#1}',
};

const input = await readStdin();

try {
  const request = JSON.parse(input || '{}');
  if (Array.isArray(request.requests)) {
    const macros = {
      ...defaultMacros,
      ...(isPlainObject(request.macros) ? request.macros : {}),
    };
    writeJSON({
      ok: true,
      engine: 'katex',
      version: katexPackage.version,
      results: request.requests.map((item) => renderOne(item, macros)),
    });
    process.exitCode = 0;
  } else {
    const source = typeof request.source === 'string' ? request.source : '';
    const displayMode = Boolean(request.displayMode);
    const macros = {
      ...defaultMacros,
      ...(isPlainObject(request.macros) ? request.macros : {}),
    };

    const rendered = renderOne({ source, displayMode }, macros);
    if (!rendered.ok) {
      throw Object.assign(new Error(rendered.message), { code: rendered.code });
    }

    writeJSON({
      ok: true,
      engine: 'katex',
      version: katexPackage.version,
      html: rendered.html,
    });
  }
} catch (error) {
  writeJSON({
    ok: false,
    engine: 'katex',
    version: katexPackage.version,
    code: error?.code || error?.name || 'katex.render.failed',
    message: error?.message || String(error),
  });
  process.exitCode = 2;
}

function renderOne(item, macros) {
  const source = typeof item?.source === 'string' ? item.source : '';
  const displayMode = Boolean(item?.displayMode);
  try {
    if (!source.trim()) {
      throw Object.assign(new Error('math source is empty'), { code: 'math.source.empty' });
    }
    const html = katex.renderToString(source, {
      displayMode,
      throwOnError: true,
      strict: 'error',
      trust: false,
      output: 'html',
      macros,
    });
    return { ok: true, html };
  } catch (error) {
    return {
      ok: false,
      code: error?.code || error?.name || 'katex.render.failed',
      message: error?.message || String(error),
    };
  }
}

async function readStdin() {
  let body = '';
  process.stdin.setEncoding('utf8');
  for await (const chunk of process.stdin) {
    body += chunk;
  }
  return body;
}

function writeJSON(value) {
  process.stdout.write(`${JSON.stringify(value)}\n`);
}

function isPlainObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}
