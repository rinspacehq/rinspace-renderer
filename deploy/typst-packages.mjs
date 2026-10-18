import { createHash } from 'node:crypto';
import { gunzipSync } from 'node:zlib';
import { readFile, readdir, mkdir, writeFile, lstat } from 'node:fs/promises';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const hash = body => createHash('sha256').update(body).digest('hex');
const canonical = name => name && !name.startsWith('/') && !name.includes('\\') && !/[\0\r\n]/.test(name) &&
  name.split('/').every(segment => segment && segment !== '.' && segment !== '..');

// The package registry archives are plain tar.gz files. Accept regular files
// and directories only; no symlinks, hardlinks, device nodes, extended headers,
// path traversal, duplicate paths or unchecked archive headers.
export function unpackPackage(archive) {
  if (archive.length > 8 << 20) throw new Error('Compressed package exceeds limit');
  const tar = gunzipSync(archive, { maxOutputLength: 32 << 20 });
  if (tar.length % 512 !== 0) throw new Error('Truncated tar archive');
  const files = new Map();
  for (let offset = 0; offset + 512 <= tar.length;) {
    const block = tar.subarray(offset, offset + 512);
    if (block.every(value => value === 0)) break;
    const text = (from, length) => block.subarray(from, from + length).toString().replace(/\0.*$/s, '');
    const octal = (from, length) => {
      const value = text(from, length).trim();
      if (!/^[0-7]+$/.test(value)) throw new Error('Invalid tar numeric header');
      return parseInt(value, 8);
    };
    const checksum = block.reduce((sum, value, index) => sum + (index >= 148 && index < 156 ? 32 : value), 0);
    if (checksum !== octal(148, 8)) throw new Error('Invalid tar checksum');
    const prefix = text(345, 155);
    let name = (prefix ? `${prefix}/` : '') + text(0, 100);
    name = name.replace(/^\.\//, '').replace(/\/$/, '');
    const type = block[156];
    const size = octal(124, 12);
    offset += 512;
    if (size > 4 << 20 || offset + size > tar.length) throw new Error('Package member too large or truncated');
    if (type === 53) {
      if (name !== '.' && name && !canonical(name)) throw new Error('Unsafe package directory');
    } else if ((type === 0 || type === 48) && canonical(name)) {
      if (files.has(name) || files.size >= 2000) throw new Error('Duplicate or excessive package files');
      files.set(name, Buffer.from(tar.subarray(offset, offset + size)));
    } else {
      throw new Error(`Unsupported package member: ${name}`);
    }
    offset += Math.ceil(size / 512) * 512;
  }
  if (!files.has('typst.toml') || ![...files.keys()].some(name => /^(?:LICENSE|COPYING)(?:\.|$)/i.test(name))) {
    throw new Error('Package metadata or license missing');
  }
  return files;
}

export function packageSnapshotHash(files) {
  const digest = createHash('sha256');
  for (const [name, body] of [...files].sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0)) {
    digest.update(`${name}\0${hash(body)}\0${body.length}\n`);
  }
  return digest.digest('hex');
}

export async function packageFiles(directory) {
  const files = new Map();
  let bytes = 0;
  async function walk(relative = '') {
    for (const entry of await readdir(path.join(directory, relative), { withFileTypes: true })) {
      const name = relative ? `${relative}/${entry.name}` : entry.name;
      if (!canonical(name)) throw new Error('Unsafe package path');
      if (entry.isDirectory()) await walk(name);
      else if (entry.isFile()) {
        const stat = await lstat(path.join(directory, name));
        bytes += stat.size;
        if (stat.size > 4 << 20 || files.size >= 2000 || bytes > 64 << 20) throw new Error('Package set too large');
        files.set(name, await readFile(path.join(directory, name)));
      } else throw new Error('Package snapshot contains a non-regular file');
    }
  }
  await walk();
  return files;
}

export async function assemblePackages(approved, directory, downloads) {
  await mkdir(directory, { mode: 0o755 });
  const files = new Map();
  for (const item of approved) {
    if (!/^[a-z][a-z0-9-]*$/.test(item.name) || !/^\d+\.\d+\.\d+$/.test(item.version) || !/^[a-f0-9]{64}$/.test(item.archiveSha256)) {
      throw new Error('Invalid pinned package');
    }
    let archive;
    if (downloads) archive = await readFile(path.join(downloads, `${item.name}-${item.version}.tar.gz`));
    else {
      const response = await fetch(`https://packages.typst.org/preview/${item.name}-${item.version}.tar.gz`, { redirect: 'error', signal: AbortSignal.timeout(30_000) });
      if (!response.ok) throw new Error(`Package download HTTP ${response.status}`);
      let size = 0;
      const chunks = [];
      for await (const chunk of response.body) {
        size += chunk.length;
        if (size > 8 << 20) throw new Error('Package download exceeds limit');
        chunks.push(chunk);
      }
      archive = Buffer.concat(chunks);
    }
    if (archive.length > 8 << 20 || hash(archive) !== item.archiveSha256) throw new Error('Package archive digest mismatch');
    const members = unpackPackage(archive);
    const manifest = members.get('typst.toml').toString();
    for (const [key, value] of [['name', item.name], ['version', item.version], ['license', item.license]]) {
      if (!manifest.split('\n').some(line => line.trim() === `${key} = "${value}"`)) throw new Error(`Package metadata mismatch: ${key}`);
    }
    for (const [member, body] of members) {
      const name = `preview/${item.name}/${item.version}/${member}`;
      const target = path.join(directory, name);
      await mkdir(path.dirname(target), { recursive: true });
      await writeFile(target, body, { flag: 'wx', mode: 0o444 });
      files.set(name, body);
    }
  }
  return packageSnapshotHash(files);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [command, first, second, downloads] = process.argv.slice(2);
  if (command === 'verify') {
    const lock = JSON.parse(await readFile(first));
    const digest = packageSnapshotHash(await packageFiles(second));
    if (digest !== lock.packages.packageSetDigest) throw new Error('Sealed package tree digest mismatch');
    console.log(digest);
  } else if (command === 'assemble') {
    const lock = JSON.parse(await readFile(first));
    const digest = await assemblePackages(lock.packages.approved, second, downloads);
    if (digest !== lock.packages.packageSetDigest) throw new Error(`Assembled package tree digest mismatch: ${digest}`);
    console.log(digest);
  } else throw new Error('Usage: typst-packages.mjs assemble|verify LOCK PACKAGE_DIRECTORY [DOWNLOAD_DIRECTORY]');
}
