import assert from 'node:assert/strict';
import { compileMarkdownBookDraft } from './compiler.mjs';
import { finalizeMarkdownBundle, planMarkdownBookCache } from './finalizer.mjs';

const page = { pageId: 'rin-md-catalog-chapter', sourcePath: 'chapter.md', title: 'Chapter', source: 'A chapter without any headings.', level: 1 };
const input = { projectHash: 'a'.repeat(64), title: 'Book', pages: [page] };
const draft = await compileMarkdownBookDraft(input);
const final = await finalizeMarkdownBundle({ bundle: draft.bundle, resolvedWork: { units: {} } });
assert.equal(final.bundle.pages.length, 1);
assert.deepEqual(final.bundle.pages[0].toc, [{ id: page.pageId, depth: 1, text: 'Chapter' }]);
assert.match(final.bundle.pages[0].fragment, /id="rin-md-catalog-chapter"/);

const book = { ...input, pages: [page, { ...page, pageId: 'rin-md-child', sourcePath: 'child.md', title: 'Child', level: 2 }] };
const nested = await compileMarkdownBookDraft(book);
const nestedFinal = await finalizeMarkdownBundle({ bundle: nested.bundle, resolvedWork: { units: {} } });
assert.equal(nestedFinal.bundle.pages[1].toc[0].depth, 2);
const flat = await compileMarkdownBookDraft({ ...book, pages: book.pages.map((item) => ({ ...item, level: 1 })) });
assert.notEqual(planMarkdownBookCache({ bundle: nested.bundle, resolvedWork: { units: {} } }).pages[1].pageSemanticHash,
  planMarkdownBookCache({ bundle: flat.bundle, resolvedWork: { units: {} } }).pages[1].pageSemanticHash);
console.log('Markdown catalog hierarchy, one-chapter and cache tests passed');
