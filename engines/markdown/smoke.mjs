import { execFileSync } from 'node:child_process';
import process from 'node:process';

const workerContract = 'rin-node-worker/v1';
const source = `# Worker smoke

| feature | ready |
| --- | --- |
| GFM | yes |

Inline math $\\not\\exists x$ and ~~strikethrough~~.

:::note[Directive probe]
safe body
:::

\`\`\`javascript
const answer = 42
\`\`\`

<script>globalThis.rinUnsafe = true</script>
`;

const requests = [
  request('health', 'health', {}),
  request('compile', 'markdown.smoke', { source }),
  request('unsupported', 'markdown.publish', {}),
  {
    ...request('deadline', 'health', {}),
    deadlineUnixMs: 1,
  },
];
const responses = runWorker(requests);
if (responses.length !== requests.length) {
  throw new Error(`expected ${requests.length} worker responses, received ${responses.length}`);
}
responses.forEach((response, index) => {
  if (response.contractVersion !== workerContract || response.id !== requests[index].id) {
    throw new Error('worker did not preserve contract version and request correlation');
  }
  if (response.metrics?.tasks !== index + 1 || !response.metrics?.rssBytes) {
    throw new Error('worker task/RSS metrics are missing or inconsistent');
  }
});

const health = responses[0];
if (!health.ok || !health.result?.ready || health.result.engine !== 'unified-markdown') {
  throw new Error('Markdown worker health failed');
}
if (health.result.engineVersion !== '0.6.0' || health.result.shiki?.version !== '4.4.2') {
  throw new Error('Markdown worker reports unexpected engine or Shiki version');
}
if (!/^rin-markdown-pipeline\/v2\+deps\.sha256\.[a-f0-9]{64}$/.test(health.result.pipelineVersion)) {
  throw new Error(`invalid deterministic pipeline version: ${health.result.pipelineVersion}`);
}
for (const dependency of [
  'unified',
  'remark-parse',
  'remark-gfm',
  'remark-math',
  'remark-directive',
  'remark-rehype',
  'rehype-sanitize',
  'rehype-stringify',
  'shiki',
  'unist-util-visit',
  'unist-util-visit-parents',
]) {
  if (!health.result.dependencies?.[dependency]) {
    throw new Error(`capabilities omit pinned dependency ${dependency}`);
  }
}
if (
  health.result.policy?.rawHtml !== 'disabled' ||
  health.result.policy?.mdx !== 'disabled' ||
  health.result.policy?.publishableSmokeOutput !== false
) {
  throw new Error('bootstrap safety policy is not explicit');
}

const compile = responses[1];
if (!compile.ok || compile.result?.publishable !== false) {
  throw new Error('non-publishable Markdown smoke failed');
}
for (const expected of ['<table>', '<del>strikethrough</del>', '<code class="language-math">', '<pre><code class="language-javascript">']) {
  if (!compile.result.html.includes(expected)) {
    throw new Error(`Markdown smoke HTML omits ${expected}`);
  }
}
for (const forbidden of ['<script', 'globalThis.rinUnsafe', 'onclick=']) {
  if (compile.result.html.includes(forbidden)) {
    throw new Error(`raw HTML policy leaked ${forbidden}`);
  }
}
if (
  compile.result.astSummary?.counts?.inlineMath !== 1 ||
  compile.result.astSummary?.counts?.containerDirective !== 1 ||
  compile.result.astSummary?.counts?.code !== 1 ||
  compile.result.astSummary?.directiveNames?.[0] !== 'note'
) {
  throw new Error('remark math/directive/code nodes were not observed before HAST conversion');
}
if (!compile.result.shikiProbe?.containsTokenMarkup || !/^[a-f0-9]{64}$/.test(compile.result.shikiProbe?.sha256 || '')) {
  throw new Error('Shiki probe did not produce deterministic token markup');
}
if (compile.result.capabilities?.pipelineVersion !== health.result.pipelineVersion) {
  throw new Error('health and work responses disagree on pipeline version');
}
if (responses[2].ok || responses[2].error?.code !== 'nodeworker.operation.unsupported') {
  throw new Error('unsupported operation did not produce a structured error');
}
if (responses[3].ok || responses[3].error?.code !== 'nodeworker.deadline.exceeded') {
  throw new Error('expired deadline did not produce a structured error');
}

const sourceBound = runWorker([
  request('source-bound', 'markdown.smoke', { source: '123456789' }),
], { RIN_MARKDOWN_MAX_SOURCE_BYTES: '8' })[0];
if (sourceBound.ok || sourceBound.error?.code !== 'markdown.source.too_large') {
  throw new Error('source byte limit was not enforced');
}

const responseBound = runWorker([
  request('response-bound', 'markdown.smoke', { source }),
], { RIN_NODE_WORKER_MAX_RESPONSE_BYTES: '512' })[0];
if (responseBound.ok || responseBound.error?.code !== 'nodeworker.response.too_large') {
  throw new Error('response byte limit was not enforced');
}

console.log(`Markdown worker smoke ok: ${health.result.pipelineVersion}; bounded NDJSON and Shiki ready`);

function request(id, operation, payload) {
  return { contractVersion: workerContract, id, operation, payload };
}

function runWorker(messages, extraEnvironment = {}) {
  const stdout = execFileSync(process.execPath, ['worker.mjs', '--ndjson-worker'], {
    cwd: new URL('.', import.meta.url),
    input: `${messages.map((message) => JSON.stringify(message)).join('\n')}\n`,
    encoding: 'utf8',
    maxBuffer: 16 * 1024 * 1024,
    env: { ...process.env, ...extraEnvironment },
  });
  return stdout.trim().split('\n').filter(Boolean).map((line) => JSON.parse(line));
}
