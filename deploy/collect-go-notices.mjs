#!/usr/bin/env node
import { createHash } from 'node:crypto';
import { lstat, mkdir, readFile, readdir, writeFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';

const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');
const licenseName = /^(?:LICENSE|LICENCE|COPYING|NOTICE)(?:[._-].*)?$/i;

export async function collectGoNotices(moduleListPath, outputDirectory) {
  const lines = (await readFile(moduleListPath, 'utf8')).trim().split('\n').filter(Boolean);
  if (lines.length < 2 || lines.length > 100) throw new Error('unexpected Go module count');
  const root = resolve(outputDirectory);
  await mkdir(root, { recursive: true });
  const entries = [];
  for (const line of lines) {
    const separator = line.lastIndexOf('|');
    if (separator < 1) throw new Error('malformed Go module row');
    const module = line.slice(0, separator);
    const directory = line.slice(separator + 1);
    if (module.endsWith('@')) continue; // main module is the AGPL source tree
    if (!/^[-A-Za-z0-9._~/!]+@v[-A-Za-z0-9.+]+$/.test(module) || !directory.startsWith('/')) {
      throw new Error(`invalid Go module identity: ${module}`);
    }
    const files = (await readdir(directory)).filter(name => licenseName.test(name)).sort();
    if (!files.some(name => /^(?:LICENSE|LICENCE|COPYING)/i.test(name))) throw new Error(`missing Go module license: ${module}`);
    const key = sha256(Buffer.from(module));
    const target = join(root, key);
    await mkdir(target, { recursive: true });
    const notices = [];
    for (const name of files) {
      const source = join(directory, name);
      const stat = await lstat(source);
      if (!stat.isFile() || stat.isSymbolicLink() || stat.size > 1 << 20) throw new Error(`invalid Go notice: ${module}/${name}`);
      const bytes = await readFile(source);
      await writeFile(join(target, name), bytes);
      notices.push({ file: name, sha256: sha256(bytes) });
    }
    entries.push({ module, directory: key, notices });
  }
  if (entries.length === 0) throw new Error('no external Go modules');
  entries.sort((a, b) => a.module.localeCompare(b.module));
  await writeFile(join(root, 'index.json'), JSON.stringify({ schemaVersion: 'rin-renderer-go-notices/v1', entries }, null, 2) + '\n');
  return entries.length;
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    if (process.argv.length !== 4) throw new Error('usage: node deploy/collect-go-notices.mjs MODULES_TSV OUTPUT_DIR');
    console.log(`Collected full notice files for ${await collectGoNotices(process.argv[2], process.argv[3])} Go modules`);
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
