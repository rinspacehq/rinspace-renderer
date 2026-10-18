import assert from 'node:assert/strict';

import {
  APPROVED_SHIKI_LANGUAGES,
  APPROVED_SHIKI_THEME,
  SHIKI_BATCH_CONTRACT,
  SHIKI_RESULT_CONTRACT,
  assertShikiReady,
  resolveShikiBatch,
} from './shiki.mjs';

await assertShikiReady();

// A slow machine must not publish a silently partially highlighted line.
const deterministicInput = { contractVersion: SHIKI_BATCH_CONTRACT, theme: APPROVED_SHIKI_THEME,
  items: [{ id: 'rw_10000000000000000000000000000000', source: 'const vector = [1, 2, 3]', language: 'js' }] };
const realClock = Date.now;
const normal = await resolveShikiBatch(deterministicInput);
try {
  let elapsed = 0;
  Date.now = () => (elapsed += 1000);
  const throttled = await resolveShikiBatch(deterministicInput);
  assert.equal(throttled.items[0].html, normal.items[0].html);
} finally { Date.now = realClock; }
const longLine = '<'.repeat(5000);
const bounded = await resolveShikiBatch({ ...deterministicInput, items: [{ ...deterministicInput.items[0], source: longLine }] });
assert.ok(!bounded.items[0].html.includes('<'.repeat(5)));
assert.equal((bounded.items[0].html.match(/&lt;|&#x3c;|&#60;/gi) || []).length, 5000);
assert.equal(APPROVED_SHIKI_LANGUAGES.length, 30);
assert.ok(APPROVED_SHIKI_LANGUAGES.includes('yaml'));

const source = '<script>alert("x")</script>\nconst answer = 42;';
const result = await resolveShikiBatch({
  contractVersion: SHIKI_BATCH_CONTRACT,
  theme: APPROVED_SHIKI_THEME,
  items: [
    { id: 'rw_00000000000000000000000000000001', source, language: 'js' },
    { id: 'rw_00000000000000000000000000000002', source, language: 'made-up\" onclick=\"x' },
    { id: 'rw_00000000000000000000000000000003', source: '', language: '' },
  ],
});
assert.equal(result.contractVersion, SHIKI_RESULT_CONTRACT);
assert.equal(result.engine, 'shiki');
assert.equal(result.engineVersion, '4.4.2');
assert.equal(result.theme, APPROVED_SHIKI_THEME);
assert.equal(result.items[0].canonicalLanguage, 'javascript');
assert.equal(result.items[0].highlighted, true);
assert.match(result.items[0].html, /class="shiki github-light"/);
assert.ok(!result.items[0].html.includes('<script>'));
assert.equal(result.items[1].highlighted, false);
assert.match(result.items[1].html, /data-rin-code-language="made-up&quot; onclick=&quot;x"/);
assert.ok(!result.items[1].html.includes('<script>'));
assert.equal(result.items[1].diagnostics[0].code, 'code.language.unknown');
assert.equal(result.items[2].diagnostics.length, 0);

await assert.rejects(() => resolveShikiBatch({
  contractVersion: SHIKI_BATCH_CONTRACT,
  theme: APPROVED_SHIKI_THEME,
  items: [
    { id: 'rw_00000000000000000000000000000001', source: 'a', language: 'js' },
    { id: 'rw_00000000000000000000000000000001', source: 'b', language: 'js' },
  ],
}), /invalid or duplicate id/);

console.log(`Shiki batch ok: ${APPROVED_SHIKI_LANGUAGES.length} pinned languages, safe unknown fallback`);
