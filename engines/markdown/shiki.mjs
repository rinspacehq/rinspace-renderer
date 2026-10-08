import { createHighlighterCore } from 'shiki/core';
import { createJavaScriptRegexEngine } from 'shiki/engine/javascript';
import bash from 'shiki/langs/bash.mjs';
import c from 'shiki/langs/c.mjs';
import cpp from 'shiki/langs/cpp.mjs';
import csharp from 'shiki/langs/csharp.mjs';
import css from 'shiki/langs/css.mjs';
import go from 'shiki/langs/go.mjs';
import html from 'shiki/langs/html.mjs';
import java from 'shiki/langs/java.mjs';
import javascript from 'shiki/langs/javascript.mjs';
import json from 'shiki/langs/json.mjs';
import jsx from 'shiki/langs/jsx.mjs';
import kotlin from 'shiki/langs/kotlin.mjs';
import latex from 'shiki/langs/latex.mjs';
import lua from 'shiki/langs/lua.mjs';
import markdown from 'shiki/langs/markdown.mjs';
import objectiveC from 'shiki/langs/objective-c.mjs';
import objectiveCpp from 'shiki/langs/objective-cpp.mjs';
import php from 'shiki/langs/php.mjs';
import python from 'shiki/langs/python.mjs';
import ruby from 'shiki/langs/ruby.mjs';
import rust from 'shiki/langs/rust.mjs';
import scss from 'shiki/langs/scss.mjs';
import shellscript from 'shiki/langs/shellscript.mjs';
import sql from 'shiki/langs/sql.mjs';
import tex from 'shiki/langs/tex.mjs';
import toml from 'shiki/langs/toml.mjs';
import tsx from 'shiki/langs/tsx.mjs';
import typescript from 'shiki/langs/typescript.mjs';
import xml from 'shiki/langs/xml.mjs';
import yaml from 'shiki/langs/yaml.mjs';
import githubLight from 'shiki/themes/github-light.mjs';

export const SHIKI_BATCH_CONTRACT = 'rin-shiki-batch/v1';
export const SHIKI_RESULT_CONTRACT = 'rin-shiki-result/v1';
export const APPROVED_SHIKI_THEME = 'github-light';
export const APPROVED_SHIKI_LANGUAGES = Object.freeze([
  'bash', 'c', 'cpp', 'csharp', 'css', 'go', 'html', 'java', 'javascript', 'json', 'jsx',
  'kotlin', 'latex', 'lua', 'markdown', 'objective-c', 'objective-cpp', 'php', 'python',
  'ruby', 'rust', 'scss', 'shellscript', 'sql', 'tex', 'toml', 'tsx', 'typescript', 'xml', 'yaml',
]);
export const MAX_SHIKI_BATCH_ITEMS = 512;
export const MAX_SHIKI_SOURCE_BYTES = 512 * 1024;

const grammars = [
  bash, c, cpp, csharp, css, go, html, java, javascript, json, jsx, kotlin, latex, lua,
  markdown, objectiveC, objectiveCpp, php, python, ruby, rust, scss, shellscript, sql,
  tex, toml, tsx, typescript, xml, yaml,
];
const aliases = new Map(Object.entries({
  bash: 'bash', shell: 'shellscript', sh: 'shellscript', zsh: 'shellscript', console: 'shellscript',
  shellscript: 'shellscript', c: 'c', h: 'c', cpp: 'cpp', 'c++': 'cpp', cplusplus: 'cpp',
  cxx: 'cpp', hpp: 'cpp', cs: 'csharp', 'c#': 'csharp', csharp: 'csharp', css: 'css',
  scss: 'scss', go: 'go', golang: 'go', html: 'html', java: 'java', js: 'javascript',
  javascript: 'javascript', json: 'json', jsx: 'jsx', kotlin: 'kotlin', kt: 'kotlin',
  latex: 'latex', tex: 'tex', lua: 'lua', md: 'markdown', markdown: 'markdown',
  m: 'objective-c', objc: 'objective-c', 'obj-c': 'objective-c', objectivec: 'objective-c',
  'objective-c': 'objective-c', mm: 'objective-cpp', 'objective-cpp': 'objective-cpp',
  php: 'php', py: 'python', python: 'python', rb: 'ruby', ruby: 'ruby', rust: 'rust',
  rs: 'rust', sql: 'sql', toml: 'toml', ts: 'typescript', typescript: 'typescript',
  tsx: 'tsx', xml: 'xml', yaml: 'yaml', yml: 'yaml', text: '', txt: '', plain: '',
}));

const highlighterPromise = createHighlighterCore({
  themes: [githubLight],
  langs: grammars,
  engine: createJavaScriptRegexEngine(),
});

export async function assertShikiReady() {
  const highlighter = await highlighterPromise;
  if (!highlighter.getLoadedThemes().includes(APPROVED_SHIKI_THEME)) {
    throw new Error('approved Shiki theme failed to load');
  }
  const loaded = new Set(highlighter.getLoadedLanguages());
  for (const language of APPROVED_SHIKI_LANGUAGES) {
    if (!loaded.has(language)) throw new Error(`approved Shiki language failed to load: ${language}`);
  }
}

export async function resolveShikiBatch(payload) {
  const batch = normalizeBatch(payload);
  const highlighter = await highlighterPromise;
  return {
    contractVersion: SHIKI_RESULT_CONTRACT,
    engine: 'shiki',
    engineVersion: '4.4.2',
    theme: APPROVED_SHIKI_THEME,
    items: batch.items.map((item) => renderItem(highlighter, item)),
  };
}

function normalizeBatch(payload) {
  if (!payload || payload.contractVersion !== SHIKI_BATCH_CONTRACT ||
      payload.theme !== APPROVED_SHIKI_THEME || !Array.isArray(payload.items) ||
      payload.items.length < 1 || payload.items.length > MAX_SHIKI_BATCH_ITEMS) {
    throw shikiError('shiki.batch.invalid', 'Shiki batch contract is invalid');
  }
  const seen = new Set();
  const items = payload.items.map((input) => {
    const id = typeof input?.id === 'string' ? input.id : '';
    const source = typeof input?.source === 'string' ? input.source : '';
    const language = typeof input?.language === 'string' ? input.language.slice(0, 128) : '';
    if (!/^rw_[a-f0-9]{32}$/.test(id) || seen.has(id)) {
      throw shikiError('shiki.batch.invalid', 'Shiki batch contains an invalid or duplicate id');
    }
    if (Buffer.byteLength(source, 'utf8') > MAX_SHIKI_SOURCE_BYTES) {
      throw shikiError('shiki.source.too_large', 'Shiki source exceeds byte limit');
    }
    seen.add(id);
    return { id, source, language };
  });
  return { items };
}

function renderItem(highlighter, item) {
  const canonicalLanguage = normalizeLanguage(item.language);
  if (!canonicalLanguage) {
    const unknown = item.language.trim();
    return {
      id: item.id,
      html: plainCodeHTML(item.source, unknown),
      canonicalLanguage: '',
      highlighted: false,
      diagnostics: unknown ? [{
        code: 'code.language.unknown',
        severity: 'warning',
        message: 'Unknown or disabled code language; escaped plain code was used.',
      }] : [],
    };
  }
  return {
    id: item.id,
    html: highlighter.codeToHtml(item.source, {
      lang: canonicalLanguage,
      theme: APPROVED_SHIKI_THEME,
    }),
    canonicalLanguage,
    highlighted: true,
    diagnostics: [],
  };
}

function normalizeLanguage(value) {
  const token = String(value || '')
    .trim()
    .replace(/^language-/i, '')
    .replace(/^\{?\.?/, '')
    .replace(/\}?$/, '')
    .toLowerCase();
  return aliases.get(token) || '';
}

function plainCodeHTML(source, language) {
  const label = language || 'text';
  return `<pre class="rin-code-pre rin-code-plain" data-rin-code-language="${escapeAttribute(label)}"><code>${escapeHTML(source)}</code></pre>`;
}

function escapeHTML(value) {
  return String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');
}

function escapeAttribute(value) {
  return escapeHTML(value).replaceAll('"', '&quot;').replaceAll("'", '&#39;');
}

function shikiError(code, message) {
  return Object.assign(new Error(message), { code });
}
