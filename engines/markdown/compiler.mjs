import { createHash, randomBytes } from 'node:crypto';
import { posix as path } from 'node:path';

import { toString as mdastToString } from 'mdast-util-to-string';
import rehypeStringify from 'rehype-stringify';
import remarkDirective from 'remark-directive';
import remarkGfm from 'remark-gfm';
import remarkMath from 'remark-math';
import remarkParse from 'remark-parse';
import remarkRehype from 'remark-rehype';
import { unified } from 'unified';
import { visit } from 'unist-util-visit';
import { VFile } from 'vfile';

export const DOCUMENT_BUNDLE_SCHEMA_VERSION = 'rin-document-bundle/v2';
export const PROJECT_GRAPH_SCHEMA_VERSION = 'rin-project-graph/v1';
export const MARKDOWN_ADAPTER_NAME = 'rin-markdown';
export const MARKDOWN_ADAPTER_VERSION = '0.6.0';
export const DRAFT_FRAGMENT_FORMAT = 'html-with-rin-placeholders';
export const SAFE_ADMONITIONS = Object.freeze([
  'note',
  'tip',
  'important',
  'warning',
  'caution',
]);

const sha256Pattern = /^[a-f0-9]{64}$/;
const identifierPattern = /^[A-Za-z][A-Za-z0-9._:-]{0,127}$/;
const pageIdentifierPattern = /^\p{L}[\p{L}\p{N}\p{M}._:-]{0,127}$/u;
const workIDPattern = /^rw_[a-f0-9]{32}$/;
const blockIDPattern = /^rb_[a-f0-9]{32}$/;
const blockKinds = new Set(['heading', 'paragraph', 'list-item', 'theorem', 'math', 'code', 'figure', 'table', 'quote']);
const canonicalPlaceholderPattern = /<rin-work data-id="(rw_[a-f0-9]{32})"(?: data-rin-block-id="rb_[a-f0-9]{32}" data-rin-block-kind="(?:heading|paragraph|list-item|theorem|math|code|figure|table|quote)")?><\/rin-work>/g;
const anyWorkElementPattern = /<\s*\/?\s*rin-work\b/i;
const maximumDiagnostics = 256;
const maximumAssets = 2048;
const defaultMacroContextHash = sha256('{}');
const admonitionTitles = Object.freeze({
  note: 'Note',
  tip: 'Tip',
  important: 'Important',
  warning: 'Warning',
  caution: 'Caution',
});
const diagramTypeAliases = new Map(Object.entries({
  tikz: 'tikzpicture',
  tikzpicture: 'tikzpicture',
  tikzcd: 'tikzcd',
  'tikz-cd': 'tikzcd',
  axis: 'axis',
  pgfplots: 'axis',
  pspicture: 'pspicture',
  xymatrix: 'xymatrix',
  xy: 'xymatrix',
  amscd: 'amscd',
  cd: 'amscd',
  picture: 'picture',
  forest: 'forest',
  circuitikz: 'circuitikz',
  chemfig: 'chemfig',
  'chemfig-scheme': 'chemfig-scheme',
  scheme: 'chemfig-scheme',
}));
const safeDimensionPattern = /^(?:0|[1-9][0-9]{0,3})(?:\.[0-9]{1,4})?(?:pt|pc|in|bp|cm|mm|dd|cc|sp|em|ex)$/;
const codeLanguageAliases = new Map(Object.entries({
  bash: 'bash', sh: 'shellscript', shell: 'shellscript', shellscript: 'shellscript',
  zsh: 'shellscript', console: 'shellscript', c: 'c', h: 'c', cpp: 'cpp', 'c++': 'cpp',
  cplusplus: 'cpp', cxx: 'cpp', hpp: 'cpp', cs: 'csharp', 'c#': 'csharp', csharp: 'csharp',
  css: 'css', scss: 'scss', go: 'go', html: 'html', java: 'java', js: 'javascript',
  javascript: 'javascript', json: 'json', jsx: 'jsx', kotlin: 'kotlin', kt: 'kotlin',
  latex: 'latex', tex: 'tex', lua: 'lua', md: 'markdown', markdown: 'markdown',
  m: 'objective-c', objc: 'objective-c', 'obj-c': 'objective-c', objectivec: 'objective-c',
  'objective-c': 'objective-c', mm: 'objective-cpp', 'objective-cpp': 'objective-cpp',
  php: 'php', py: 'python', python: 'python', rb: 'ruby', ruby: 'ruby', rust: 'rust',
  rs: 'rust', sql: 'sql', toml: 'toml', ts: 'typescript', typescript: 'typescript',
  tsx: 'tsx', xml: 'xml', yaml: 'yaml', yml: 'yaml', text: '', txt: '', plain: '',
}));

export async function compileMarkdownDraft(payload, options = {}) {
  const input = normalizeCompileInput(payload);
  const state = {
    source: input.source,
    sourcePath: input.sourcePath,
    diagnostics: [],
    droppedDiagnostics: 0,
    workUnits: [],
    toc: [],
    headingAliases: new Map(),
    headingIDs: new Set(),
	blocks: [],
	blockDuplicateOrdinals: new Map(),
    firstH1: '',
    assetsByPath: input.assetsByPath,
    usedAssetPaths: new Set(),
    macroContextHash: input.macroContextHash,
    language: input.language,
    book: options.book || null,
  };

  const file = new VFile({ path: input.sourcePath, value: input.source });
  const rendered = await createDraftProcessor(state).process(file);
  finishDiagnostics(state);
  const sourceHash = sha256(input.source);
  const usedAssets = [...state.usedAssetPaths]
    .sort((left, right) => left.localeCompare(right))
    .map((projectPath) => state.assetsByPath.get(projectPath));
  const dependencyHashes = [...new Set([
    sourceHash,
    ...input.dependencyHashes,
    ...usedAssets.map((asset) => asset.sha256),
  ])].sort((left, right) => left.localeCompare(right));
  const title = boundedText(input.title || state.firstH1 || titleFromPath(input.sourcePath), 512);
  const engineVersion = boundedText(options.engineVersion || 'unified-pinned', 256);
  const adapterVersion = boundedText(options.adapterVersion || MARKDOWN_ADAPTER_VERSION, 64);

  const bundle = {
    schemaVersion: DOCUMENT_BUNDLE_SCHEMA_VERSION,
    projectHash: input.projectHash,
    state: 'draft',
    contentKind: 'markdown',
    documentEngine: MARKDOWN_ADAPTER_NAME,
    title,
    pages: [{
      id: input.pageId || pageIDFromPath(input.sourcePath),
      sourcePath: input.sourcePath,
      ...(title ? { title } : {}),
      fragment: String(rendered),
      fragmentFormat: DRAFT_FRAGMENT_FORMAT,
      toc: state.toc,
      dependencyHashes,
	  blocks: state.blocks,
    }],
    workUnits: state.workUnits,
    assets: usedAssets,
    diagnostics: state.diagnostics,
    provenance: {
      adapter: MARKDOWN_ADAPTER_NAME,
      adapterVersion,
      engineVersion,
      projectGraphSchemaVersion: PROJECT_GRAPH_SCHEMA_VERSION,
    },
  };
  validateDraftBundle(bundle);
  return { publishable: false, bundle };
}

export async function compileMarkdownBookDraft(payload, options = {}) {
  const input = normalizeBookCompileInput(payload);
  const headingIndexes = new Map(input.pages.map((page) => [
    page.sourcePath,
    collectHeadingIndex(page.source),
  ]));
  const pageByPath = new Map(input.pages.map((page) => [page.sourcePath, {
    id: page.pageId,
    title: page.title || titleFromPath(page.sourcePath),
  }]));
  const drafts = [];
  for (const page of input.pages) {
    drafts.push(await compileMarkdownDraft({
      ...page,
      projectHash: input.projectHash,
      language: page.language || input.language,
      macroContextHash: input.macroContextHash,
      assets: input.assets,
    }, {
      ...options,
      book: { pageByPath, headingIndexes },
    }));
  }

  const pages = drafts.map((draft) => draft.bundle.pages[0]);
  for (let index = 0; index < pages.length; index += 1) {
    pages[index].fragment = appendBookNavigation(pages[index].fragment, pages, index);
  }
  const assetsByID = new Map();
  for (const draft of drafts) {
    for (const asset of draft.bundle.assets) {
      const previous = assetsByID.get(asset.id);
      if (previous && JSON.stringify(previous) !== JSON.stringify(asset)) {
        throw compilerError('markdown.book.asset.conflict', 'Markdown Book pages declared conflicting asset metadata');
      }
      assetsByID.set(asset.id, asset);
    }
  }
  const bundle = {
    schemaVersion: DOCUMENT_BUNDLE_SCHEMA_VERSION,
    projectHash: input.projectHash,
    state: 'draft',
    contentKind: 'markdown',
    documentEngine: MARKDOWN_ADAPTER_NAME,
    title: boundedText(input.title || pages[0].title || 'Markdown Book', 512),
    pages,
    workUnits: drafts.flatMap((draft) => draft.bundle.workUnits),
    assets: [...assetsByID.values()].sort((left, right) => left.projectPath.localeCompare(right.projectPath)),
    diagnostics: drafts.flatMap((draft) => draft.bundle.diagnostics).slice(0, maximumDiagnostics),
    provenance: drafts[0].bundle.provenance,
  };
  validateDraftBundle(bundle);
  return { publishable: false, bundle };
}

function createDraftProcessor(state) {
  return unified()
    .use(remarkParse)
    .use(remarkGfm)
    .use(remarkMath)
    .use(remarkDirective)
    .use(rinMarkdownSemantics, state)
    .use(remarkRehype, {
      allowDangerousHtml: false,
      clobberPrefix: 'rin-md-',
      footnoteLabel: '脚注',
      footnoteBackLabel: (_referenceIndex, rereferenceIndex) =>
        rereferenceIndex > 1 ? `返回正文脚注引用 ${rereferenceIndex}` : '返回正文脚注',
    })
    .use(wrapDraftHast)
    .use(rehypeStringify, { allowDangerousHtml: false });
}

function rinMarkdownSemantics(state) {
  return (tree) => {
	removeReservedBlockProperties(tree);
    applyStableHeadingIDs(tree, state);
    transformDirectiveChildren(tree, state);
    applySafeURLs(tree, state);
    applyFigures(tree, state);
    transformWorkAndRawHTMLChildren(tree, state);
	applySemanticBlocks(tree, state);
  };
}

function removeReservedBlockProperties(tree) {
  visit(tree, (node) => {
    if (!node?.data?.hProperties || typeof node.data.hProperties !== 'object') return;
    for (const key of Object.keys(node.data.hProperties)) {
      if (/^data(?:RinBlock|-rin-block)/i.test(key)) delete node.data.hProperties[key];
    }
  });
}

function applySemanticBlocks(tree, state) {
  const headingStack = [];

  function visitChildren(parent, context = {}) {
    if (!Array.isArray(parent.children)) return;
    for (const node of parent.children) visitNode(node, context);
  }

  function visitNode(node, context) {
    if (node.type === 'heading') {
      while (headingStack.length && headingStack.at(-1).depth >= node.depth) headingStack.pop();
      const explicitAnchor = String(node.data?.hProperties?.id || '');
      addSemanticBlock(node, 'heading', nodeBlockText(node), headingStack.map((item) => item.id), state, explicitAnchor);
      headingStack.push({ depth: node.depth, id: explicitAnchor });
      return;
    }

    const headingPath = headingStack.map((item) => item.id);
    const candidate = node.data?.rinBlockCandidate;
    if (candidate) {
      addSemanticBlock(node, candidate.kind, candidate.text, headingPath, state, candidate.explicitAnchor || '', candidate.sourceLocation);
      return;
    }

    if (node.type === 'listItem') {
      const hasNestedList = node.children?.some((child) => child.type === 'list');
      if (!hasNestedList) {
        addSemanticBlock(node, 'list-item', nodeBlockText(node), headingPath, state);
        return;
      }
      visitChildren(node, { ...context, directListItem: true });
      return;
    }
    if (node.type === 'blockquote') {
      visitChildren(node, { ...context, directQuote: true });
      return;
    }
    if (node.type === 'paragraph') {
      if (context.directListItem) {
        addSemanticBlock(node, 'list-item', nodeBlockText(node), headingPath, state);
      } else if (context.directQuote) {
        addSemanticBlock(node, 'quote', nodeBlockText(node), headingPath, state);
      } else if (node.data?.hName === 'figure') {
        addSemanticBlock(node, 'figure', nodeBlockText(node), headingPath, state);
      } else {
        addSemanticBlock(node, 'paragraph', nodeBlockText(node), headingPath, state);
      }
      return;
    }
    if (node.type === 'table') {
      addSemanticBlock(node, 'table', nodeBlockText(node), headingPath, state);
      return;
    }
    visitChildren(node, context);
  }

  visitChildren(tree);
}

function addSemanticBlock(node, kind, value, headingPath, state, explicitAnchor = '', candidateLocation) {
  if (!blockKinds.has(kind)) throw compilerError('markdown.block.kind_invalid', 'Markdown semantic block kind is invalid');
  const text = normalizeBlockText(value);
  const identityBase = explicitAnchor
    ? ['anchor', state.sourcePath, kind, explicitAnchor]
    : ['content', state.sourcePath, ...headingPath, kind, text];
  const duplicateKey = identityBase.join('\u0000');
  const duplicateOrdinal = state.blockDuplicateOrdinals.get(duplicateKey) || 0;
  state.blockDuplicateOrdinals.set(duplicateKey, duplicateOrdinal + 1);
  const identity = explicitAnchor ? identityBase : [...identityBase, String(duplicateOrdinal)];
  const id = `rb_${sha256(`rin-document-block/v1\u0000${identity.join('\u0000')}`).slice(0, 32)}`;
  const location = candidateLocation || sourceLocation(state.sourcePath, node);
  const block = {
    id,
    kind,
    text,
    textHash: sha256(text),
    headingPath: [...headingPath],
    ...(location ? { sourceLocation: location } : {}),
  };
  node.data = {
    ...(node.data || {}),
    hProperties: {
      ...(node.data?.hProperties || {}),
      'data-rin-block-id': id,
      'data-rin-block-kind': kind,
    },
  };
  state.blocks.push(block);
}

function nodeBlockText(node) {
  return mdastToString(node);
}

function normalizeBlockText(value) {
  return String(value || '').trim().replace(/\s+/gu, ' ');
}

function applyStableHeadingIDs(tree, state) {
  visit(tree, 'heading', (node) => {
    const text = boundedText(mdastToString(node).replace(/\s+/g, ' ').trim(), 512);
    const base = headingIdentifier(text);
    const id = uniqueIdentifier(base, state.headingIDs);
    node.data = fixedHastData(node.data, 'h' + node.depth, { id });
    state.toc.push({ id, depth: node.depth, text });
    if (!state.firstH1 && node.depth === 1) state.firstH1 = text;
    if (!state.headingAliases.has(base)) state.headingAliases.set(base, id);
    state.headingAliases.set(id, id);
  });
}

function transformDirectiveChildren(parent, state) {
  if (!Array.isArray(parent.children)) return;
  for (let index = 0; index < parent.children.length; index += 1) {
    const node = parent.children[index];
    if (!isDirective(node)) {
      transformDirectiveChildren(node, state);
      continue;
    }

    const name = String(node.name || '').toLowerCase();
    if (node.type === 'containerDirective' && SAFE_ADMONITIONS.includes(name)) {
      const label = takeDirectiveLabel(node) || admonitionTitles[name];
      if (Object.keys(node.attributes || {}).length > 0) {
        addDiagnostic(state, 'markdown.directive.attributes_ignored', 'warning',
          'Admonition attributes are not supported and were ignored.', node);
      }
      node.data = fixedHastData(undefined, 'aside', {
        className: ['rin-admonition', `rin-admonition-${name}`],
        role: 'note',
        'data-rin-admonition': name,
      });
      node.children.unshift({
        type: 'paragraph',
        data: fixedHastData(undefined, 'p', { className: ['rin-admonition-title'] }),
        children: [{ type: 'text', value: boundedText(label, 256) }],
      });
      transformDirectiveChildren(node, state);
      continue;
    }

    if (node.type === 'containerDirective' && name === 'diagram') {
      const result = diagramUnitFromDirective(node, state);
      if (result) {
        parent.children[index] = placeholderNode(result.id, {
          kind: 'figure', text: result.source, sourceLocation: result.sourceLocation,
        });
      } else {
        parent.children[index] = directiveFallback(node, parent, state.source);
      }
      continue;
    }

    addDiagnostic(state, 'markdown.directive.unsupported', 'warning',
      'Unsupported directive was preserved as inert text.', node);
    parent.children[index] = directiveFallback(node, parent, state.source);
  }
}

function diagramUnitFromDirective(node, state) {
  const attributes = Object.fromEntries(
    Object.entries(node.attributes || {}).map(([key, value]) => [String(key), String(value)]),
  );
  const type = normalizeDiagramType(attributes.type);
  const alignment = String(attributes.alignment || '').toLowerCase().trim();
  delete attributes.type;
  delete attributes.alignment;
  const source = directiveBodySource(node, state.source);
  try {
    if (!type) throw new Error('unsupported type');
    if (!source.trim()) throw new Error('empty source');
    if (alignment && !['center', 'flushleft', 'flushright'].includes(alignment)) {
      throw new Error('unsupported alignment');
    }
    const serializedOptions = serializeDiagramOptions(type, attributes);
    const unit = {
      kind: 'diagram',
      id: newWorkID(),
      diagramType: type,
      source,
      ...(serializedOptions ? { options: serializedOptions } : {}),
      ...(alignment ? { layout: { alignment } } : {}),
      sourceLocation: sourceLocation(state.sourcePath, node),
    };
    state.workUnits.push(unit);
    return unit;
  } catch {
    addDiagnostic(state, 'markdown.diagram.invalid', 'warning',
      'Diagram directive was preserved as inert text because its type or options are invalid.', node);
    return null;
  }
}

function applySafeURLs(tree, state) {
  visit(tree, (node) => {
    if (node.type !== 'link' && node.type !== 'image') return;
    const image = node.type === 'image';
    const result = safeURL(node.url, image);
    if (!result.safe) {
      addDiagnostic(state, 'markdown.url.disallowed', 'warning',
        image ? 'Image URL was removed by the Markdown URL policy.' : 'Link URL was neutralized by the Markdown URL policy.', node);
      node.url = image ? '' : '#';
      return;
    }
    if (result.kind === 'fragment') {
      const rewritten = rewriteHeadingFragment(node.url, state);
      if (rewritten) node.url = rewritten;
      return;
    }
    if (!image && result.kind === 'relative' && state.book && isMarkdownPath(result.path)) {
      rewriteBookPageLink(node, state, result.path);
      return;
    }
    if (!image || result.kind !== 'relative') return;
    const projectPath = resolveProjectAssetPath(state.sourcePath, result.path);
    if (!projectPath || !state.assetsByPath.has(projectPath)) {
      addDiagnostic(state, 'markdown.image.asset_unresolved', 'warning',
        'Project-relative image was removed because no matching Project Graph asset was supplied.', node);
      node.url = '';
      return;
    }
    state.usedAssetPaths.add(projectPath);
    node.data = fixedHastData(node.data, 'img', { 'data-rin-project-path': projectPath });
  });
}

function rewriteBookPageLink(node, state, relativePath) {
  const targetPath = resolveProjectPath(state.sourcePath, relativePath);
  const target = targetPath ? state.book.pageByPath.get(targetPath) : null;
  if (!target) {
    addDiagnostic(state, 'markdown.link.page_unresolved', 'warning',
      'Cross-page Markdown link was neutralized because its target is not a Book page.', node);
    node.url = '#';
    return;
  }
  const hash = String(node.url).includes('#') ? String(node.url).slice(String(node.url).indexOf('#') + 1) : '';
  if (!hash) {
    node.url = targetPath;
    return;
  }
  let decoded;
  try { decoded = decodeURIComponent(hash); } catch { decoded = ''; }
  const index = state.book.headingIndexes.get(targetPath);
  const targetID = decoded && (index?.get(decoded) || index?.get(headingIdentifier(decoded)));
  if (!targetID) {
    addDiagnostic(state, 'markdown.link.page_fragment_unresolved', 'warning',
      'Cross-page Markdown heading link does not resolve in the target page.', node);
    node.url = targetPath;
    return;
  }
  node.url = `${targetPath}#${targetID}`;
}

function applyFigures(tree, state) {
  visit(tree, 'paragraph', (node) => {
    if (node.children?.length !== 1 || node.children[0].type !== 'image') return;
    const image = node.children[0];
    const quiver = isQuiverDiagramURL(image.url);
    const originalAlt = String(image.alt || '').trim();
    const caption = quiver ? '' : String(image.title || '').trim() ||
      (isGeneratedImageAlt(originalAlt) ? '' : originalAlt);
    if (isGeneratedImageAlt(originalAlt)) image.alt = '';
    image.data = fixedHastData(image.data, 'img', {
      ...(image.data?.hProperties || {}),
      className: quiver ? ['rin-quiver-image'] : ['rin-markdown-image'],
      loading: 'lazy',
    });
    node.data = fixedHastData(undefined, 'figure', {
      className: quiver
        ? ['rin-markdown-figure', 'rin-quiver', 'rin-quiver-image-figure']
        : ['rin-markdown-figure'],
    });
    if (caption) {
      node.children.push({
        type: 'paragraph',
        data: fixedHastData(undefined, 'figcaption', { className: ['rin-markdown-caption'] }),
        children: [{ type: 'text', value: boundedText(caption, 1024) }],
      });
    }
  });
}

function transformWorkAndRawHTMLChildren(parent, state) {
  if (!Array.isArray(parent.children)) return;
  for (let index = 0; index < parent.children.length; index += 1) {
    const node = parent.children[index];
    if (node.type === 'inlineMath' || node.type === 'math') {
      if (!String(node.value || '').trim()) {
        addDiagnostic(state, 'markdown.math.empty', 'warning', 'Empty math was preserved as text.', node);
        parent.children[index] = { type: 'text', value: sourceSlice(state.source, node) };
        continue;
      }
      const unit = {
        kind: 'math',
        id: newWorkID(),
        source: String(node.value),
        display: node.type === 'math',
        macroContextHash: state.macroContextHash,
        ...(state.language ? { accessibilityContext: { language: state.language } } : {}),
        sourceLocation: sourceLocation(state.sourcePath, node),
      };
      state.workUnits.push(unit);
      parent.children[index] = placeholderNode(unit.id, node.type === 'math' ? {
        kind: 'math', text: unit.source, sourceLocation: unit.sourceLocation,
      } : undefined);
      continue;
    }
    if (node.type === 'code') {
      const language = typeof node.lang === 'string' ? node.lang : '';
      const meta = typeof node.meta === 'string' ? node.meta : '';
      if (language && !codeLanguageAliases.has(language.toLowerCase())) {
        addDiagnostic(state, 'markdown.code.language_unknown', 'warning',
          'Unknown code language will use escaped plain-code fallback.', node);
      }
      const unit = {
        kind: 'code',
        id: newWorkID(),
        source: String(node.value || ''),
        ...(language ? { language } : {}),
        ...(meta ? { meta } : {}),
        sourceLocation: sourceLocation(state.sourcePath, node),
      };
      state.workUnits.push(unit);
      parent.children[index] = placeholderNode(unit.id, {
        kind: 'code', text: unit.source, sourceLocation: unit.sourceLocation,
      });
      continue;
    }
    if (node.type === 'html') {
      const value = String(node.value || '');
      if (/^<br\s*\/?>$/i.test(value.trim())) {
        parent.children[index] = { type: 'break' };
      } else if (/^<!--\s*rin-quiver\b[\s\S]*-->$/i.test(value.trim())) {
        addDiagnostic(state, 'markdown.quiver_comment.deprecated', 'info',
          'Legacy Quiver comment metadata was removed.', node);
        parent.children[index] = { type: 'text', value: '' };
      } else {
        addDiagnostic(state, 'markdown.raw_html.disabled', 'warning',
          'Raw HTML was escaped because canonical Markdown publication disables it.', node);
        parent.children[index] = { type: 'text', value };
      }
      continue;
    }
    transformWorkAndRawHTMLChildren(node, state);
  }
}

function wrapDraftHast() {
  return (tree) => {
    tree.children = [{
      type: 'element',
      tagName: 'article',
      properties: { className: ['rin-markdown-draft'] },
      children: tree.children,
    }];
  };
}

function normalizeCompileInput(payload) {
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) {
    throw compilerError('markdown.payload.invalid', 'Markdown draft payload must be an object');
  }
  const source = typeof payload.source === 'string' ? payload.source : '';
  if (!source) throw compilerError('markdown.source.empty', 'Markdown source is empty');
  const sourcePath = String(payload.sourcePath || '');
  if (!canonicalPath(sourcePath) || !/\.(?:md|markdown)$/i.test(sourcePath)) {
    throw compilerError('markdown.source_path.invalid', 'Markdown source path must be a canonical .md path');
  }
  const projectHash = String(payload.projectHash || '');
  if (!sha256Pattern.test(projectHash)) {
    throw compilerError('markdown.project_hash.invalid', 'Markdown project hash must be sha256');
  }
  const pageId = payload.pageId == null ? '' : String(payload.pageId);
  if (pageId && !validPageID(pageId)) {
    throw compilerError('markdown.page_id.invalid', 'Markdown page id is invalid');
  }
  const dependencyHashes = Array.isArray(payload.dependencyHashes)
    ? payload.dependencyHashes.map(String)
    : [];
  if (dependencyHashes.some((value) => !sha256Pattern.test(value))) {
    throw compilerError('markdown.dependency_hash.invalid', 'Markdown dependency hash is invalid');
  }
  const macroContextHash = payload.macroContextHash == null
    ? defaultMacroContextHash
    : String(payload.macroContextHash);
  if (!sha256Pattern.test(macroContextHash)) {
    throw compilerError('markdown.macro_context.invalid', 'Markdown macro context hash is invalid');
  }
  const assetsByPath = normalizeInputAssets(payload.assets);
  return {
    source,
    sourcePath,
    projectHash,
    pageId,
    dependencyHashes,
    macroContextHash,
    assetsByPath,
    title: typeof payload.title === 'string' ? payload.title.trim() : '',
    language: typeof payload.language === 'string' ? boundedText(payload.language.trim(), 64) : '',
  };
}

function normalizeBookCompileInput(payload) {
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) {
    throw compilerError('markdown.book.payload.invalid', 'Markdown Book payload must be an object');
  }
  const projectHash = String(payload.projectHash || '');
  if (!sha256Pattern.test(projectHash)) throw compilerError('markdown.project_hash.invalid', 'Markdown project hash must be sha256');
  if (!Array.isArray(payload.pages) || payload.pages.length < 2 || payload.pages.length > 2048) {
    throw compilerError('markdown.book.pages.invalid', 'Markdown Book requires between 2 and 2048 ordered pages');
  }
  const seenPaths = new Set();
  const seenIDs = new Set();
  const pages = payload.pages.map((page) => {
    const normalized = normalizeCompileInput({
      ...page,
      projectHash,
      macroContextHash: payload.macroContextHash,
      assets: payload.assets,
    });
    if (!normalized.pageId || seenPaths.has(normalized.sourcePath) || seenIDs.has(normalized.pageId)) {
      throw compilerError('markdown.book.pages.invalid', 'Markdown Book page paths and stable IDs must be unique');
    }
    seenPaths.add(normalized.sourcePath);
    seenIDs.add(normalized.pageId);
    return {
      source: normalized.source,
      sourcePath: normalized.sourcePath,
      pageId: normalized.pageId,
      title: normalized.title,
      language: normalized.language,
      dependencyHashes: normalized.dependencyHashes,
    };
  });
  const assetsByPath = normalizeInputAssets(payload.assets);
  return {
    projectHash,
    pages,
    assets: [...assetsByPath.values()],
    macroContextHash: payload.macroContextHash == null ? defaultMacroContextHash : String(payload.macroContextHash),
    title: typeof payload.title === 'string' ? payload.title.trim() : '',
    language: typeof payload.language === 'string' ? boundedText(payload.language.trim(), 64) : '',
  };
}

function normalizeInputAssets(value) {
  if (value == null) return new Map();
  if (!Array.isArray(value) || value.length > maximumAssets) {
    throw compilerError('markdown.assets.invalid', 'Markdown assets are invalid or exceed the count limit');
  }
  const byPath = new Map();
  const ids = new Set();
  for (const input of value) {
    if (!input || typeof input !== 'object' || Array.isArray(input)) {
      throw compilerError('markdown.assets.invalid', 'Markdown asset entry is invalid');
    }
    const asset = {
      id: String(input.id || ''),
      kind: 'project-file',
      sha256: String(input.sha256 || ''),
      bytes: Number(input.bytes),
      mediaType: String(input.mediaType || ''),
      projectPath: String(input.projectPath || ''),
    };
    if (
      !identifierPattern.test(asset.id) || ids.has(asset.id) ||
      !sha256Pattern.test(asset.sha256) ||
      !Number.isSafeInteger(asset.bytes) || asset.bytes < 0 ||
      !asset.mediaType.trim() || !canonicalPath(asset.projectPath) ||
      byPath.has(asset.projectPath)
    ) {
      throw compilerError('markdown.assets.invalid', 'Markdown asset metadata is invalid or duplicated');
    }
    ids.add(asset.id);
    byPath.set(asset.projectPath, asset);
  }
  return byPath;
}

function isDirective(node) {
  return node && ['containerDirective', 'leafDirective', 'textDirective'].includes(node.type);
}

function takeDirectiveLabel(node) {
  let label = '';
  const nextChildren = [];
  for (const child of node.children || []) {
    if (child.data?.directiveLabel) {
      if (!label) label = mdastToString(child).trim();
      continue;
    }
    if (Array.isArray(child.children)) {
      const ordinary = [];
      for (const nested of child.children) {
        if (nested.data?.directiveLabel) {
          if (!label) label = mdastToString(nested).trim();
        } else {
          ordinary.push(nested);
        }
      }
      child.children = ordinary;
      if (ordinary.length === 0 && child.data?.directiveLabel) continue;
    }
    nextChildren.push(child);
  }
  node.children = nextChildren;
  return label;
}

function directiveFallback(node, parent, source) {
  const value = sourceSlice(source, node) || `:::${String(node.name || '')}`;
  if (parent.type === 'paragraph') return { type: 'text', value };
  return { type: 'paragraph', children: [{ type: 'text', value }] };
}

function directiveBodySource(node, source) {
  const raw = sourceSlice(source, node).replace(/\r\n?/g, '\n');
  const lines = raw.split('\n');
  if (lines.length > 0) lines.shift();
  if (lines.length > 0 && /^\s*:::\s*$/.test(lines[lines.length - 1])) lines.pop();
  return lines.join('\n');
}

function normalizeDiagramType(value) {
  return diagramTypeAliases.get(String(value || '').toLowerCase().trim()) || '';
}

function serializeDiagramOptions(type, attributes) {
  const options = [];
  const seen = new Set();
  for (const [rawKey, rawValue] of Object.entries(attributes)) {
    const key = canonicalDiagramOptionKey(rawKey);
    if (!key || seen.has(key)) throw new Error('unsupported or duplicate option');
    seen.add(key);
    const [engineKey, value] = normalizeDiagramOption(type, key, rawValue);
    options.push([engineKey, value]);
  }
  options.sort(([left], [right]) => left.localeCompare(right));
  return options.map(([key, value]) => `${key}=${value}`).join(',');
}

function canonicalDiagramOptionKey(value) {
  const key = String(value).toLowerCase().trim().replaceAll('_', '-');
  if (['scale', 'xscale', 'yscale', 'rotate', 'width', 'height', 'grid'].includes(key)) return key;
  if (['row-sep', 'rowsep'].includes(key)) return 'row-sep';
  if (['column-sep', 'columnsep', 'col-sep', 'colsep'].includes(key)) return 'column-sep';
  return '';
}

function normalizeDiagramOption(type, key, rawValue) {
  const value = String(rawValue).toLowerCase().trim();
  if (['scale', 'xscale', 'yscale'].includes(key)) {
    if (!['tikzpicture', 'axis'].includes(type)) throw new Error('option not applicable');
    return [key, boundedNumber(value, 0.05, 20)];
  }
  if (key === 'rotate') {
    if (type !== 'tikzpicture') throw new Error('option not applicable');
    return [key, boundedNumber(value, -360, 360)];
  }
  if (['row-sep', 'column-sep'].includes(key)) {
    if (type !== 'tikzcd' || !['tiny', 'small', 'normal', 'large', 'huge'].includes(value)) {
      throw new Error('invalid spacing');
    }
    return [key.replace('-', ' '), value];
  }
  if (['width', 'height'].includes(key)) {
    if (type !== 'axis' || !safeDimensionPattern.test(value)) throw new Error('invalid dimension');
    return [key, value];
  }
  if (key === 'grid') {
    if (type !== 'axis' || !['none', 'major', 'minor', 'both'].includes(value)) {
      throw new Error('invalid grid');
    }
    return [key, value];
  }
  throw new Error('unsupported option');
}

function boundedNumber(value, minimum, maximum) {
  if (!/^[+-]?(?:\d+(?:\.\d*)?|\.\d+)$/.test(value)) throw new Error('invalid number');
  const number = Number(value);
  if (!Number.isFinite(number) || number < minimum || number > maximum) throw new Error('number outside range');
  return String(number);
}

function safeURL(value, image) {
  const raw = String(value || '').trim();
  if (!raw || /[\u0000-\u001f\u007f]/.test(raw) || raw.startsWith('//') || raw.includes('\\')) {
    return { safe: false };
  }
  if (raw.startsWith('#')) return { safe: true, kind: 'fragment' };
  const scheme = /^([A-Za-z][A-Za-z0-9+.-]*):/.exec(raw)?.[1]?.toLowerCase();
  if (scheme) {
    const allowed = image ? ['http', 'https'] : ['http', 'https', 'mailto'];
    return { safe: allowed.includes(scheme), kind: 'absolute' };
  }
  if (raw.startsWith('/')) return { safe: !raw.startsWith('//'), kind: 'root' };
  const pathPart = raw.split(/[?#]/, 1)[0];
  return { safe: Boolean(pathPart), kind: 'relative', path: pathPart };
}

function rewriteHeadingFragment(url, state) {
  let decoded;
  try {
    decoded = decodeURIComponent(url.slice(1));
  } catch {
    addDiagnostic(state, 'markdown.link.fragment_invalid', 'warning',
      'Invalid internal fragment was left inert.', undefined);
    return '#';
  }
  const alias = headingIdentifier(decoded);
  const target = state.headingAliases.get(decoded) || state.headingAliases.get(alias);
  if (!target) {
    addDiagnostic(state, 'markdown.link.fragment_unresolved', 'warning',
      'Internal heading link does not resolve in this page.', undefined);
    return url;
  }
  return `#${target}`;
}

function resolveProjectAssetPath(sourcePath, relativeURL) {
  let decoded;
  try {
    decoded = decodeURIComponent(relativeURL);
  } catch {
    return '';
  }
  const joined = path.normalize(path.join(path.dirname(sourcePath), decoded));
  return canonicalPath(joined) ? joined : '';
}

function resolveProjectPath(sourcePath, relativeURL) {
  let decoded;
  try { decoded = decodeURIComponent(relativeURL); } catch { return ''; }
  const joined = path.normalize(path.join(path.dirname(sourcePath), decoded));
  return canonicalPath(joined) ? joined : '';
}

function isMarkdownPath(value) {
  return /\.(?:md|markdown)$/i.test(String(value || ''));
}

function collectHeadingIndex(source) {
  const tree = unified().use(remarkParse).use(remarkGfm).use(remarkMath).use(remarkDirective).parse(source);
  const used = new Set();
  const index = new Map();
  visit(tree, 'heading', (node) => {
    const text = boundedText(mdastToString(node).replace(/\s+/g, ' ').trim(), 512);
    const base = headingIdentifier(text);
    const id = uniqueIdentifier(base, used);
    if (!index.has(base)) index.set(base, id);
    if (!index.has(text)) index.set(text, id);
    index.set(id, id);
  });
  return index;
}

function appendBookNavigation(fragment, pages, index) {
  const links = [];
  if (index > 0) {
    const previous = pages[index - 1];
    links.push(`<a class="rin-book-navigation-previous" href="${escapeAttribute(previous.sourcePath)}">Previous: ${escapeHTML(previous.title || titleFromPath(previous.sourcePath))}</a>`);
  }
  if (index + 1 < pages.length) {
    const next = pages[index + 1];
    links.push(`<a class="rin-book-navigation-next" href="${escapeAttribute(next.sourcePath)}">Next: ${escapeHTML(next.title || titleFromPath(next.sourcePath))}</a>`);
  }
  if (!links.length) return fragment;
  const navigation = `<aside class="rin-book-navigation" aria-label="Book navigation">${links.join('')}</aside>`;
  return fragment.endsWith('</article>')
    ? `${fragment.slice(0, -'</article>'.length)}${navigation}</article>`
    : `${fragment}${navigation}`;
}

function escapeHTML(value) {
  return String(value || '').replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');
}

function escapeAttribute(value) {
  return escapeHTML(value).replaceAll('"', '&quot;').replaceAll("'", '&#x27;');
}

function isQuiverDiagramURL(value) {
  try {
    return new URL(String(value || ''), 'https://rinspace.invalid').pathname.includes('/api/diagrams/');
  } catch {
    return false;
  }
}

function isGeneratedImageAlt(value) {
  const normalized = String(value || '').trim();
  return !normalized ||
    /^\d+(?:\.\d+)?$/.test(normalized) ||
    /^(?:img|image|figure|fig|pic|photo|screenshot|screen-shot)[-_ ]?\d*$/i.test(normalized) ||
    /^图片[-_ ]?\d*$/.test(normalized) ||
    /^[a-f0-9]{16,}$/i.test(normalized) ||
    /^[\w.-]+\.(?:png|jpe?g|gif|webp|svg|bmp|tiff?|avif)(?:\?.*)?$/i.test(normalized);
}

function fixedHastData(previous, hName, hProperties = {}) {
  return { ...(previous || {}), hName, hProperties };
}

function placeholderNode(id, rinBlockCandidate) {
  return {
    type: 'rinWork',
	data: {
	  ...fixedHastData(undefined, 'rin-work', { 'data-id': id }),
	  ...(rinBlockCandidate ? { rinBlockCandidate } : {}),
	},
    children: [],
  };
}

function newWorkID() {
  return `rw_${randomBytes(16).toString('hex')}`;
}

function sourceLocation(sourcePath, node) {
  const start = node?.position?.start;
  const end = node?.position?.end;
  if (!start?.line || !start?.column) return undefined;
  return {
    path: sourcePath,
    start: { line: start.line, column: start.column },
    ...(end?.line && end?.column ? { end: { line: end.line, column: end.column } } : {}),
  };
}

function sourceSlice(source, node) {
  const start = node?.position?.start?.offset;
  const end = node?.position?.end?.offset;
  if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || start < 0 || end < start) return '';
  return source.slice(start, end);
}

function addDiagnostic(state, code, severity, message, node) {
  if (state.diagnostics.length >= maximumDiagnostics) {
    state.droppedDiagnostics += 1;
    return;
  }
  state.diagnostics.push({
    code,
    severity,
    message: boundedText(message.replace(/[\r\n\t]+/g, ' '), 256),
    stage: 'compile',
    ...(node ? { sourceLocation: sourceLocation(state.sourcePath, node) } : {}),
  });
}

function finishDiagnostics(state) {
  if (state.droppedDiagnostics === 0) return;
  const summary = {
    code: 'markdown.diagnostics.truncated',
    severity: 'warning',
    message: 'Additional Markdown diagnostics were omitted by the count limit.',
    stage: 'compile',
  };
  if (state.diagnostics.length >= maximumDiagnostics) state.diagnostics[maximumDiagnostics - 1] = summary;
  else state.diagnostics.push(summary);
}

function headingIdentifier(value) {
  let slug = String(value || '')
    .normalize('NFKD')
    .replace(/[\u0300-\u036f]/g, '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
  if (!slug) slug = `section-${sha256(String(value || '')).slice(0, 12)}`;
  if (!/^[a-z]/.test(slug) || slug.startsWith('rin-md-')) slug = `section-${slug}`;
  return slug.slice(0, 128).replace(/-+$/g, '') || 'section';
}

function uniqueIdentifier(base, used) {
  let candidate = base;
  let index = 2;
  while (used.has(candidate)) {
    const suffix = `-${index}`;
    candidate = `${base.slice(0, 128 - suffix.length)}${suffix}`;
    index += 1;
  }
  used.add(candidate);
  return candidate;
}

function pageIDFromPath(sourcePath) {
  const stem = path.basename(sourcePath).replace(/\.[^.]+$/, '');
  return headingIdentifier(`page-${stem || sourcePath}`);
}

function titleFromPath(sourcePath) {
  return path.basename(sourcePath).replace(/\.[^.]+$/, '').replace(/[-_]+/g, ' ').trim();
}

function canonicalPath(value) {
  return typeof value === 'string' && value.length > 0 && value === path.normalize(value) &&
    !value.startsWith('/') && !/^[A-Za-z]:/.test(value) && !value.includes('\\') &&
    !value.split('/').includes('..') && !value.includes('\u0000') && value !== '.';
}

function boundedText(value, maximum) {
  return [...String(value || '')].slice(0, maximum).join('');
}

function sha256(value) {
  return createHash('sha256').update(value).digest('hex');
}

function compilerError(code, message) {
  return Object.assign(new Error(message), { code });
}

export function validateDraftBundle(bundle) {
  invariant(bundle?.schemaVersion === DOCUMENT_BUNDLE_SCHEMA_VERSION, 'schema version');
  invariant(bundle.state === 'draft' && bundle.contentKind === 'markdown', 'state/content kind');
  invariant(sha256Pattern.test(bundle.projectHash), 'project hash');
  invariant(bundle.documentEngine === MARKDOWN_ADAPTER_NAME, 'document engine');
  invariant(Array.isArray(bundle.pages) && bundle.pages.length >= 1 && bundle.pages.length <= 2048, 'draft pages');
  invariant(Array.isArray(bundle.workUnits) && Array.isArray(bundle.assets) && Array.isArray(bundle.diagnostics), 'bundle arrays');
  invariant(bundle.provenance?.projectGraphSchemaVersion === PROJECT_GRAPH_SCHEMA_VERSION, 'provenance');

  const workIDs = new Set();
  for (const unit of bundle.workUnits) {
    invariant(workIDPattern.test(unit.id) && !workIDs.has(unit.id), 'opaque unique work id');
    invariant(typeof unit.source === 'string', 'work source');
    invariant(canonicalSourceLocation(unit.sourceLocation), 'work source location');
    if (unit.kind === 'math') {
      invariant(typeof unit.display === 'boolean' && sha256Pattern.test(unit.macroContextHash) && unit.source.trim(), 'math unit');
    } else if (unit.kind === 'diagram') {
      invariant(diagramTypeAliases.has(unit.diagramType) && unit.source.trim(), 'diagram unit');
    } else if (unit.kind === 'code') {
      invariant(!('display' in unit) && !('diagramType' in unit), 'code unit');
    } else {
      invariant(false, 'work kind');
    }
    workIDs.add(unit.id);
  }

  const placeholderCounts = new Map();
  const pageIDs = new Set();
  const pagePaths = new Set();
  for (const page of bundle.pages) {
    invariant(validPageID(page.id) && !pageIDs.has(page.id) && canonicalPath(page.sourcePath) && !pagePaths.has(page.sourcePath), 'page identity');
    pageIDs.add(page.id);
    pagePaths.add(page.sourcePath);
    invariant(page.fragmentFormat === DRAFT_FRAGMENT_FORMAT && typeof page.fragment === 'string', 'draft fragment');
    invariant(Array.isArray(page.toc) && Array.isArray(page.dependencyHashes), 'page metadata');
    invariant(page.dependencyHashes.every((value) => sha256Pattern.test(value)) && new Set(page.dependencyHashes).size === page.dependencyHashes.length, 'dependency hashes');
    invariant(page.toc.every((entry) => identifierPattern.test(entry.id) && entry.depth >= 1 && entry.depth <= 6), 'toc entries');
    invariant(new Set(page.toc.map((entry) => entry.id)).size === page.toc.length, 'toc id uniqueness');
	validateSemanticBlocks(page);
    const stripped = page.fragment.replace(canonicalPlaceholderPattern, (_match, id) => {
      placeholderCounts.set(id, (placeholderCounts.get(id) || 0) + 1);
      return '';
    });
    invariant(!anyWorkElementPattern.test(stripped), 'malformed/source-created work element');
  }
  for (const id of workIDs) invariant(placeholderCounts.get(id) === 1, 'one placeholder per work unit');
  for (const id of placeholderCounts.keys()) invariant(workIDs.has(id), 'placeholder references known work unit');

  const assetIDs = new Set();
  for (const asset of bundle.assets) {
    invariant(identifierPattern.test(asset.id) && !assetIDs.has(asset.id), 'asset id');
    invariant(asset.kind === 'project-file' && sha256Pattern.test(asset.sha256), 'project asset');
    invariant(Number.isSafeInteger(asset.bytes) && asset.bytes >= 0 && asset.mediaType && canonicalPath(asset.projectPath), 'asset metadata');
    assetIDs.add(asset.id);
  }
  invariant(bundle.diagnostics.length <= maximumDiagnostics, 'diagnostic count');
  for (const diagnostic of bundle.diagnostics) {
    invariant(diagnostic.code && ['info', 'warning', 'error'].includes(diagnostic.severity) && diagnostic.stage === 'compile', 'diagnostic');
    if (diagnostic.sourceLocation) invariant(canonicalSourceLocation(diagnostic.sourceLocation), 'diagnostic source location');
  }
  return bundle;
}

function validateSemanticBlocks(page) {
  invariant(Array.isArray(page.blocks) && page.blocks.length > 0, 'semantic blocks');
  const ids = new Set();
  for (const block of page.blocks) {
    invariant(blockIDPattern.test(block?.id) && !ids.has(block.id), 'semantic block id');
    invariant(blockKinds.has(block.kind), 'semantic block kind');
    invariant(typeof block.text === 'string' && block.text === normalizeBlockText(block.text), 'semantic block text');
    invariant(block.textHash === sha256(block.text), 'semantic block text hash');
    invariant(Array.isArray(block.headingPath) && block.headingPath.every((id) => identifierPattern.test(id)), 'semantic block heading path');
    if (block.sourceLocation) invariant(canonicalSourceLocation(block.sourceLocation), 'semantic block source location');
    ids.add(block.id);
  }
  const matches = [...page.fragment.matchAll(/<[A-Za-z][^>]*\bdata-rin-block-id="(rb_[a-f0-9]{32})"[^>]*>/g)];
  invariant(matches.length === page.blocks.length, 'semantic block DOM count');
  const domCounts = new Map();
  for (const match of matches) domCounts.set(match[1], (domCounts.get(match[1]) || 0) + 1);
  for (const id of ids) invariant(domCounts.get(id) === 1, 'semantic block DOM identity');
	const actualTags = page.fragment.match(/<[A-Za-z][^>]*>/g) || [];
	invariant(actualTags.every((tag) => !/\bdata-rin-block-id\b/i.test(tag) ||
	  /\bdata-rin-block-id="rb_[a-f0-9]{32}"/.test(tag)), 'malformed semantic block DOM identity');
}

export function validPageID(value) {
  return typeof value === 'string' && pageIdentifierPattern.test(value);
}

function canonicalSourceLocation(location) {
  if (!location || !canonicalPath(location.path)) return false;
  const start = location.start;
  const end = location.end;
  if (!Number.isSafeInteger(start?.line) || start.line < 1 || !Number.isSafeInteger(start?.column) || start.column < 1) return false;
  if (!end) return true;
  if (!Number.isSafeInteger(end.line) || end.line < 1 || !Number.isSafeInteger(end.column) || end.column < 1) return false;
  return end.line > start.line || end.line === start.line && end.column >= start.column;
}

function invariant(condition, label) {
  if (!condition) throw compilerError('markdown.bundle.invalid', `invalid Markdown draft bundle: ${label}`);
}
