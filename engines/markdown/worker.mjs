#!/usr/bin/env node
import process from 'node:process';

import {
  DEFAULT_MAX_REQUEST_BYTES,
  DEFAULT_MAX_RESPONSE_BYTES,
  DEFAULT_MAX_SOURCE_BYTES,
  WORKER_CONTRACT_VERSION,
  assertRuntimeReady,
  pipelineCapabilities,
  runMarkdownBookCachePlan,
  runMarkdownBookDraft,
  runMarkdownDraft,
  runMarkdownFinalize,
  runMarkdownSmoke,
} from './pipeline.mjs';
import { resolveShikiBatch } from './shiki.mjs';

const maxRequestBytes = boundedPositiveInteger(
  process.env.RIN_NODE_WORKER_MAX_REQUEST_BYTES,
  DEFAULT_MAX_REQUEST_BYTES,
  256,
);
const maxResponseBytes = boundedPositiveInteger(
  process.env.RIN_NODE_WORKER_MAX_RESPONSE_BYTES,
  DEFAULT_MAX_RESPONSE_BYTES,
  256,
);
const maxSourceBytes = boundedPositiveInteger(
  process.env.RIN_MARKDOWN_MAX_SOURCE_BYTES,
  DEFAULT_MAX_SOURCE_BYTES,
  1,
);
const limits = { maxRequestBytes, maxResponseBytes, maxSourceBytes };

if (process.argv.includes('--ndjson-worker')) {
  await runNDJSONWorker();
} else {
  await runOneShot();
}

async function runOneShot() {
  try {
    const input = await readBoundedStdin();
    const request = JSON.parse(input || '{}');
    const response = await handleWorkerRequest(request);
    writeBoundedResponse(response);
    if (!response.ok) process.exitCode = 2;
  } catch (error) {
    writeBoundedResponse(failureEnvelope('', error));
    process.exitCode = 2;
  }
}

async function runNDJSONWorker() {
  process.stdin.setEncoding('utf8');
  let buffered = '';
  let tasks = 0;

  for await (const chunk of process.stdin) {
    buffered += chunk;
    if (Buffer.byteLength(buffered, 'utf8') > maxRequestBytes && !buffered.includes('\n')) {
      throw workerError('nodeworker.request.too_large', 'node worker request exceeds byte limit');
    }
    while (true) {
      const newline = buffered.indexOf('\n');
      if (newline < 0) break;
      const line = buffered.slice(0, newline);
      buffered = buffered.slice(newline + 1);
      if (!line.trim()) continue;
      if (Buffer.byteLength(line, 'utf8') > maxRequestBytes) {
        throw workerError('nodeworker.request.too_large', 'node worker request exceeds byte limit');
      }
      const response = await handleWorkerLine(line);
      tasks += 1;
      response.metrics = { tasks, rssBytes: process.memoryUsage().rss };
      writeBoundedResponse(response);
    }
  }
  if (buffered.trim()) {
    throw workerError('nodeworker.ndjson.unterminated', 'node worker received unterminated NDJSON');
  }
}

async function handleWorkerLine(line) {
  let request;
  try {
    request = JSON.parse(line);
  } catch {
    return failureEnvelope('', workerError('nodeworker.json.invalid', 'invalid worker JSON'));
  }
  return handleWorkerRequest(request);
}

async function handleWorkerRequest(request) {
  const id = validRequestID(request?.id) ? request.id : '';
  const base = { contractVersion: WORKER_CONTRACT_VERSION, id };
  if (request?.contractVersion !== WORKER_CONTRACT_VERSION || !id) {
    return {
      ...base,
      ok: false,
      error: {
        code: 'nodeworker.contract.invalid',
        message: 'invalid worker contract or request id',
      },
    };
  }
  if (Number.isFinite(request.deadlineUnixMs) && Date.now() >= request.deadlineUnixMs) {
    return {
      ...base,
      ok: false,
      error: {
        code: 'nodeworker.deadline.exceeded',
        message: 'request deadline expired before execution',
      },
    };
  }
  try {
    if (request.operation === 'health') {
      await assertRuntimeReady();
      return { ...base, ok: true, result: { ready: true, ...pipelineCapabilities(limits) } };
    }
    if (request.operation === 'shiki.render-batch') {
      return { ...base, ok: true, result: await resolveShikiBatch(request.payload) };
    }
    if (request.operation === 'markdown.finalize') {
      const result = await runMarkdownFinalize(request.payload);
      return { ...base, ok: true, result: { ...result, capabilities: pipelineCapabilities(limits) } };
    }
    if (request.operation === 'markdown.plan-book-cache') {
      return { ...base, ok: true, result: runMarkdownBookCachePlan(request.payload) };
    }
    if (request.operation !== 'markdown.smoke' && request.operation !== 'markdown.compile-draft' &&
        request.operation !== 'markdown.compile-book-draft') {
      throw workerError('nodeworker.operation.unsupported', 'unsupported worker operation');
    }
    const sources = request.operation === 'markdown.compile-book-draft'
      ? (Array.isArray(request.payload?.pages) ? request.payload.pages.map((page) => page?.source) : [])
      : [request.payload?.source];
    if (!sources.length || sources.some((source) => typeof source !== 'string' || !source)) {
      throw workerError('markdown.source.empty', 'Markdown source is empty');
    }
    if (sources.some((source) => Buffer.byteLength(source, 'utf8') > maxSourceBytes)) {
      throw workerError('markdown.source.too_large', 'Markdown source exceeds byte limit');
    }
    const result = request.operation === 'markdown.compile-draft'
      ? await runMarkdownDraft(request.payload)
      : request.operation === 'markdown.compile-book-draft'
        ? await runMarkdownBookDraft(request.payload)
        : await runMarkdownSmoke(sources[0]);
    return {
      ...base,
      ok: true,
      result: {
        ...result,
        capabilities: pipelineCapabilities(limits),
      },
    };
  } catch (error) {
    return failureEnvelope(id, error);
  }
}

async function readBoundedStdin() {
  let body = '';
  process.stdin.setEncoding('utf8');
  for await (const chunk of process.stdin) {
    body += chunk;
    if (Buffer.byteLength(body, 'utf8') > maxRequestBytes) {
      throw workerError('nodeworker.request.too_large', 'node worker request exceeds byte limit');
    }
  }
  return body;
}

function writeBoundedResponse(response) {
  let serialized = JSON.stringify(response);
  if (Buffer.byteLength(serialized, 'utf8') > maxResponseBytes) {
    serialized = JSON.stringify({
      contractVersion: WORKER_CONTRACT_VERSION,
      id: validRequestID(response?.id) ? response.id : '',
      ok: false,
      error: {
        code: 'nodeworker.response.too_large',
        message: 'node worker response exceeds byte limit',
      },
      metrics: response?.metrics,
    });
  }
  process.stdout.write(`${serialized}\n`);
}

function failureEnvelope(id, error) {
  return {
    contractVersion: WORKER_CONTRACT_VERSION,
    id: validRequestID(id) ? id : '',
    ok: false,
    error: {
      code: safeCode(error?.code),
      message: safeMessage(error?.message),
    },
  };
}

function workerError(code, message) {
  return Object.assign(new Error(message), { code });
}

function validRequestID(value) {
  return typeof value === 'string' && value.length > 0 && value.length <= 128 && /^[A-Za-z0-9._:-]+$/.test(value);
}

function safeCode(value) {
  return typeof value === 'string' && /^[a-z0-9._-]{1,96}$/.test(value)
    ? value
    : 'markdown.worker.failed';
}

function safeMessage(value) {
  const message = typeof value === 'string' ? value : 'Markdown worker failed';
  return message.replace(/[\r\n\t]+/g, ' ').slice(0, 256);
}

function boundedPositiveInteger(value, fallback, minimum) {
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) && parsed >= minimum ? parsed : fallback;
}
