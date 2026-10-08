#!/usr/bin/env node
import { createHash } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';

export function verifyNoticeLock(lock) {
  if (lock?.schemaVersion !== 'rin-renderer-notice-sources/v1' || !Array.isArray(lock.sources) || lock.sources.length !== 5) {
    throw new Error('invalid notice source lock');
  }
  const seen = new Set();
  for (const item of lock.sources) {
    if (!/^[a-z0-9-]+-LICENSE$/.test(item.name) || seen.has(item.name)) throw new Error('invalid notice name');
    seen.add(item.name);
    const url = new URL(item.url);
    if (url.protocol !== 'https:' || url.hostname !== 'raw.githubusercontent.com' || url.search || url.hash) throw new Error('untrusted notice source URL');
    if (!/^[a-f0-9]{64}$/.test(item.sha256)) throw new Error('invalid notice SHA-256');
  }
  return lock;
}

export async function fetchNoticeSources(lockPath, outputDir) {
  const lock = verifyNoticeLock(JSON.parse(await readFile(lockPath, 'utf8')));
  await mkdir(outputDir, { recursive: true });
  for (const item of lock.sources) {
    const response = await fetch(item.url, { redirect: 'error', signal: AbortSignal.timeout(30000) });
    if (!response.ok) throw new Error(`notice fetch failed: ${item.name}, HTTP ${response.status}`);
    const bytes = Buffer.from(await response.arrayBuffer());
    if (bytes.length === 0 || bytes.length > 1 << 20 || createHash('sha256').update(bytes).digest('hex') !== item.sha256) {
      throw new Error(`notice digest differs: ${item.name}`);
    }
    await writeFile(join(outputDir, item.name), bytes);
  }
  await writeFile(join(outputDir, 'notice-sources.lock.json'), JSON.stringify(lock, null, 2) + '\n');
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    if (process.argv.length !== 4) throw new Error('usage: node deploy/fetch-notice-sources.mjs LOCK OUTPUT_DIR');
    await fetchNoticeSources(resolve(process.argv[2]), resolve(process.argv[3]));
    console.log('Fetched five exact third-party notice sources');
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
