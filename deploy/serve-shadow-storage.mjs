#!/usr/bin/env node
import crypto from 'node:crypto';
import http from 'node:http';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const maxObjectBytes = 4 * 1024 * 1024;
const maxTotalBytes = 64 * 1024 * 1024;
const objectPattern = /^diagrams\/v1\/svg-sha256\/([0-9a-f]{2})\/([0-9a-f]{64})\.svg$/;

export function createShadowStorage({ token }) {
  if (!token) throw new Error('shadow storage requires a token');
  const objects = new Map();
  let totalBytes = 0;
  return http.createServer(async (request, response) => {
    if (request.method === 'GET' && request.url === '/health') {
      return json(response, 200, { status: 'ok', objects: objects.size, bytes: totalBytes });
    }
    const target = storageTarget(request.url || '');
    if (!target) return json(response, 404, { error: 'not found' });
    if (request.method === 'HEAD' || request.method === 'GET') {
      const body = objects.get(target.objectID);
      if (!body) return json(response, 404, { error: 'not found' });
      response.statusCode = 200;
      response.setHeader('Content-Type', 'image/svg+xml; charset=utf-8');
      response.setHeader('Content-Length', String(body.length));
      return response.end(request.method === 'HEAD' ? undefined : body);
    }
    if (request.method !== 'POST' || target.public) return json(response, 405, { error: 'method not allowed' });
    if (request.headers.authorization !== `Bearer ${token}` || request.headers['x-upsert'] !== 'true') {
      return json(response, 401, { error: 'unauthorized' });
    }
    let body;
    try {
      body = await readBounded(request, maxObjectBytes);
    } catch (error) {
      return json(response, 413, { error: error.message });
    }
    const match = target.objectID.match(objectPattern);
    const digest = crypto.createHash('sha256').update(body).digest('hex');
    if (!match || match[1] !== digest.slice(0, 2) || match[2] !== digest || !body.toString('utf8').startsWith('<svg')) {
      return json(response, 400, { error: 'invalid content-addressed SVG' });
    }
    const existing = objects.get(target.objectID);
    if (existing && !existing.equals(body)) return json(response, 409, { error: 'immutable object conflict' });
    if (!existing) {
      if (totalBytes + body.length > maxTotalBytes) return json(response, 507, { error: 'shadow storage capacity exceeded' });
      objects.set(target.objectID, body);
      totalBytes += body.length;
    }
    return json(response, 200, { Id: 'shadow-object', Key: `rin-renderer/${target.objectID}` });
  });
}

function storageTarget(rawURL) {
  let segments;
  try {
    segments = new URL(rawURL, 'http://shadow.invalid').pathname.split('/').filter(Boolean).map(decodeURIComponent);
  } catch {
    return null;
  }
  if (segments[0] !== 'v1' || segments[1] !== 'storages' || segments[2] !== 'object') return null;
  let index = 3;
  let isPublic = false;
  if (segments[index] === 'public') {
    isPublic = true;
    index++;
  }
  if (segments[index] !== 'rin-renderer') return null;
  const objectID = segments.slice(index + 1).join('/');
  if (!objectPattern.test(objectID)) return null;
  return { objectID, public: isPublic };
}

async function readBounded(stream, limit) {
  const chunks = [];
  let bytes = 0;
  for await (const chunk of stream) {
    bytes += chunk.length;
    if (bytes > limit) throw new Error('object too large');
    chunks.push(chunk);
  }
  return Buffer.concat(chunks);
}

function json(response, status, body) {
  const encoded = Buffer.from(JSON.stringify(body));
  response.statusCode = status;
  response.setHeader('Content-Type', 'application/json');
  response.setHeader('Content-Length', String(encoded.length));
  response.end(encoded);
}

function parseArgs(args) {
  const result = {};
  for (let index = 0; index < args.length; index += 2) {
    if (!args[index]?.startsWith('--') || !args[index + 1]) throw new Error('usage: serve-shadow-storage.mjs --addr HOST:PORT --token TOKEN');
    result[args[index].slice(2)] = args[index + 1];
  }
  return result;
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const match = String(args.addr || '').match(/^([^:]+):(\d+)$/);
  if (!match || Number(match[2]) < 1 || Number(match[2]) > 65535 || !args.token) {
    throw new Error('usage: serve-shadow-storage.mjs --addr HOST:PORT --token TOKEN');
  }
  const server = createShadowStorage({ token: args.token });
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(Number(match[2]), match[1], resolve);
  });
  console.log(`shadow storage listening on ${args.addr}`);
}

if (process.argv[1] && fileURLToPath(import.meta.url) === path.resolve(process.argv[1])) {
  main().catch((error) => {
    console.error(error instanceof Error ? error.stack || error.message : String(error));
    process.exit(1);
  });
}
