import { createHash } from 'node:crypto';

import rehypeParse from 'rehype-parse';
import rehypeSanitize from 'rehype-sanitize';
import rehypeStringify from 'rehype-stringify';
import { unified } from 'unified';
import { VFile } from 'vfile';

import { validPageID, validateDraftBundle } from './compiler.mjs';

export const MARKDOWN_SANITIZER_VERSION = 'rin-markdown-final-sanitizer/v2';
export const MARKDOWN_BOOK_CACHE_SCHEMA_VERSION = 'rin-markdown-book-page-cache/v2';
const finalFragmentFormat = 'html';
const workIDPattern = /^rw_[a-f0-9]{32}$/;
const blockIDPattern = /^rb_[a-f0-9]{32}$/;
const blockKinds = new Set(['heading', 'paragraph', 'list-item', 'theorem', 'math', 'code', 'figure', 'table', 'quote']);
const identifierPattern = /^[A-Za-z][A-Za-z0-9._:-]{0,127}$/;
const sha256Pattern = /^[a-f0-9]{64}$/;
const maximumDiagnostics = 256;
const maximumFragmentBytes = 8 * 1024 * 1024;
const maximumResolvedHTMLBytes = 4 * 1024 * 1024;
const maximumCSSBytes = 2 * 1024 * 1024;
const maximumNodes = 100_000;
const maximumDepth = 256;

const standardTags = Object.freeze([
  'a', 'article', 'aside', 'blockquote', 'br', 'code', 'del', 'div', 'em', 'figcaption', 'figure',
  'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'hr', 'img', 'input', 'li', 'ol', 'p',
  'pre', 'section', 'span', 'strong', 'style', 'sub', 'sup', 'table', 'tbody',
  'td', 'th', 'thead', 'tr', 'ul',
]);

// MathJax CHTML is a versioned custom-element surface, not arbitrary custom HTML. Keep this list
// explicit so a MathJax upgrade must cross the 5.6 corpus/security gate before new tags publish.
const mathJaxTags = Object.freeze([
  'mjx-container', 'mjx-math', 'mjx-mrow', 'mjx-mi', 'mjx-mn', 'mjx-mo', 'mjx-mtext',
  'mjx-ms', 'mjx-mspace', 'mjx-mglyph', 'mjx-mfrac', 'mjx-frac', 'mjx-num', 'mjx-den',
  'mjx-msqrt', 'mjx-mroot', 'mjx-root', 'mjx-surd', 'mjx-mstyle', 'mjx-merror',
  'mjx-mpadded', 'mjx-mphantom', 'mjx-mfenced', 'mjx-menclose', 'mjx-munderover',
  'mjx-over', 'mjx-under', 'mjx-msub', 'mjx-msup', 'mjx-msubsup', 'mjx-script',
  'mjx-mmultiscripts', 'mjx-prescripts', 'mjx-none', 'mjx-mtable', 'mjx-table',
  'mjx-itable', 'mjx-mtr', 'mjx-mtd', 'mjx-labels', 'mjx-label', 'mjx-maction',
  'mjx-semantics', 'mjx-annotation', 'mjx-xml', 'mjx-te', 'mjx-c', 'mjx-char',
  'mjx-utext', 'mjx-box', 'mjx-block', 'mjx-line', 'mjx-break', 'mjx-newline',
  'mjx-stretchy-h', 'mjx-stretchy-v', 'mjx-ext', 'mjx-base', 'mjx-row', 'mjx-nstrut',
  'mjx-dstrut', 'mjx-tstrut', 'mjx-dbox', 'mjx-dtable', 'mjx-cbox', 'mjx-munder',
  'mjx-mover', 'mjx-texatom', 'mjx-beg', 'mjx-mid', 'mjx-end', 'mjx-mark', 'mjx-spacer', 'mjx-sqrt',
  'mjx-rbox',
]);
const allowedTags = new Set([...standardTags, ...mathJaxTags]);

const exactClasses = new Set([
  'MathJax', 'contains-task-list', 'task-list-item', 'footnotes', 'data-footnote-backref',
  'sr-only', 'shiki', 'github-light', 'line',
  'rin-admonition', 'rin-admonition-title', 'rin-markdown-draft', 'rin-markdown-figure',
  'rin-markdown-image', 'rin-markdown-caption', 'rin-quiver', 'rin-quiver-image',
  'rin-quiver-image-figure', 'rin-math', 'rin-math-inline', 'rin-math-display',
  'rin-math-mathjax', 'rin-math-katex', 'rin-math-svg', 'rin-math-fallback',
  'rin-math-source-fallback', 'rin-math-svg-image', 'rin-math-tall-inline',
  'rin-reader-diagram', 'rin-diagram-fallback', 'rin-align-block', 'rin-code-pre',
  'rin-code-plain', 'rin-mathjax-chtml-style', 'rin-book-navigation',
  'rin-book-navigation-previous', 'rin-book-navigation-next',
]);
const classPrefixes = ['language-', 'mjx-', 'TEX-', 'MJX-', 'NCM-', 'MathJax', 'rin-admonition-', 'rin-reader-diagram-', 'rin-align-'];
const forbiddenURLPattern = /^(?:javascript|vbscript|data|file):/i;
const localPathPattern = /^(?:[A-Za-z]:[\\/]|\/(?:tmp|var\/tmp|home|root|proc|sys|dev|workspace)\/)/i;
const forbiddenCSSPattern = /(?:expression\s*\(|@import\b|javascript\s*:|vbscript\s*:|-moz-binding|behavior\s*:)/i;

const globalAttributes = [
  'className', 'id', 'title', 'lang', 'dir', 'role', 'ariaLabel', 'ariaHidden',
  'ariaDescribedBy', 'ariaLabelledBy', 'dataRinBlockId', 'dataRinBlockKind',
];
const mathJaxAttributes = [
  ...globalAttributes, 'style', 'jax', 'display', 'dataLatex', 'width', 'height', 'depth',
  'space', 'size', 'rspace', 'lspace', 'symmetric', 'minsize', 'maxsize', 'scriptlevel',
  'displaystyle', 'stretchy', 'accent', 'accentunder', 'movablelimits', 'largeop', 'form',
  'fence', 'separator', 'rowalign', 'columnalign', 'rowspacing', 'columnspacing',
  'columnwidth', 'rowlines', 'columnlines', 'frame', 'framespacing', 'equalrows',
  'equalcolumns', 'side', 'minlabelspacing', 'notation', 'mathvariant', 'mathsize',
  'mathcolor', 'mathbackground', 'voffset', 'overflow', 'type', 'texclass', 'variant',
  'noic', 'justify', 'limits', 'extra', 'dataFrameStyles', 'data*',
];

const attributes = {
  '*': globalAttributes,
  a: [...globalAttributes, 'href', 'dataFootnoteRef', 'dataFootnoteBackref'],
  aside: [...globalAttributes, 'dataRinAdmonition'],
  blockquote: [...globalAttributes, 'cite'],
  code: [...globalAttributes, 'dataLanguage'],
  div: [...globalAttributes, 'dataRinAlign'],
  figure: [...globalAttributes, 'dataDiagramId'],
  img: [
    ...globalAttributes, 'src', 'alt', 'width', 'height', 'loading', 'decoding',
    'dataRinProjectPath', 'dataRinAssetPath', 'dataRinDiagramObjectId', 'dataRinMathObjectId',
  ],
  input: [...globalAttributes, 'type', 'checked', 'disabled'],
  ol: [...globalAttributes, 'start'],
  pre: [...globalAttributes, 'style', 'tabIndex', 'dataRinCodeLanguage'],
  section: [...globalAttributes, 'dataFootnotes'],
  span: [
    ...globalAttributes, 'style', 'dataRinMathEngine', 'dataRinMathSource',
    'dataRinMathVersion', 'dataRinMathSpeechStyle', 'dataRinMathSvgHash', 'dataRinMathObjectId',
  ],
  style: ['className', 'dataRinMathEngine'],
  td: [...globalAttributes, ['align', 'left', 'right', 'center']],
  th: [...globalAttributes, ['align', 'left', 'right', 'center']],
};
for (const heading of ['h1', 'h2', 'h3', 'h4', 'h5', 'h6']) attributes[heading] = globalAttributes;
for (const tag of mathJaxTags) attributes[tag] = mathJaxAttributes;
attributes.div = [
  ...attributes.div, 'dataRinMathEngine', 'dataRinMathSource', 'dataRinMathVersion',
  'dataRinMathSpeechStyle', 'dataRinMathSvgHash', 'dataRinMathObjectId',
];

export const rinMarkdownSanitizeSchema = Object.freeze({
  allowComments: false,
  allowDoctypes: false,
  ancestors: {},
  attributes,
  clobber: [],
  clobberPrefix: '',
  protocols: {
    cite: ['http', 'https'], href: ['http', 'https', 'mailto'], src: ['http', 'https'],
  },
  strip: ['script'],
  tagNames: [...standardTags, ...mathJaxTags],
});

export async function finalizeMarkdownBundle(payload, options = {}) {
  const draft = structuredClone(payload?.bundle);
  validateDraftBundle(draft);
  const resolved = normalizeResolvedWork(payload?.resolvedWork, draft.workUnits);
  const cachePlan = draft.pages.length > 1 ? buildBookCachePlan(draft, resolved) : null;
  const cachedPages = cachePlan ? normalizeCachedPages(payload?.pageCache, cachePlan, draft, resolved) : new Map();
  const unitByID = new Map(draft.workUnits.map((unit) => [unit.id, unit]));
  const placeholderCounts = new Map();
  const pageIDMaps = new Map(draft.pages.map((page) => [page.sourcePath,
    new Map(page.toc.map((entry) => [entry.id, entry.id.startsWith('rin-md-') ? entry.id : `rin-md-${entry.id}`]))]));
  const finalPages = [];
  for (const page of draft.pages) {
    const planned = cachePlan?.pages.find((item) => item.id === page.id);
    const cachedPage = cachedPages.get(page.id);
    if (cachedPage) {
      for (const id of planned.workIDs) placeholderCounts.set(id, (placeholderCounts.get(id) || 0) + 1);
      finalPages.push(cachedPage);
      continue;
    }
    byteLimit(page.fragment, maximumFragmentBytes, 'markdown.finalize.fragment_too_large', 'Draft fragment exceeds byte limit.');
    const tree = parseFragment(page.fragment);
    enforceTreeLimits(tree);
    const pagePlaceholderCounts = new Map();
    replaceWorkPlaceholders(tree, unitByID, resolved, pagePlaceholderCounts);
    for (const [id, count] of pagePlaceholderCounts) {
      placeholderCounts.set(id, (placeholderCounts.get(id) || 0) + count);
    }

    const idMap = rewriteDOMIdentifiers(tree);
    rewriteCrossPageIdentifiers(tree, pageIDMaps);
    const toc = page.toc.map((entry) => {
      const id = idMap.get(entry.id);
      if (!id) throw finalizerError('markdown.finalize.toc_mismatch', 'TOC entry does not match a final heading.');
      return { ...entry, id };
    });
	const blocks = page.blocks.map((block) => ({
	  ...block,
	  headingPath: block.headingPath.map((id) => {
	    const finalID = idMap.get(id);
	    if (!finalID) throw finalizerError('markdown.finalize.block_heading_mismatch', 'Semantic block heading path does not match a final heading.');
	    return finalID;
	  }),
	}));
    normalizeProjectAssetMarkers(tree, draft.assets);
    const pageResolved = new Map([...pagePlaceholderCounts.keys()].map((id) => [id, resolved.get(id)]));
    const css = collectMathCSS(pageResolved, unitByID);
    if (css) tree.children.unshift(mathStyleNode(css));

    enforceTreeLimits(tree);
    validateCandidateTree(tree, draft.assets, pageResolved);
    const candidateHTML = stringifyTree(tree);
    const safeTree = await unified().use(rehypeSanitize, rinMarkdownSanitizeSchema).run(tree);
    const html = stringifyTree(safeTree);
    if (html !== candidateHTML) {
      const difference = firstSanitizerDifference(tree, safeTree);
      throw finalizerError(
        'markdown.finalize.sanitizer_changed_output',
        `Final sanitizer rejected an unapproved output node or property${difference ? ` (${difference})` : ''}.`,
      );
    }
    byteLimit(html, maximumFragmentBytes, 'markdown.finalize.output_too_large', 'Final fragment exceeds byte limit.');
    validateFinalHTML(html, draft.assets, pageResolved);
	const orderedBlocks = orderSemanticBlocksByFragment(html, blocks);
    finalPages.push({ ...page, fragment: html, fragmentFormat: finalFragmentFormat, toc, blocks: orderedBlocks });
  }
  for (const id of unitByID.keys()) {
    if (placeholderCounts.get(id) !== 1) {
      throw finalizerError('markdown.finalize.placeholder_mismatch', 'Each draft work unit must be inserted exactly once.');
    }
  }

  const diagnostics = mergeDiagnostics(draft.diagnostics, resolved, draft.workUnits);
  const finalBundle = {
    ...draft,
    state: 'final',
    bundleHash: '',
    pages: finalPages,
    workUnits: [],
    diagnostics,
    provenance: {
      ...draft.provenance,
      adapterVersion: options.adapterVersion || draft.provenance.adapterVersion,
      engineVersion: options.engineVersion || draft.provenance.engineVersion,
    },
  };
  finalBundle.bundleHash = sha256(JSON.stringify(canonicalize(finalBundle)));
  validateFinalBundle(finalBundle);
  const pageCacheRecords = cachePlan ? finalPages.map((page) => ({
    schemaVersion: MARKDOWN_BOOK_CACHE_SCHEMA_VERSION,
    pageSemanticHash: cachePlan.pages.find((item) => item.id === page.id).pageSemanticHash,
    pageHash: hashCanonical(page),
    page,
  })) : [];
  const projectDependencyHash = cachePlan ? hashCanonical({
    schemaVersion: MARKDOWN_BOOK_CACHE_SCHEMA_VERSION,
    globalMetadataHash: cachePlan.globalMetadataHash,
    pages: pageCacheRecords.map((record) => ({ id: record.page.id, pageHash: record.pageHash })),
  }) : '';
  return {
    publishable: true,
    bundle: finalBundle,
    artifacts: collectArtifacts(resolved),
    sanitizerVersion: MARKDOWN_SANITIZER_VERSION,
    ...(draft.pages.length > 1 ? {
      pageCache: {
        schemaVersion: MARKDOWN_BOOK_CACHE_SCHEMA_VERSION,
        globalMetadataHash: cachePlan.globalMetadataHash,
        projectDependencyHash,
        reusedPageIds: [...cachedPages.keys()],
        records: pageCacheRecords,
      },
    } : {}),
  };
}

export function planMarkdownBookCache(payload) {
  const draft = structuredClone(payload?.bundle);
  validateDraftBundle(draft);
  if (draft.pages.length < 2) throw finalizerError('markdown.book_cache.not_book', 'Book cache planning requires multiple pages.');
  const resolved = normalizeResolvedWork(payload?.resolvedWork, draft.workUnits);
  const plan = buildBookCachePlan(draft, resolved);
  return {
    schemaVersion: MARKDOWN_BOOK_CACHE_SCHEMA_VERSION,
    globalMetadataHash: plan.globalMetadataHash,
    pages: plan.pages.map(({ id, sourcePath, pageSemanticHash }) => ({ id, sourcePath, pageSemanticHash })),
  };
}

function buildBookCachePlan(draft, resolved) {
  const unitByID = new Map(draft.workUnits.map((unit) => [unit.id, unit]));
  const pages = draft.pages.map((page) => {
    const workIDs = [...page.fragment.matchAll(/<rin-work data-id="(rw_[a-f0-9]{32})"([^>]*)><\/rin-work>/g)].map((match) => match[1]);
    const slotByID = new Map(workIDs.map((id, index) => [id, `work-${index}`]));
    const normalizedFragment = page.fragment.replace(
	  /<rin-work data-id="(rw_[a-f0-9]{32})"([^>]*)><\/rin-work>/g,
	  (_match, id, attributes) => `<rin-work data-slot="${slotByID.get(id)}"${attributes}></rin-work>`,
    );
    const work = workIDs.map((id) => ({
      slot: slotByID.get(id),
      unit: omitIdentity(unitByID.get(id)),
      resolved: omitIdentity(resolved.get(id)),
    }));
    const pageSemanticHash = hashCanonical({
      schemaVersion: MARKDOWN_BOOK_CACHE_SCHEMA_VERSION,
      page: {
        id: page.id, sourcePath: page.sourcePath, title: page.title || '',
        fragment: normalizedFragment, fragmentFormat: page.fragmentFormat,
		  toc: page.toc, blocks: page.blocks, dependencyHashes: [...page.dependencyHashes].sort(),
      },
      work,
    });
    return { id: page.id, sourcePath: page.sourcePath, pageSemanticHash, workIDs };
  });
  const globalMetadataHash = hashCanonical({
    schemaVersion: MARKDOWN_BOOK_CACHE_SCHEMA_VERSION,
    title: draft.title,
    pages: draft.pages.map((page) => ({
	  id: page.id, sourcePath: page.sourcePath, title: page.title || '', toc: page.toc, blocks: page.blocks,
    })),
  });
  return { globalMetadataHash, pages };
}

function omitIdentity(value) {
  if (!value || typeof value !== 'object') return value;
  const clone = structuredClone(value);
  delete clone.id;
  return clone;
}

function hashCanonical(value) {
  return sha256(JSON.stringify(canonicalize(value)));
}

function normalizeCachedPages(value, plan, draft, resolved) {
  if (value == null) return new Map();
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    return new Map();
  }
  const byID = new Map();
  const draftByID = new Map(draft.pages.map((page) => [page.id, page]));
  const planByID = new Map(plan.pages.map((page) => [page.id, page]));
  for (const [id, record] of Object.entries(value)) {
    const expected = planByID.get(id);
    const draftPage = draftByID.get(id);
    if (!expected || record?.schemaVersion !== MARKDOWN_BOOK_CACHE_SCHEMA_VERSION ||
        record.pageSemanticHash !== expected.pageSemanticHash || !sha256Pattern.test(record.pageHash) ||
        !record.page || record.page.id !== id || record.pageHash !== hashCanonical(record.page)) {
      continue;
    }
    const page = structuredClone(record.page);
    const expectedTOC = draftPage.toc.map((entry) => ({
      ...entry, id: entry.id.startsWith('rin-md-') ? entry.id : `rin-md-${entry.id}`,
    }));
    if (page.sourcePath !== draftPage.sourcePath || (page.title || '') !== (draftPage.title || '') ||
        page.fragmentFormat !== finalFragmentFormat ||
        JSON.stringify(page.toc) !== JSON.stringify(expectedTOC) ||
        JSON.stringify(page.dependencyHashes) !== JSON.stringify(draftPage.dependencyHashes)) {
      continue;
    }
    try {
      byteLimit(page.fragment, maximumFragmentBytes, 'markdown.book_cache.invalid', 'Cached Book page exceeds byte limit.');
      const pageResolved = new Map(expected.workIDs.map((workID) => [workID, resolved.get(workID)]));
      validateFinalHTML(page.fragment, draft.assets, pageResolved);
      byID.set(id, page);
    } catch {
      // A cache candidate is only an optimization. Any malformed, stale, or unsafe record becomes
      // a bounded miss and the page follows the clean finalization path below.
    }
  }
  return byID;
}

function rewriteCrossPageIdentifiers(tree, pageIDMaps) {
  walk(tree, (node) => {
    if (node.type !== 'element' || node.tagName !== 'a') return;
    const href = stringProperty(node.properties?.href);
    if (!href || href.startsWith('#')) return;
    const hashIndex = href.indexOf('#');
    if (hashIndex < 0) return;
    const sourcePath = href.slice(0, hashIndex);
    const id = href.slice(hashIndex + 1);
    const finalID = pageIDMaps.get(sourcePath)?.get(id);
    if (finalID) node.properties.href = `${sourcePath}#${finalID}`;
  });
}

function normalizeResolvedWork(value, workUnits) {
  const units = value?.units;
  if (!units || typeof units !== 'object' || Array.isArray(units)) {
    throw finalizerError('markdown.finalize.resolved.invalid', 'Resolved work map is invalid.');
  }
  const inputByID = new Map(workUnits.map((unit) => [unit.id, unit]));
  const normalized = new Map();
  for (const [id, input] of Object.entries(units)) {
    const expected = inputByID.get(id);
    if (!expected || !workIDPattern.test(id) || input?.id !== id || input.kind !== expected.kind ||
        typeof input.html !== 'string' || !input.html.trim()) {
      throw finalizerError('markdown.finalize.resolved.invalid', 'Resolved work identity, kind, or HTML is invalid.');
    }
    byteLimit(input.html, maximumResolvedHTMLBytes, 'markdown.finalize.resolved.too_large', 'Resolved work HTML exceeds byte limit.');
    const css = input.css == null ? [] : input.css;
    if (!Array.isArray(css) || css.some((item) => typeof item !== 'string' || !item.trim())) {
      throw finalizerError('markdown.finalize.resolved.invalid', 'Resolved work CSS is invalid.');
    }
    const diagnostics = input.diagnostics == null ? [] : input.diagnostics;
    if (!Array.isArray(diagnostics) || diagnostics.length > maximumDiagnostics || diagnostics.some((item) => !validDiagnostic(item))) {
      throw finalizerError('markdown.finalize.resolved.invalid', 'Resolved work diagnostics are invalid.');
    }
    const artifact = input.artifact == null ? undefined : normalizeArtifact(input.artifact, expected.kind);
    normalized.set(id, { id, kind: input.kind, html: input.html, css: [...css], diagnostics: [...diagnostics], artifact });
  }
  if (normalized.size !== inputByID.size || [...inputByID.keys()].some((id) => !normalized.has(id))) {
    throw finalizerError('markdown.finalize.resolved.missing', 'Resolved work must match every draft unit exactly once.');
  }
  return normalized;
}

function normalizeArtifact(value, kind) {
  if (!['math', 'diagram'].includes(kind) || typeof value !== 'object' ||
      !value.artifactId || !sha256Pattern.test(value.sha256) || !Number.isSafeInteger(value.bytes) || value.bytes <= 0 ||
      value.mediaType !== 'image/svg+xml; charset=utf-8' || value.visibility !== 'public' || value.expiresAt) {
    throw finalizerError('markdown.finalize.artifact.invalid', 'Resolved artifact metadata is invalid.');
  }
  const expected = `diagrams/v1/svg-sha256/${value.sha256.slice(0, 2)}/${value.sha256}.svg`;
  if (value.artifactId !== expected) {
    throw finalizerError('markdown.finalize.artifact.invalid', 'Resolved SVG artifact is not content addressed.');
  }
  return { ...value };
}

function parseFragment(html) {
  return unified().use(rehypeParse, { fragment: true }).parse(html);
}

function parseResolvedFragment(html) {
  assertStrictResolvedMarkup(html);
  const file = new VFile({ value: html });
  const tree = unified().use(rehypeParse, { fragment: true, emitParseErrors: true }).parse(file);
  if (file.messages.length) {
    throw finalizerError('markdown.finalize.resolved.malformed', 'Resolved work HTML contains a parse error.');
  }
  return tree;
}

function assertStrictResolvedMarkup(html) {
  const voidTags = new Set(['area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input', 'link', 'meta', 'param', 'source', 'track', 'wbr']);
  const stack = [];
  let cursor = 0;
  while (cursor < html.length) {
    const start = html.indexOf('<', cursor);
    if (start < 0) break;
    let quote = '';
    let end = start + 1;
    for (; end < html.length; end += 1) {
      const character = html[end];
      if (quote) {
        if (character === quote) quote = '';
      } else if (character === '"' || character === "'") {
        quote = character;
      } else if (character === '>') {
        break;
      }
    }
    if (end >= html.length || quote) {
      throw finalizerError('markdown.finalize.resolved.malformed', 'Resolved work HTML contains an unterminated tag.');
    }
    const token = html.slice(start, end + 1);
    const match = /^<\s*(\/?)\s*([A-Za-z][A-Za-z0-9-]*)([\s\S]*?)(\/?)>$/.exec(token);
    if (!match) {
      throw finalizerError('markdown.finalize.resolved.malformed', 'Resolved work HTML contains a non-element markup token.');
    }
    const closing = match[1] === '/';
    const tag = match[2].toLowerCase();
    const trailing = match[3];
    const selfClosing = match[4] === '/';
    if (closing) {
      if (trailing.trim() || selfClosing || stack.pop() !== tag) {
        throw finalizerError('markdown.finalize.resolved.malformed', 'Resolved work HTML tags are not strictly nested.');
      }
    } else if (selfClosing && !voidTags.has(tag)) {
      throw finalizerError('markdown.finalize.resolved.malformed', 'Resolved work HTML self-closes a non-void element.');
    } else if (!voidTags.has(tag)) {
      stack.push(tag);
    }
    cursor = end + 1;
  }
  if (stack.length) {
    throw finalizerError('markdown.finalize.resolved.malformed', 'Resolved work HTML contains an unclosed element.');
  }
}

function replaceWorkPlaceholders(parent, unitByID, resolved, counts) {
  if (!Array.isArray(parent.children)) return;
  for (let index = 0; index < parent.children.length; index += 1) {
    const node = parent.children[index];
    if (node.type === 'element' && node.tagName === 'rin-work') {
      const id = property(node, 'dataId', 'data-id');
      const blockID = property(node, 'dataRinBlockId', 'data-rin-block-id');
	  const blockKind = property(node, 'dataRinBlockKind', 'data-rin-block-kind');
	  const expectedPropertyCount = blockID || blockKind ? 3 : 1;
	  if (!workIDPattern.test(id || '') || !unitByID.has(id) || node.children.length !== 0 ||
		  Object.keys(node.properties || {}).length !== expectedPropertyCount || Boolean(blockID) !== Boolean(blockKind) ||
		  blockID && (!blockIDPattern.test(blockID) || !blockKinds.has(blockKind))) {
        throw finalizerError('markdown.finalize.placeholder_invalid', 'Draft contains an unknown or malformed work placeholder.');
      }
      const resolvedUnit = resolved.get(id);
      if (!resolvedUnit || resolvedUnit.kind !== unitByID.get(id).kind) {
        throw finalizerError('markdown.finalize.resolved.kind_mismatch', 'Resolved work kind does not match its placeholder.');
      }
      const fragment = parseResolvedFragment(resolvedUnit.html);
      enforceTreeLimits(fragment);
      if (fragment.children.length !== 1 || fragment.children[0].type !== 'element') {
        throw finalizerError('markdown.finalize.resolved.root_invalid', 'Resolved work must have exactly one element root.');
      }
	  if (blockID) {
		fragment.children[0].properties = {
		  ...(fragment.children[0].properties || {}),
		  dataRinBlockId: blockID,
		  dataRinBlockKind: blockKind,
		};
	  }
      parent.children.splice(index, 1, fragment.children[0]);
      counts.set(id, (counts.get(id) || 0) + 1);
      continue;
    }
    replaceWorkPlaceholders(node, unitByID, resolved, counts);
  }
}

function rewriteDOMIdentifiers(tree) {
  const idMap = new Map();
  const finalIDs = new Set();
  walk(tree, (node) => {
    if (node.type !== 'element') return;
    const id = stringProperty(node.properties?.id);
    if (!id) return;
    if (!identifierPattern.test(id) || idMap.has(id)) {
      throw finalizerError('markdown.finalize.dom_clobbering', 'Final candidate contains an invalid or duplicate identifier.');
    }
    const finalID = id.startsWith('rin-md-') ? id : `rin-md-${id}`;
    if (finalIDs.has(finalID)) {
      throw finalizerError('markdown.finalize.dom_clobbering', 'Final candidate identifiers collide after namespacing.');
    }
    idMap.set(id, finalID);
    finalIDs.add(finalID);
  });
  walk(tree, (node) => {
    if (node.type !== 'element') return;
    const properties = node.properties || {};
    if (typeof properties.id === 'string') properties.id = idMap.get(properties.id);
    for (const name of ['href']) {
      const value = stringProperty(properties[name]);
      if (value?.startsWith('#') && idMap.has(value.slice(1))) properties[name] = `#${idMap.get(value.slice(1))}`;
    }
    for (const name of ['ariaDescribedBy', 'ariaLabelledBy', 'headers']) {
      if (!(name in properties)) continue;
      const raw = properties[name];
      const values = (Array.isArray(raw) ? raw.map(String) : stringProperty(raw).split(/\s+/)).filter(Boolean);
      if (!values.length) {
        delete properties[name];
        continue;
      }
      properties[name] = values.map((value) => idMap.get(value) || value);
    }
    if ('name' in properties) delete properties.name;
  });
  return idMap;
}

function normalizeProjectAssetMarkers(tree, assets) {
  const paths = new Set(assets.filter((asset) => asset.kind === 'project-file').map((asset) => asset.projectPath));
  walk(tree, (node) => {
    if (node.type !== 'element' || node.tagName !== 'img') return;
    const projectPath = property(node, 'dataRinProjectPath', 'data-rin-project-path');
    if (!projectPath) return;
    if (!paths.has(projectPath)) throw finalizerError('markdown.finalize.asset.unknown', 'Image references an undeclared project asset.');
    delete node.properties.dataRinProjectPath;
    delete node.properties['data-rin-project-path'];
    node.properties.dataRinAssetPath = projectPath;
  });
}

function collectMathCSS(resolved, unitByID) {
  const blocks = [];
  const seen = new Set();
  let bytes = 0;
  for (const [id, unit] of resolved) {
    if (unit.css.length && unitByID.get(id)?.kind !== 'math') {
      throw finalizerError('markdown.finalize.css.kind_invalid', 'Only shared Math work may supply final stylesheet rules.');
    }
    for (const css of unit.css) {
      if (seen.has(css)) continue;
      validateMathJaxStylesheet(css);
      bytes += Buffer.byteLength(css, 'utf8');
      if (bytes > maximumCSSBytes) throw finalizerError('markdown.finalize.css.too_large', 'MathJax CSS exceeds byte limit.');
      seen.add(css);
      blocks.push(css.trim());
    }
  }
  return blocks.join('\n');
}

function mathStyleNode(css) {
  return {
    type: 'element', tagName: 'style',
    properties: { className: ['rin-mathjax-chtml-style'], dataRinMathEngine: 'mathjax-chtml' },
    children: [{ type: 'text', value: css }],
  };
}

function validateCandidateTree(tree, assets, resolved) {
  const assetPaths = new Set(assets.filter((asset) => asset.kind === 'project-file').map((asset) => asset.projectPath));
  const artifactIDs = new Set(collectArtifacts(resolved).map((artifact) => artifact.artifactId));
  const seenArtifactIDs = new Set();
  const seenIDs = new Set();
  walk(tree, (node, ancestors) => {
    if (node.type !== 'element') return;
    if (!allowedTags.has(node.tagName)) throw finalizerError('markdown.finalize.element_disallowed', `Element <${node.tagName}> is not in the final schema.`);
    for (const name of Object.keys(node.properties || {})) {
      if (/^on/i.test(name)) throw finalizerError('markdown.finalize.event_handler', 'Event handler attributes are forbidden.');
    }
    validateClasses(node.properties?.className);
    const id = stringProperty(node.properties?.id);
    if (id) {
      if (!id.startsWith('rin-md-') || seenIDs.has(id)) throw finalizerError('markdown.finalize.dom_clobbering', 'Final identifiers must be unique and namespaced.');
      seenIDs.add(id);
    }
    for (const name of ['href', 'src', 'cite']) {
      const value = stringProperty(node.properties?.[name]);
      if (value) validateURL(value, node.tagName, name);
    }
    const style = stringProperty(node.properties?.style);
    if (style) validateInlineStyle(style, node, ancestors);
    if (node.tagName === 'style') validateMathJaxStylesheet(textContent(node));
    const projectPath = property(node, 'dataRinAssetPath', 'data-rin-asset-path');
    if (projectPath && !assetPaths.has(projectPath)) throw finalizerError('markdown.finalize.asset.unknown', 'Final image references an undeclared project asset.');
    for (const marker of ['dataRinDiagramObjectId', 'dataRinMathObjectId', 'data-rin-diagram-object-id', 'data-rin-math-object-id']) {
      const artifactID = stringProperty(node.properties?.[marker]);
      if (!artifactID) continue;
      if (!artifactIDs.has(artifactID)) throw finalizerError('markdown.finalize.artifact.unknown', 'Final HTML references an undeclared generated artifact.');
      seenArtifactIDs.add(artifactID);
    }
  });
  for (const artifactID of artifactIDs) {
    if (!seenArtifactIDs.has(artifactID)) throw finalizerError('markdown.finalize.artifact.unreferenced', 'Resolved artifact is not referenced by final HTML.');
  }
}

function validateClasses(value) {
  const classes = Array.isArray(value) ? value : value == null ? [] : String(value).split(/\s+/);
  for (const name of classes) {
    if (!name) continue;
    if (!exactClasses.has(name) && !classPrefixes.some((prefix) => name.startsWith(prefix))) {
      throw finalizerError('markdown.finalize.class_disallowed', `Class ${name} is not in the final schema.`);
    }
  }
}

function validateURL(value, tag, attribute) {
  const raw = String(value).trim();
  if (!raw || raw.startsWith('#')) return;
  if (/[\u0000-\u001f\u007f]/.test(raw) || raw.startsWith('//') || forbiddenURLPattern.test(raw) || localPathPattern.test(raw)) {
    throw finalizerError('markdown.finalize.url_disallowed', `Unsafe ${attribute} URL on <${tag}>.`);
  }
  let parsed;
  try { parsed = new URL(raw, 'https://rinspace.invalid/'); } catch { throw finalizerError('markdown.finalize.url_disallowed', 'Malformed final URL.'); }
  const explicit = /^[A-Za-z][A-Za-z0-9+.-]*:/.test(raw);
  if (explicit && !['http:', 'https:', 'mailto:'].includes(parsed.protocol)) throw finalizerError('markdown.finalize.url_disallowed', 'Final URL protocol is not allowed.');
  if ((attribute === 'src' || attribute === 'cite') && explicit && !['http:', 'https:'].includes(parsed.protocol)) {
    throw finalizerError('markdown.finalize.url_disallowed', 'Resource URL protocol is not allowed.');
  }
}

function validateInlineStyle(style, node, ancestors) {
  if (forbiddenCSSPattern.test(style) || /[<>{}@]/.test(style)) throw finalizerError('markdown.finalize.style_disallowed', 'Unsafe inline style.');
  const inShiki = node.tagName === 'pre' && classList(node).includes('shiki') ||
    ancestors.some((ancestor) => ancestor.type === 'element' && ancestor.tagName === 'pre' && classList(ancestor).includes('shiki'));
  const inMath = node.tagName.startsWith('mjx-') || ancestors.some((ancestor) => ancestor.type === 'element' && ancestor.tagName.startsWith('mjx-'));
  if (!inShiki && !inMath) throw finalizerError('markdown.finalize.style_disallowed', 'Inline styles are restricted to pinned Shiki and MathJax output.');
  const shikiProperties = new Set(['background-color', 'color', 'font-style', 'font-weight', 'text-decoration']);
  const mathProperties = new Set([
    ...shikiProperties, 'border', 'border-bottom', 'border-left', 'border-right', 'border-top',
    'box-sizing', 'display', 'font-family', 'font-size', 'height', 'left', 'line-height', 'margin',
    'margin-bottom', 'margin-left', 'margin-right', 'margin-top', 'max-width', 'min-width', 'overflow', 'overflow-x',
    'padding', 'padding-bottom', 'padding-left', 'padding-right', 'padding-top', 'position', 'right', 'text-align', 'top',
    'transform', 'transform-origin', 'vertical-align', 'width',
  ]);
  const allowed = inMath ? mathProperties : shikiProperties;
  for (const declaration of style.split(';')) {
    if (!declaration.trim()) continue;
    const separator = declaration.indexOf(':');
    const propertyName = declaration.slice(0, separator).trim().toLowerCase();
    const value = declaration.slice(separator + 1).trim();
    if (separator < 1 || !allowed.has(propertyName) || !safeCSSValue(value)) {
      const label = /^[a-z-]{1,64}$/.test(propertyName) ? ` ${propertyName}` : '';
      throw finalizerError('markdown.finalize.style_disallowed', `Inline style property${label} or its value is not allowed.`);
    }
  }
}

function safeCSSValue(value) {
  if (!value || forbiddenCSSPattern.test(value) || /url\s*\(/i.test(value) || /[^A-Za-z0-9#.,%+\-\s()]/.test(value)) return false;
  for (const match of value.matchAll(/([A-Za-z][A-Za-z0-9-]*)\s*\(/g)) {
    if (!['calc', 'scale', 'scaleX', 'scaleY', 'translate', 'translateX', 'translateY'].includes(match[1])) return false;
  }
  return true;
}

function validateMathJaxStylesheet(css) {
  if (!css || Buffer.byteLength(css, 'utf8') > maximumCSSBytes || forbiddenCSSPattern.test(css) || /<|<\/style/i.test(css)) {
    throw finalizerError('markdown.finalize.css_disallowed', 'MathJax stylesheet contains an unsafe construct.');
  }
  for (const match of css.matchAll(/url\s*\(\s*(['"]?)(.*?)\1\s*\)/gi)) {
    if (!/^\/fonts\/mathjax-newcm\/woff2(?:\/[A-Za-z0-9._/-]+)?$/.test(match[2])) {
      throw finalizerError('markdown.finalize.css_disallowed', 'MathJax stylesheet references an unapproved font URL.');
    }
  }
  const withoutComments = css.replace(/\/\*[\s\S]*?\*\//g, '');
  for (const match of withoutComments.matchAll(/([^{}]+)\{/g)) {
    const selector = match[1].trim();
    if (selector.startsWith('@font-face')) continue;
    if (selector === '@media (prefers-color-scheme: dark)') continue;
    if (selector.startsWith('@')) throw finalizerError('markdown.finalize.css_disallowed', 'MathJax stylesheet contains an unapproved at-rule.');
    for (const part of selector.split(',')) {
      const value = part.trim();
      if (!/(?:mjx|MathJax|MJX)/.test(value) && !/^\.NCM-[A-Za-z0-9-]+$/.test(value)) {
        throw finalizerError('markdown.finalize.css_disallowed', 'MathJax stylesheet selector escapes its namespace.');
      }
    }
  }
}

function validateFinalHTML(html, assets, resolved) {
  if (/<\/?\s*rin-work\b/i.test(html) || /<\/?\s*(?:script|iframe|object|embed|form|template|svg|math)\b/i.test(html)) {
    throw finalizerError('markdown.finalize.output_invalid', 'Final HTML contains a forbidden element, handler, or placeholder.');
  }
  if (localPathPattern.test(html)) throw finalizerError('markdown.finalize.output_invalid', 'Final HTML exposes a renderer-local path.');
  const parsed = parseFragment(html);
  enforceTreeLimits(parsed);
  validateCandidateTree(parsed, assets, resolved);
}

function collectArtifacts(resolved) {
  const byID = new Map();
  for (const unit of resolved.values()) {
    if (!unit.artifact) continue;
    const previous = byID.get(unit.artifact.artifactId);
    if (previous && JSON.stringify(previous) !== JSON.stringify(unit.artifact)) {
      throw finalizerError('markdown.finalize.artifact.conflict', 'Resolved artifacts have conflicting metadata.');
    }
    byID.set(unit.artifact.artifactId, unit.artifact);
  }
  return [...byID.values()].sort((left, right) => left.artifactId.localeCompare(right.artifactId));
}

function mergeDiagnostics(existing, resolved, workUnits) {
  const diagnostics = [...existing];
  for (const workUnit of workUnits) diagnostics.push(...resolved.get(workUnit.id).diagnostics);
  if (diagnostics.length <= maximumDiagnostics) return diagnostics;
  return [...diagnostics.slice(0, maximumDiagnostics - 1), {
    code: 'markdown.diagnostics.truncated', severity: 'warning',
    message: 'Additional finalization diagnostics were omitted by the count limit.', stage: 'finalize',
  }];
}

function validateFinalBundle(bundle) {
  if (bundle?.schemaVersion !== 'rin-document-bundle/v2' || bundle.state !== 'final' ||
      bundle.contentKind !== 'markdown' || !sha256Pattern.test(bundle.projectHash) || !sha256Pattern.test(bundle.bundleHash) ||
      !Array.isArray(bundle.pages) || bundle.pages.length < 1 || bundle.pages.length > 2048 || bundle.workUnits?.length !== 0) {
    throw finalizerError('markdown.finalize.bundle_invalid', 'Final Bundle identity or state is invalid.');
  }
  const pageIDs = new Set();
  const pagePaths = new Set();
  for (const page of bundle.pages) {
    if (!validPageID(page.id) || pageIDs.has(page.id) || typeof page.sourcePath !== 'string' || pagePaths.has(page.sourcePath) ||
        page.fragmentFormat !== finalFragmentFormat || typeof page.fragment !== 'string' || /<\/?\s*rin-work\b/i.test(page.fragment) ||
        !Array.isArray(page.toc) || page.toc.some((entry) => !identifierPattern.test(entry.id) || !entry.id.startsWith('rin-md-'))) {
      throw finalizerError('markdown.finalize.bundle_invalid', 'Final Bundle page is invalid.');
    }
    pageIDs.add(page.id);
    pagePaths.add(page.sourcePath);
	validateFinalSemanticBlocks(page);
  }
  if (!Array.isArray(bundle.diagnostics) || bundle.diagnostics.length > maximumDiagnostics || bundle.diagnostics.some((item) => !validDiagnostic(item))) {
    throw finalizerError('markdown.finalize.bundle_invalid', 'Final Bundle diagnostics are invalid.');
  }
  return bundle;
}

function validateFinalSemanticBlocks(page) {
  if (!Array.isArray(page.blocks) || page.blocks.length === 0) {
	throw finalizerError('markdown.finalize.bundle_invalid', 'Final Bundle semantic blocks are missing.');
  }
  const seen = new Set();
  for (const block of page.blocks) {
	if (!blockIDPattern.test(block?.id) || seen.has(block.id) || !blockKinds.has(block.kind) ||
		typeof block.text !== 'string' || block.text !== normalizeBlockText(block.text) || block.textHash !== sha256(block.text) ||
		!Array.isArray(block.headingPath) || block.headingPath.some((id) => !identifierPattern.test(id))) {
	  throw finalizerError('markdown.finalize.bundle_invalid', 'Final Bundle semantic block is invalid.');
	}
	seen.add(block.id);
  }
  const matches = semanticBlockMarkers(page.fragment);
  const mismatchIndex = matches.findIndex((match, index) =>
	match.id !== page.blocks[index]?.id || match.kind !== page.blocks[index]?.kind);
  if (matches.length !== page.blocks.length || mismatchIndex >= 0) {
	throw finalizerError('markdown.finalize.bundle_invalid',
	  `Final Bundle semantic block manifest does not match its fragment (manifest=${page.blocks.length}, dom=${matches.length}, firstMismatch=${mismatchIndex}).`);
  }
}

function orderSemanticBlocksByFragment(fragment, blocks) {
  const byID = new Map(blocks.map((block) => [block.id, block]));
  const ids = semanticBlockMarkers(fragment).map((marker) => marker.id);
	if (ids.length !== blocks.length || new Set(ids).size !== blocks.length || ids.some((id) => !byID.has(id))) {
	throw finalizerError('markdown.finalize.bundle_invalid', 'Final Bundle semantic block identities do not match its fragment.');
  }
  return ids.map((id) => byID.get(id));
}

function semanticBlockMarkers(fragment) {
  const markers = [];
  walk(parseFragment(fragment), (node) => {
    if (node.type !== 'element') return;
    const id = property(node, 'dataRinBlockId', 'data-rin-block-id');
    if (!id) return;
    markers.push({ id, kind: property(node, 'dataRinBlockKind', 'data-rin-block-kind') });
  });
  return markers;
}

function normalizeBlockText(value) {
  return String(value || '').trim().replace(/\s+/gu, ' ');
}

function validDiagnostic(value) {
  return value && typeof value.code === 'string' && value.code && ['info', 'warning', 'error'].includes(value.severity) &&
    typeof value.message === 'string' && typeof value.stage === 'string' && value.stage;
}

function enforceTreeLimits(tree) {
  let count = 0;
  walk(tree, (_node, ancestors) => {
    count += 1;
    if (count > maximumNodes || ancestors.length > maximumDepth) {
      throw finalizerError('markdown.finalize.tree_too_large', 'HTML tree exceeds node or nesting limits.');
    }
  });
}

function walk(node, visitor, ancestors = []) {
  visitor(node, ancestors);
  if (!Array.isArray(node.children)) return;
  for (const child of node.children) walk(child, visitor, [...ancestors, node]);
}

function property(node, ...names) {
  for (const name of names) {
    const value = stringProperty(node.properties?.[name]);
    if (value) return value;
  }
  return '';
}

function stringProperty(value) {
  if (Array.isArray(value)) return value.join(' ');
  return typeof value === 'string' || typeof value === 'number' ? String(value) : '';
}

function classList(node) {
  const value = node.properties?.className;
  return Array.isArray(value) ? value.map(String) : typeof value === 'string' ? value.split(/\s+/) : [];
}

function textContent(node) {
  if (node.type === 'text') return node.value || '';
  return Array.isArray(node.children) ? node.children.map(textContent).join('') : '';
}

function stringifyTree(tree) {
  return unified().use(rehypeStringify, { allowDangerousHtml: false }).stringify(tree);
}

// Report only structural metadata so an untrusted Markdown source is never copied into build or
// production logs. This makes sanitizer/schema drift actionable without weakening the final gate.
function firstSanitizerDifference(candidate, sanitized, path = 'root') {
  if (candidate?.type !== sanitized?.type) return `${path}: node type changed`;
  if (candidate?.tagName !== sanitized?.tagName) return `${path}: element changed`;
  const candidateProperties = candidate?.properties || {};
  const sanitizedProperties = sanitized?.properties || {};
  for (const name of [...new Set([...Object.keys(candidateProperties), ...Object.keys(sanitizedProperties)])].sort()) {
    if (!(name in sanitizedProperties)) return `${path}: property ${name} removed`;
    if (!(name in candidateProperties)) return `${path}: property ${name} added`;
    if (JSON.stringify(candidateProperties[name]) !== JSON.stringify(sanitizedProperties[name])) {
      return `${path}: property ${name} changed`;
    }
  }
  if (candidate?.type === 'text' && candidate.value !== sanitized?.value) return `${path}: text changed`;
  const candidateChildren = candidate?.children || [];
  const sanitizedChildren = sanitized?.children || [];
  if (candidateChildren.length !== sanitizedChildren.length) return `${path}: child count changed`;
  for (let index = 0; index < candidateChildren.length; index += 1) {
    const child = candidateChildren[index];
    const label = child?.type === 'element' ? child.tagName : child?.type || 'node';
    const difference = firstSanitizerDifference(child, sanitizedChildren[index], `${path}/${label}[${index}]`);
    if (difference) return difference;
  }
  return '';
}

function byteLimit(value, maximum, code, message) {
  if (Buffer.byteLength(value, 'utf8') > maximum) throw finalizerError(code, message);
}

function sha256(value) {
  return createHash('sha256').update(value).digest('hex');
}

function canonicalize(value) {
  if (Array.isArray(value)) return value.map(canonicalize);
  if (value && typeof value === 'object') {
    return Object.fromEntries(Object.entries(value).sort(([left], [right]) => left.localeCompare(right)).map(([key, child]) => [key, canonicalize(child)]));
  }
  return value;
}

function finalizerError(code, message) {
  return Object.assign(new Error(message), { code });
}
