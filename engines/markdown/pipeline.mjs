import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

import rehypeSanitize from 'rehype-sanitize';
import rehypeStringify from 'rehype-stringify';
import remarkDirective from 'remark-directive';
import remarkGfm from 'remark-gfm';
import remarkMath from 'remark-math';
import remarkParse from 'remark-parse';
import remarkRehype from 'remark-rehype';
import { unified } from 'unified';
import { visit } from 'unist-util-visit';
import { visitParents } from 'unist-util-visit-parents';
import { VFile } from 'vfile';

import {
  SAFE_ADMONITIONS,
  compileMarkdownBookDraft,
  compileMarkdownDraft,
} from './compiler.mjs';
import { MARKDOWN_SANITIZER_VERSION, finalizeMarkdownBundle, planMarkdownBookCache } from './finalizer.mjs';
import {
  APPROVED_SHIKI_LANGUAGES,
  APPROVED_SHIKI_THEME,
  assertShikiReady,
  resolveShikiBatch,
} from './shiki.mjs';

export const WORKER_CONTRACT_VERSION = 'rin-node-worker/v1';
export const PIPELINE_CONTRACT_VERSION = 'rin-markdown-pipeline/v2';
export const ENGINE_NAME = 'unified-markdown';
export { APPROVED_SHIKI_THEME };
export const SMOKE_SHIKI_LANGUAGES = APPROVED_SHIKI_LANGUAGES;
export const RIN_MDAST_PLUGINS = Object.freeze([
  'stable-heading-ids',
  'safe-directives-admonitions',
  'safe-links-images-figures',
  'footnote-prefixing',
  'exact-math-code-diagram-work-units',
  'raw-html-compatibility',
	'stable-semantic-blocks',
]);
export const DEFAULT_MAX_REQUEST_BYTES = 4 * 1024 * 1024;
export const DEFAULT_MAX_RESPONSE_BYTES = 8 * 1024 * 1024;
export const DEFAULT_MAX_SOURCE_BYTES = 2 * 1024 * 1024;

const packageDirectory = new URL('.', import.meta.url);
const packageJSON = JSON.parse(await readFile(new URL('package.json', packageDirectory), 'utf8'));
const packageLock = JSON.parse(await readFile(new URL('package-lock.json', packageDirectory), 'utf8'));
const dependencyVersions = validateAndReadDependencyVersions(packageJSON, packageLock);
const dependencyGraphHash = hashCanonicalLockGraph(packageLock);

const pluginOrder = Object.freeze([
  ['parse', 'remark-parse'],
  ['mdast', 'remark-gfm'],
  ['mdast', 'remark-math'],
  ['mdast', 'remark-directive'],
  ['mdast-to-hast', 'remark-rehype'],
  ['bootstrap-sanitize', 'rehype-sanitize'],
  ['serialize', 'rehype-stringify'],
]);

export function pipelineCapabilities(limits = {}) {
  return {
    engine: ENGINE_NAME,
    engineVersion: packageJSON.version,
    workerContractVersion: WORKER_CONTRACT_VERSION,
    pipelineContractVersion: PIPELINE_CONTRACT_VERSION,
    pipelineVersion: `${PIPELINE_CONTRACT_VERSION}+deps.sha256.${dependencyGraphHash}`,
    nodeRange: packageJSON.engines.node,
    operations: ['health', 'markdown.smoke', 'markdown.compile-draft', 'markdown.compile-book-draft', 'markdown.plan-book-cache', 'markdown.finalize', 'shiki.render-batch'],
    pipeline: pluginOrder.map(([stage, dependency]) => ({
      stage,
      dependency,
      version: dependencyVersions[dependency],
    })),
    dependencies: dependencyVersions,
    rinMdastPlugins: [...RIN_MDAST_PLUGINS],
    directives: {
      admonitions: [...SAFE_ADMONITIONS],
      executable: false,
    },
    shiki: {
      version: dependencyVersions.shiki,
      engine: 'javascript-regexp',
      themes: [APPROVED_SHIKI_THEME],
      languages: [...SMOKE_SHIKI_LANGUAGES],
    },
    policy: {
      rawHtml: 'disabled',
      mdx: 'disabled',
      executableExtensions: 'disabled',
      publishableSmokeOutput: false,
      publishableDraftOutput: false,
      publishableFinalOutput: true,
      sanitizerVersion: MARKDOWN_SANITIZER_VERSION,
    },
    limits: {
      maxRequestBytes: limits.maxRequestBytes ?? DEFAULT_MAX_REQUEST_BYTES,
      maxResponseBytes: limits.maxResponseBytes ?? DEFAULT_MAX_RESPONSE_BYTES,
      maxSourceBytes: limits.maxSourceBytes ?? DEFAULT_MAX_SOURCE_BYTES,
    },
  };
}

export async function assertRuntimeReady() {
  await assertShikiReady();
}

export async function runMarkdownSmoke(source) {
  const file = new VFile({ value: source });
  const rendered = await createBootstrapProcessor().process(file);
  const shikiBatch = await resolveShikiBatch({
    contractVersion: 'rin-shiki-batch/v1', theme: APPROVED_SHIKI_THEME,
    items: [{ id: 'rw_00000000000000000000000000000000', source: 'const answer = 42', language: 'javascript' }],
  });
  const shikiProbe = shikiBatch.items[0].html;
  return {
    publishable: false,
    html: String(rendered),
    astSummary: file.data.rinAstSummary,
    shikiProbe: {
      language: 'javascript',
      theme: APPROVED_SHIKI_THEME,
      sha256: createHash('sha256').update(shikiProbe).digest('hex'),
      containsTokenMarkup: shikiProbe.includes('<span style='),
    },
  };
}

export async function runMarkdownDraft(payload) {
  return compileMarkdownDraft(payload, {
    engineVersion: pipelineCapabilities().pipelineVersion,
    adapterVersion: packageJSON.version,
  });
}

export async function runMarkdownBookDraft(payload) {
  return compileMarkdownBookDraft(payload, {
    engineVersion: pipelineCapabilities().pipelineVersion,
    adapterVersion: packageJSON.version,
  });
}

export async function runMarkdownFinalize(payload) {
  return finalizeMarkdownBundle(payload, {
    engineVersion: pipelineCapabilities().pipelineVersion,
    adapterVersion: packageJSON.version,
  });
}

export function runMarkdownBookCachePlan(payload) {
  return planMarkdownBookCache(payload);
}

function createBootstrapProcessor() {
  return unified()
    .use(remarkParse)
    .use(remarkGfm)
    .use(remarkMath)
    .use(remarkDirective)
    .use(observeBootstrapTree)
    .use(remarkRehype, { allowDangerousHtml: false })
    .use(rehypeSanitize)
    .use(rehypeStringify);
}

function observeBootstrapTree() {
  return (tree, file) => {
    const counts = {};
    const directiveNames = new Set();
    const codeParentTypes = [];
    visit(tree, (node) => {
      counts[node.type] = (counts[node.type] || 0) + 1;
      if (node.type.endsWith('Directive') && typeof node.name === 'string') {
        directiveNames.add(node.name);
      }
    });
    visitParents(tree, 'code', (_node, parents) => {
      codeParentTypes.push(parents.map((parent) => parent.type).join('/'));
    });
    file.data.rinAstSummary = {
      counts,
      directiveNames: [...directiveNames].sort(),
      codeParentTypes,
    };
  };
}

function validateAndReadDependencyVersions(manifest, lock) {
  const root = lock.packages?.[''];
  if (lock.lockfileVersion !== 3 || !root || root.name !== manifest.name) {
    throw new Error('Markdown package lock root is invalid');
  }
  const versions = {};
  for (const [name, requested] of Object.entries(manifest.dependencies || {}).sort(([a], [b]) => a.localeCompare(b))) {
    if (!/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(requested)) {
      throw new Error(`dependency ${name} is not pinned exactly`);
    }
    if (root.dependencies?.[name] !== requested) {
      throw new Error(`package lock root does not pin ${name} consistently`);
    }
    const locked = lock.packages?.[`node_modules/${name}`];
    if (!locked || locked.version !== requested || typeof locked.integrity !== 'string') {
      throw new Error(`package lock entry for ${name} is incomplete`);
    }
    versions[name] = locked.version;
  }
  return Object.freeze(versions);
}

function hashCanonicalLockGraph(lock) {
  const graph = {
    lockfileVersion: lock.lockfileVersion,
    requires: lock.requires,
    packages: lock.packages,
  };
  return createHash('sha256').update(JSON.stringify(canonicalize(graph))).digest('hex');
}

function canonicalize(value) {
  if (Array.isArray(value)) return value.map(canonicalize);
  if (value && typeof value === 'object') {
    return Object.fromEntries(
      Object.entries(value)
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([key, child]) => [key, canonicalize(child)]),
    );
  }
  return value;
}

export const packagePath = fileURLToPath(packageDirectory);
