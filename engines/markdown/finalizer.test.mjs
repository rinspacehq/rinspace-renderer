import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';

import { compileMarkdownDraft } from './compiler.mjs';
import { MARKDOWN_SANITIZER_VERSION, finalizeMarkdownBundle } from './finalizer.mjs';

const projectHash = 'a'.repeat(64);
const imageHash = 'b'.repeat(64);
const source = String.raw`# location

[Jump](#location) and inline $\not\exists k\in\mathbb R$.

$$
P(X>N)=\sum_{k=N+1}^{n}P(X=k)
$$

![A useful caption](images/example.png "Figure title")

~~~js title="sample.js"
const value = "<script>";
~~~

:::diagram{type="tikzpicture" alignment="center"}
\draw (0,0)--(1,1);
:::

Footnote.[^safe]

[^safe]: footnote text

| left | right |
| :--- | ----: |
| one  | two   |

<script>alert(1)</script>
`;

const compiled = await compileMarkdownDraft({
  source, sourcePath: 'docs/article.md', projectHash, language: 'zh-CN',
  assets: [{
    id: 'asset-image', kind: 'project-file', sha256: imageHash, bytes: 123,
    mediaType: 'image/png', projectPath: 'docs/images/example.png',
  }],
}, { adapterVersion: '0.6.0', engineVersion: 'pipeline-test' });

const resolvedWork = resolvedFor(compiled.bundle);
const finalized = await finalizeMarkdownBundle({ bundle: compiled.bundle, resolvedWork }, {
  adapterVersion: '0.6.0', engineVersion: 'pipeline-final-test',
});
assert.equal(finalized.publishable, true);
assert.equal(finalized.sanitizerVersion, MARKDOWN_SANITIZER_VERSION);
assert.equal(finalized.bundle.state, 'final');
assert.equal(finalized.bundle.workUnits.length, 0);
assert.match(finalized.bundle.bundleHash, /^[a-f0-9]{64}$/);
assert.equal(finalized.bundle.pages[0].fragmentFormat, 'html');
assert.equal(finalized.bundle.pages[0].toc[0].id, 'rin-md-location');
assert.equal(finalized.bundle.schemaVersion, 'rin-document-bundle/v2');
assert.ok(finalized.bundle.pages[0].blocks.length > 0);
for (const expected of [
  'id="rin-md-location"', 'href="#rin-md-location"', 'data-rin-asset-path="docs/images/example.png"',
  'class="rin-mathjax-chtml-style"', 'mjx-c2204', 'class="shiki github-light"',
  'data-rin-diagram-object-id=', 'id="rin-md-fn-safe"',
  '<th align="left">left</th>', '<th align="right">right</th>',
]) {
  assert.ok(finalized.bundle.pages[0].fragment.includes(expected), `final HTML omits ${expected}`);
}
for (const forbidden of [
  '<rin-work', '<script', 'onclick=', 'data-rin-project-path=', 'headers=""',
  'aria-describedby=""', 'aria-labelledby=""',
]) {
  assert.ok(!finalized.bundle.pages[0].fragment.includes(forbidden), `final HTML leaked ${forbidden}`);
}
assert.equal(finalized.artifacts.length, 1);
assert.ok(finalized.bundle.diagnostics.some((item) => item.code === 'markdown.raw_html.disabled'));

const officialCHTMLPayload = structuredClone({ bundle: compiled.bundle, resolvedWork });
const officialMath = officialCHTMLPayload.bundle.workUnits.find((unit) => unit.kind === 'math');
officialCHTMLPayload.resolvedWork.units[officialMath.id].html = `<span class="rin-math rin-math-inline rin-math-mathjax" data-rin-math-engine="mathjax-chtml" data-rin-math-source="x" data-rin-math-version="4.1.3"><mjx-container class="MathJax" jax="CHTML" overflow="overflow" display="true"><mjx-math data-latex="x" display="true" class="NCM-N"><mjx-mtable justify="left" data-frame-styles="" style="min-width: 2em; margin-top: 0.284em; margin-bottom: 0.2em"><mjx-table><mjx-itable><mjx-mtr><mjx-mtd style="padding-top: 0.2em; padding-bottom: 0.15em"><mjx-beg></mjx-beg><mjx-mark></mjx-mark><mjx-spacer style="font-family: MJX-NCM-ZERO, serif"></mjx-spacer><mjx-end></mjx-end><mjx-msqrt><mjx-sqrt><mjx-surd></mjx-surd></mjx-sqrt></mjx-msqrt></mjx-mtd></mjx-mtr></mjx-itable></mjx-table></mjx-mtable><mjx-rbox><mjx-mfrac><mjx-frac type="d"><mjx-num><mjx-nstrut type="d"></mjx-nstrut><mjx-mn><mjx-c class="mjx-c31" noic="true">1</mjx-c></mjx-mn></mjx-num><mjx-dbox><mjx-dtable><mjx-line type="d"></mjx-line><mjx-row><mjx-den><mjx-dstrut type="d"></mjx-dstrut><mjx-texatom texclass="ORD"><mjx-mi><mjx-utext variant="italic" extra="true" limits="false">x</mjx-utext></mjx-mi></mjx-texatom></mjx-den></mjx-row></mjx-dtable></mjx-dbox></mjx-frac></mjx-mfrac></mjx-rbox></mjx-math></mjx-container></span>`;
officialCHTMLPayload.resolvedWork.units[officialMath.id].css = [
  '@media (prefers-color-scheme: dark) { mjx-container .mjx-selected { outline: 2px solid #C8C8C8; } }',
  '.NCM-N { font-family: MJX-NCM; }',
  '@font-face { font-family: MJX-NCM; src: url("/fonts/mathjax-newcm/woff2/mjx-ncm-n.woff2") format("woff2"); }',
];
const officialCHTMLFinal = await finalizeMarkdownBundle(officialCHTMLPayload);
assert.match(officialCHTMLFinal.bundle.pages[0].fragment, /class="NCM-N"/);
assert.match(officialCHTMLFinal.bundle.pages[0].fragment, /overflow="overflow"/);
assert.match(officialCHTMLFinal.bundle.pages[0].fragment, /<mjx-beg>/);
assert.match(officialCHTMLFinal.bundle.pages[0].fragment, /font-family: MJX-NCM-ZERO, serif/);
assert.match(officialCHTMLFinal.bundle.pages[0].fragment, /variant="italic"/);
assert.match(officialCHTMLFinal.bundle.pages[0].fragment, /noic="true"/);
assert.match(officialCHTMLFinal.bundle.pages[0].fragment, /justify="left"/);
assert.match(officialCHTMLFinal.bundle.pages[0].fragment, /margin-bottom: 0.2em/);
assert.match(officialCHTMLFinal.bundle.pages[0].fragment, /<mjx-rbox>/);

// Pinned MathJax uses mjx-menclose[data-padding] for \hline in an array. Article 582 exposed
// this exact output. MathJax data attributes are inert renderer metadata and remain confined to
// the explicitly admitted mjx-* custom elements.
const arrayRulePayload = structuredClone(officialCHTMLPayload);
arrayRulePayload.resolvedWork.units[officialMath.id].html =
  arrayRulePayload.resolvedWork.units[officialMath.id].html.replace(
    '<mjx-mtable',
    '<mjx-menclose data-padding="0" data-latex="\\begin{array}{c|cc} \\hline X &amp; 1 &amp; 0 \\\\ \\hline \\end{array}"><mjx-mtable',
  ).replace('</mjx-mtable>', '</mjx-mtable></mjx-menclose>');
const arrayRuleFinal = await finalizeMarkdownBundle(arrayRulePayload);
assert.equal(arrayRuleFinal.publishable, true);
assert.match(arrayRuleFinal.bundle.pages[0].fragment, /<mjx-menclose data-padding="0"/);
const mathDataPayload = structuredClone(officialCHTMLPayload);
mathDataPayload.resolvedWork.units[officialMath.id].html =
  mathDataPayload.resolvedWork.units[officialMath.id].html.replace('<mjx-math ', '<mjx-math data-official-layout="compact" ');
const mathDataFinal = await finalizeMarkdownBundle(mathDataPayload);
assert.match(mathDataFinal.bundle.pages[0].fragment, /<mjx-math data-official-layout="compact"/);

// Pinned MathJax emits mjx-mid for the center piece of a tall stretchy brace.
const stretchyBracePayload = structuredClone(officialCHTMLPayload);
stretchyBracePayload.resolvedWork.units[officialMath.id].html =
  stretchyBracePayload.resolvedWork.units[officialMath.id].html.replace(
    '<mjx-mtable',
    '<mjx-mo><mjx-stretchy-v class="mjx-c7B"><mjx-beg>⎧</mjx-beg><mjx-ext class="NCM-EM"><mjx-spacer>{</mjx-spacer></mjx-ext><mjx-mid>⎨</mjx-mid><mjx-ext class="NCM-EM"><mjx-spacer>{</mjx-spacer></mjx-ext><mjx-end>⎩</mjx-end></mjx-stretchy-v></mjx-mo><mjx-mtable',
  );
const stretchyBraceFinal = await finalizeMarkdownBundle(stretchyBracePayload);
assert.equal(stretchyBraceFinal.publishable, true);
assert.match(stretchyBraceFinal.bundle.pages[0].fragment, /<mjx-mid>⎨<\/mjx-mid>/);
const unknownMathTagPayload = structuredClone(stretchyBracePayload);
unknownMathTagPayload.resolvedWork.units[officialMath.id].html =
  unknownMathTagPayload.resolvedWork.units[officialMath.id].html
    .replace('<mjx-mid>', '<mjx-unapproved>')
    .replace('</mjx-mid>', '</mjx-unapproved>');
await assert.rejects(
  () => finalizeMarkdownBundle(unknownMathTagPayload),
  (error) => error?.code === 'markdown.finalize.element_disallowed',
  'unknown MathJax elements must remain blocked',
);

const repeated = await finalizeMarkdownBundle({ bundle: compiled.bundle, resolvedWork }, {
  adapterVersion: '0.6.0', engineVersion: 'pipeline-final-test',
});
assert.deepEqual(repeated, finalized, 'finalization was not deterministic for one admitted draft/resolution');

await rejectMutation('missing work', (payload) => {
  delete payload.resolvedWork.units[payload.bundle.workUnits[0].id];
}, 'markdown.finalize.resolved.missing');
await rejectMutation('extra work', (payload) => {
  payload.resolvedWork.units.rw_ffffffffffffffffffffffffffffffff = {
    id: 'rw_ffffffffffffffffffffffffffffffff', kind: 'code', html: '<pre><code>x</code></pre>', css: [], diagnostics: [],
  };
}, 'markdown.finalize.resolved.invalid');
await rejectMutation('kind mismatch', (payload) => {
  payload.resolvedWork.units[payload.bundle.workUnits[0].id].kind = 'code';
}, 'markdown.finalize.resolved.invalid');
await rejectMutation('script worker output', replaceCodeHTML('<script>alert(1)</script>'), 'markdown.finalize.element_disallowed');
await rejectMutation('event worker output', replaceCodeHTML('<pre class="rin-code-pre" onclick="alert(1)"><code>x</code></pre>'), 'markdown.finalize.event_handler');
await rejectMutation('unsafe worker URL', replaceCodeHTML('<pre class="rin-code-pre"><code><a href="javascript:alert(1)">x</a></code></pre>'), 'markdown.finalize.url_disallowed');
await rejectMutation('unsafe Shiki style', replaceCodeHTML('<pre class="shiki github-light" style="background-image:url(javascript:alert(1))"><code>x</code></pre>'), 'markdown.finalize.style_disallowed');
await rejectMutation('inline SVG', replaceCodeHTML('<pre class="rin-code-pre"><code><svg><script>x</script></svg></code></pre>'), 'markdown.finalize.element_disallowed');
await rejectMutation('unknown property', replaceCodeHTML('<pre class="rin-code-pre" data-made-up="x"><code>x</code></pre>'), 'markdown.finalize.sanitizer_changed_output');
await rejectMutation('malformed worker HTML', replaceCodeHTML('<pre class="rin-code-pre"><code>x</pre>'), 'markdown.finalize.resolved.malformed');
await rejectMutation('duplicate worker IDs', replaceCodeHTML('<pre class="rin-code-pre"><code><span id="x"></span><span id="x"></span></code></pre>'), 'markdown.finalize.dom_clobbering');
await rejectMutation('post-prefix worker ID collision', replaceCodeHTML('<pre class="rin-code-pre"><code><span id="x"></span><span id="rin-md-x"></span></code></pre>'), 'markdown.finalize.dom_clobbering');
await rejectMutation('deep worker tree', replaceCodeHTML(`<pre class="rin-code-pre"><code>${'<span>'.repeat(300)}x${'</span>'.repeat(300)}</code></pre>`), 'markdown.finalize.tree_too_large');
await rejectMutation('oversized worker output', (payload) => {
  const code = payload.bundle.workUnits.find((unit) => unit.kind === 'code');
  payload.resolvedWork.units[code.id].html = `<pre class="rin-code-pre"><code>${'x'.repeat(4 * 1024 * 1024)}</code></pre>`;
}, 'markdown.finalize.resolved.too_large');
await rejectMutation('non-math CSS', (payload) => {
  const code = payload.bundle.workUnits.find((unit) => unit.kind === 'code');
  payload.resolvedWork.units[code.id].css = ['body{display:none}'];
}, 'markdown.finalize.css.kind_invalid');
await rejectMutation('escaped math CSS namespace', (payload) => {
  const math = payload.bundle.workUnits.find((unit) => unit.kind === 'math');
  payload.resolvedWork.units[math.id].css = ['body{display:none}'];
}, 'markdown.finalize.css_disallowed');
await rejectMutation('escaped NewCM selector', (payload) => {
  const math = payload.bundle.workUnits.find((unit) => unit.kind === 'math');
  payload.resolvedWork.units[math.id].css = ['.NCM-N body{display:none}'];
}, 'markdown.finalize.css_disallowed');
await rejectMutation('unapproved MathJax media rule', (payload) => {
  const math = payload.bundle.workUnits.find((unit) => unit.kind === 'math');
  payload.resolvedWork.units[math.id].css = ['@media print{mjx-container{display:none}}'];
}, 'markdown.finalize.css_disallowed');
await rejectMutation('artifact mismatch', (payload) => {
  const diagram = payload.bundle.workUnits.find((unit) => unit.kind === 'diagram');
  payload.resolvedWork.units[diagram.id].artifact.artifactId = 'diagrams/v1/svg-sha256/ff/bad.svg';
}, 'markdown.finalize.artifact.invalid');

const reservedSource = await compileMarkdownDraft({
  source: '# Safe\n\n<rin-work data-id="rw_00000000000000000000000000000000"></rin-work>',
  sourcePath: 'reserved.md', projectHash,
}, { adapterVersion: '0.6.0', engineVersion: 'pipeline-test' });
const reservedFinal = await finalizeMarkdownBundle({ bundle: reservedSource.bundle, resolvedWork: { units: {} } });
assert.ok(!reservedFinal.bundle.pages[0].fragment.includes('<rin-work'));
assert.match(reservedFinal.bundle.pages[0].fragment, /(?:&lt;|&#x3C;)rin-work/);

const workerFinal = runWorker({
  contractVersion: 'rin-node-worker/v1', id: 'finalize-operation',
  operation: 'markdown.finalize', payload: { bundle: compiled.bundle, resolvedWork },
});
assert.equal(workerFinal.ok, true);
assert.equal(workerFinal.id, 'finalize-operation');
assert.equal(workerFinal.result?.publishable, true);
assert.equal(workerFinal.result?.bundle?.state, 'final');
assert.equal(workerFinal.result?.bundle?.workUnits?.length, 0);
assert.equal(workerFinal.result?.sanitizerVersion, MARKDOWN_SANITIZER_VERSION);
assert.equal(workerFinal.result?.capabilities?.policy?.publishableFinalOutput, true);
assert.equal(workerFinal.metrics?.tasks, 1);

console.log(`Markdown finalizer ok: ${compiled.bundle.workUnits.length} typed insertions, final sanitizer and attack corpus`);

function resolvedFor(bundle) {
  const units = {};
  for (const unit of bundle.workUnits) {
    if (unit.kind === 'math') {
      units[unit.id] = {
        id: unit.id, kind: unit.kind,
        html: `<span class="rin-math rin-math-inline rin-math-mathjax" data-rin-math-engine="mathjax-chtml" data-rin-math-source="${escapeAttribute(unit.source)}" data-rin-math-version="4.1.3"><mjx-container class="MathJax" jax="CHTML" data-latex="${escapeAttribute(unit.source)}"><mjx-math><mjx-mi><mjx-c class="mjx-c2204"></mjx-c></mjx-mi></mjx-math></mjx-container></span>`,
        css: ['mjx-container[jax="CHTML"]{display:inline-block}\nmjx-c.mjx-c2204{padding:0.5em}'], diagnostics: [],
      };
    } else if (unit.kind === 'code') {
      units[unit.id] = {
        id: unit.id, kind: unit.kind,
        html: '<pre class="shiki github-light" style="background-color:#fff;color:#24292e" tabindex="0"><code><span class="line"><span style="color:#D73A49">const</span> value</span></code></pre>',
        css: [], diagnostics: [],
      };
    } else {
      const hash = 'c'.repeat(64);
      const artifactId = `diagrams/v1/svg-sha256/cc/${hash}.svg`;
      units[unit.id] = {
        id: unit.id, kind: unit.kind,
        html: `<div class="rin-align-block rin-align-center" data-rin-align="center"><figure class="rin-reader-diagram rin-reader-diagram-tikzpicture" data-diagram-id="diagram-1"><img src="https://assets.example/${artifactId}" alt="tikzpicture diagram" loading="lazy" decoding="async" data-rin-diagram-object-id="${artifactId}"></figure></div>`,
        css: [], diagnostics: [],
        artifact: {
          artifactId, sha256: hash, bytes: 1234,
          mediaType: 'image/svg+xml; charset=utf-8', visibility: 'public',
        },
      };
    }
  }
  return { units };
}

function replaceCodeHTML(html) {
  return (payload) => {
    const code = payload.bundle.workUnits.find((unit) => unit.kind === 'code');
    payload.resolvedWork.units[code.id].html = html;
  };
}

async function rejectMutation(name, mutate, code) {
  const payload = structuredClone({ bundle: compiled.bundle, resolvedWork });
  mutate(payload);
  await assert.rejects(
    () => finalizeMarkdownBundle(payload),
    (error) => error?.code === code,
    name,
  );
}

function escapeAttribute(value) {
  return String(value).replaceAll('&', '&amp;').replaceAll('"', '&quot;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');
}

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
