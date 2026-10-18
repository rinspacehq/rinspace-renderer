import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';

import { compileMarkdownDraft } from './compiler.mjs';
import { finalizeMarkdownBundle } from './finalizer.mjs';

// Exercise the real pinned producer AND the consumer, not hand-written CHTML
// or a tag-only regex. Inline and display forced breaks use different layout
// attributes; grouping, delimiters and extra leading must survive unchanged.
const formulas = [
  String.raw`a\\b`,
  String.raw`{a\\b}`,
  String.raw`\left(a\\b\right)`,
  String.raw`a\newline b`,
  String.raw`a\\[1em]b`,
];
const requests = formulas.flatMap((source) => [false, true].map((displayMode) => ({ source, displayMode })));
const rendered = JSON.parse(execFileSync(process.execPath, ['render-mathjax.mjs'], {
  cwd: new URL('../mathjax/', import.meta.url),
  input: JSON.stringify({ requests }), encoding: 'utf8', maxBuffer: 10 * 1024 * 1024,
}));
assert.equal(rendered.ok, true);
assert.equal(rendered.version, '4.1.3');
assert.equal(rendered.results.length, requests.length);

for (const [index, request] of requests.entries()) {
  const output = rendered.results[index];
  assert.equal(output.ok, true, output.message);
  for (const tag of ['mjx-linestack', 'mjx-linebox', 'mjx-linestrut']) {
    assert.ok(output.html.includes(`<${tag}`), `Pinned producer did not cover ${tag}`);
  }
  assert.match(output.html, /lineno="1"/);
  assert.match(output.html, request.displayMode ? /align="center"/ : /align="left"/);
  const draft = await compileMarkdownDraft({
    source: request.displayMode ? `$$\n${request.source}\n$$` : `$${request.source}$`,
    sourcePath: 'main.md', projectHash: 'a'.repeat(64),
  });
  assert.equal(draft.bundle.workUnits.length, 1);
  const unit = draft.bundle.workUnits[0];
  assert.equal(unit.kind, 'math');
  assert.equal(unit.source, request.source);
  const payload = {
    bundle: draft.bundle,
    resolvedWork: { units: { [unit.id]: {
      id: unit.id, kind: 'math', html: output.html, css: [rendered.css], diagnostics: [],
    } } },
  };
  const final = await finalizeMarkdownBundle(payload);
  assert.equal(final.publishable, true);
  for (const tag of ['mjx-linestack', 'mjx-linebox', 'mjx-linestrut']) {
    assert.ok(final.bundle.pages[0].fragment.includes(`<${tag}`));
  }
  assert.match(final.bundle.pages[0].fragment, /lineno="1"/);
  assert.match(final.bundle.pages[0].fragment, request.displayMode ? /align="center"/ : /align="left"/);

  // New static layout nodes are not a wildcard for custom elements, events,
  // navigation, executable styles, or unreviewed metadata on ordinary HTML.
  for (const [name, mutate, code] of [
    ['unknown tag', (html) => html.replaceAll('mjx-linestrut', 'mjx-unreviewed'), 'markdown.finalize.element_disallowed'],
    ['event', (html) => html.replace('<mjx-linebox ', '<mjx-linebox onclick="alert(1)" '), 'markdown.finalize.event_handler'],
    ['URL', (html) => html.replace('<mjx-linebox ', '<mjx-linebox href="javascript:alert(1)" '), 'markdown.finalize.url_disallowed'],
    ['style', (html) => html.replace('<mjx-linestrut>', '<mjx-linestrut style="background-image:url(javascript:alert(1))">'), 'markdown.finalize.style_disallowed'],
    ['ordinary HTML layout attributes', (html) => html.replace('</mjx-container>', '<span lineno="1">x</span></mjx-container>'), 'markdown.finalize.sanitizer_changed_output'],
  ]) {
    const attack = structuredClone(payload);
    attack.resolvedWork.units[unit.id].html = mutate(output.html);
    await assert.rejects(() => finalizeMarkdownBundle(attack), (error) => error.code === code, name);
  }
}
console.log(`Real MathJax → Markdown finalizer: ${requests.length} multiline cases and 50 security cases passed`);
