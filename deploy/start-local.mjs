#!/usr/bin/env node
import { randomBytes } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { chmod, readFile, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { verifyLocalFiles, verifyReleaseManifest } from './verify-release-manifest.mjs';

const directory = dirname(fileURLToPath(import.meta.url));
const envPath = join(directory, '.local.env');
const composePath = join(directory, 'local.compose.yml');
const imagePattern = /^ghcr\.io\/rinspacehq\/rinspace-renderer(?:\/[a-z0-9-]+)?@sha256:[a-f0-9]{64}$/;

async function readIfPresent(path) {
  try { return await readFile(path, 'utf8'); }
  catch (error) { if (error.code === 'ENOENT') return ''; throw error; }
}

function runDocker(args) {
  const result = spawnSync('docker', args, { stdio: 'inherit' });
  if (result.error || result.status !== 0) {
    throw new Error(`docker ${args.join(' ')} failed: ${result.error?.message || result.status}`);
  }
}

async function main() {
  const args = process.argv.slice(2);
  if (args.length > 2 || args.length === 1 || args.length === 2 && args[0] !== '--image') {
    throw new Error('usage: node deploy/start-local.mjs [--image ghcr.io/rinspacehq/rinspace-renderer/runtime@sha256:<digest>]');
  }
  const previous = Object.fromEntries((await readIfPresent(envPath)).split('\n').filter(Boolean).map(line => {
    const index = line.indexOf('=');
    return [line.slice(0, index), line.slice(index + 1)];
  }));
  let image = args.length === 2 ? args[1] : '';
  const manifestSource = await readIfPresent(join(directory, 'release-manifest.json'));
  if (manifestSource) {
    const manifest = verifyReleaseManifest(JSON.parse(manifestSource));
    await verifyLocalFiles(manifest, join(directory, '..'));
    const lockedImage = manifest.artifacts.runtimeImage.reference;
    if (image && image !== lockedImage) throw new Error('requested image differs from reviewed release manifest');
    image = lockedImage;
  }
  if (!image) image = previous.RIN_RENDERER_IMAGE || '';
  if (!imagePattern.test(image)) throw new Error('local startup requires an exact reviewed GHCR image digest');
  const password = previous.RIN_RENDERER_LOCAL_DB_PASSWORD || randomBytes(24).toString('hex');
  const token = previous.RIN_RENDERER_LOCAL_SERVICE_TOKEN || randomBytes(32).toString('hex');
  if (!/^[a-f0-9]{48}$/.test(password) || !/^[a-f0-9]{64}$/.test(token)) throw new Error('existing local credentials are invalid');
  await writeFile(envPath, `RIN_RENDERER_IMAGE=${image}\nRIN_RENDERER_LOCAL_DB_PASSWORD=${password}\nRIN_RENDERER_LOCAL_SERVICE_TOKEN=${token}\n`, { mode: 0o600 });
  await chmod(envPath, 0o600);
  const compose = ['compose', '--project-name', 'rin-renderer-local', '--env-file', envPath, '--file', composePath];
  runDocker([...compose, 'config', '--quiet']);
  runDocker([...compose, 'up', '--detach', '--wait']);
  console.log('Rinspace Renderer local API is ready at http://127.0.0.1:8090; credentials are in deploy/.local.env.');
}

main().catch(error => {
  console.error(error.message);
  process.exitCode = 1;
});
