import assert from 'node:assert/strict';

import { navigationTargetVariants, requiresShikiHighlighting } from './run-book-shadow.mjs';

assert.equal(requiresShikiHighlighting('```text\nplain\n```'), false);
assert.equal(requiresShikiHighlighting('```plaintext\nplain\n```'), false);
assert.equal(requiresShikiHighlighting('```js\nconst value = 1;\n```'), true);
assert.deepEqual(navigationTargetVariants('./01-calculus.md'), ['./01-calculus.md', '01-calculus.md']);
assert.deepEqual(navigationTargetVariants('../chapter.md#part'), ['../chapter.md#part']);

console.log('Markdown Book shadow invariants ok');
