import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';

const request = JSON.stringify({
  source: '\\not\\exists x',
  displayMode: true,
});

const stdout = execFileSync(process.execPath, ['render-mathjax.mjs'], {
  cwd: new URL('.', import.meta.url),
  input: `${request}\n`,
  encoding: 'utf8',
  maxBuffer: 10 * 1024 * 1024,
});

const result = JSON.parse(stdout);
if (!result.ok) {
  throw new Error(`MathJax smoke failed: ${result.code || 'unknown'} ${result.message || ''}`);
}
for (const [field, expected] of [
  ['engine', 'mathjax-chtml'],
  ['version', '4.1.3'],
  ['output', 'chtml'],
]) {
  if (result[field] !== expected) {
    throw new Error(`Expected ${field}=${expected}, got ${result[field]}`);
  }
}
for (const expected of ['mjx-container', '∄', '/fonts/mathjax-newcm/woff2']) {
  if (!stdout.includes(expected)) {
    throw new Error(`Expected MathJax smoke output to contain ${expected}`);
  }
}

const stretchyBrace = JSON.parse(execFileSync(process.execPath, ['render-mathjax.mjs'], {
  cwd: new URL('.', import.meta.url),
  input: `${JSON.stringify({
    source: String.raw`\left\{\begin{matrix}a\\b\\c\\d\\e\end{matrix}\right\}`,
    displayMode: true,
  })}\n`,
  encoding: 'utf8', maxBuffer: 10 * 1024 * 1024,
}));
if (!stretchyBrace.ok || !stretchyBrace.html.includes('<mjx-mid>')) {
  throw new Error('Pinned MathJax did not emit the expected stretchy brace midpoint.');
}

// Keep the finalizer's explicit CHTML schema aligned with the exact MathJax
// worker output. A package change or missed tag must fail before deployment.
const finalizerSource = readFileSync(new URL('../markdown/finalizer.mjs', import.meta.url), 'utf8');
const mathJaxTagList = finalizerSource.match(/const mathJaxTags = Object\.freeze\(\[([\s\S]*?)\]\);/);
if (!mathJaxTagList) throw new Error('Markdown finalizer MathJax tag contract is missing.');
const allowedMathJaxTags = new Set([...mathJaxTagList[1].matchAll(/'([^']+)'/g)].map((match) => match[1]));
const schemaProbeSources = [
  String.raw`\left\{\begin{matrix}a\\b\\c\\d\\e\end{matrix}\right\}`,
  String.raw`\begin{aligned}a&=b+c\\d&=e+f\end{aligned}`,
  String.raw`\begin{array}{c|c}a&b\\c&d\end{array}`,
  String.raw`\overset{x}{\longrightarrow}+\underbrace{a+b}_{c}`,
  String.raw`\sqrt[3]{\frac{x_1^2}{y}}`,
  String.raw`\sum_{i=1}^n \frac{1}{i}`,
  String.raw`\begin{cases}x^2 & x>0\\0 & x\le 0\end{cases}`,
  String.raw`\begin{multline}a+b+c+d+e+f\\+g+h+i+j+k+l\end{multline}`,
  String.raw`\xrightarrow[below]{above}`,
];
const schemaProbe = JSON.parse(execFileSync(process.execPath, ['render-mathjax.mjs'], {
  cwd: new URL('.', import.meta.url),
  input: `${JSON.stringify({ requests: schemaProbeSources.map((source) => ({ source, displayMode: true })) })}\n`,
  encoding: 'utf8', maxBuffer: 10 * 1024 * 1024,
}));
if (!schemaProbe.ok || schemaProbe.results.length !== schemaProbeSources.length || schemaProbe.results.some((item) => !item.ok)) {
  throw new Error('Pinned MathJax schema probe did not render every formula.');
}
const emittedMathJaxTags = new Set(schemaProbe.results.flatMap((item) =>
  [...item.html.matchAll(/<\/?(mjx-[a-z0-9-]+)/g)].map((match) => match[1])));
const unapprovedMathJaxTags = [...emittedMathJaxTags].filter((tag) => !allowedMathJaxTags.has(tag));
if (unapprovedMathJaxTags.length) {
  throw new Error(`Pinned MathJax emitted tags missing from the Markdown finalizer: ${unapprovedMathJaxTags.join(', ')}`);
}

const warmInput = [
  {
    contractVersion: 'rin-node-worker/v1',
    id: 'smoke-render',
    operation: 'mathjax.render',
    payload: JSON.parse(request),
  },
  {
    contractVersion: 'rin-node-worker/v1',
    id: 'smoke-health',
    operation: 'health',
    payload: {},
  },
].map((value) => JSON.stringify(value)).join('\n');
const warmStdout = execFileSync(process.execPath, ['render-mathjax.mjs', '--ndjson-worker'], {
  cwd: new URL('.', import.meta.url),
  input: `${warmInput}\n`,
  encoding: 'utf8',
  maxBuffer: 10 * 1024 * 1024,
});
const warmResponses = warmStdout.trim().split('\n').map((line) => JSON.parse(line));
if (warmResponses.length !== 2 || warmResponses[0].id !== 'smoke-render' || warmResponses[1].id !== 'smoke-health') {
  throw new Error('Warm MathJax worker did not preserve NDJSON request correlation');
}
if (!warmResponses[0].ok || warmResponses[0].result.html !== result.html || warmResponses[0].result.css !== result.css) {
  throw new Error('Warm MathJax output differs from process-per-request output');
}
if (!warmResponses[1].ok || warmResponses[1].result.ready !== true || !warmResponses[1].metrics?.rssBytes) {
  throw new Error('Warm MathJax health or metrics response is invalid');
}

console.log(`MathJax smoke ok: ${result.engine} ${result.version} ${result.output}; warm NDJSON parity ok`);

const mathmlRequest = { source: '<math><msup><mi>x</mi><mn>2</mn></msup></math>', inputFormat: 'mathml' };
const mathmlOutput = JSON.parse(execFileSync(process.execPath, ['render-mathjax.mjs'], {
  cwd: new URL('.', import.meta.url), input: `${JSON.stringify(mathmlRequest)}\n`, encoding: 'utf8', maxBuffer: 10 * 1024 * 1024,
}));
if (!mathmlOutput.ok || !mathmlOutput.html.includes('<mjx-container') || mathmlOutput.html.includes('data-latex=')) {
  throw new Error('MathML input did not use the dedicated MathJax MathML processor');
}
const mathmlWarm = JSON.parse(execFileSync(process.execPath, ['render-mathjax.mjs', '--ndjson-worker'], {
  cwd: new URL('.', import.meta.url),
  input: `${JSON.stringify({ contractVersion: 'rin-node-worker/v1', id: 'mathml', operation: 'mathjax.render', payload: mathmlRequest })}\n`,
  encoding: 'utf8', maxBuffer: 10 * 1024 * 1024,
}).trim());
if (!mathmlWarm.ok || mathmlWarm.result.html !== mathmlOutput.html) {
  throw new Error('MathML process and warm-worker outputs differ');
}
