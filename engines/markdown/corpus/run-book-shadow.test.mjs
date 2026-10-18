import assert from 'node:assert/strict';

import { bookArchive, readBookShadowManifest, navigationTargetVariants, requiresShikiHighlighting } from './run-book-shadow.mjs';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';

assert.equal(requiresShikiHighlighting('```text\nplain\n```'), false);
assert.equal(requiresShikiHighlighting('```plaintext\nplain\n```'), false);
assert.equal(requiresShikiHighlighting('```js\nconst value = 1;\n```'), true);
assert.deepEqual(navigationTargetVariants('./01-calculus.md'), ['./01-calculus.md', '01-calculus.md']);
assert.deepEqual(navigationTargetVariants('../chapter.md#part'), ['../chapter.md#part']);

const { books } = await readBookShadowManifest(fileURLToPath(new URL('../fixtures/book-shadow-synthetic.json', import.meta.url)));
for (const book of books) {
  const archive = bookArchive(book);
  assert.ok(archive.includes(Buffer.from('main.md')));
  assert.ok(archive.includes(Buffer.from(book.pages.map(page => `- ${page.path}\n`).join(''))));
  for (const page of book.pages) assert.equal(page.id, `rin-md-${createHash('sha256').update(page.path).digest('hex').slice(0,24)}`);
}

console.log('Markdown Book shadow invariants ok');
