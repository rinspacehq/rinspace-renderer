import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtemp, mkdir, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { spawn, execFileSync } from 'node:child_process';
import test from 'node:test';

const official = '--registry=https://registry.npmjs.org';
const replacement = '--replace-registry-host=always';

test('image and source checks replace mirror download hosts without changing frozen inputs', async () => {
  // Audit every image stage, including the LaTeXML base that runtime inherits.
  // Reviewing only Dockerfile.runtime leaves frozen mirrors in its parent image.
  const dockerfiles = await Promise.all((await readdir(new URL('./', import.meta.url)))
    .filter(name => name.startsWith('Dockerfile.'))
    .map(name => readFile(new URL(name, import.meta.url), 'utf8')));
  const workflow = await readFile(new URL('../.github/workflows/renderer-check.yml', import.meta.url), 'utf8');
  const commands = [
    ...dockerfiles.flatMap(text => text.split('\n').filter(line => /npm (?:--prefix \S+ )?ci(?:\s|$)/.test(line))),
    ...workflow.split('\n').filter(line => /npm --prefix engines\/(markdown|mathjax|katex) ci /.test(line)),
  ];
  assert.equal(commands.length, 7);
  for (const command of commands) {
    for (const flag of [official, replacement, '--ignore-scripts', '--omit=dev', '--no-audit', '--no-fund', '--fetch-timeout=60000', '--fetch-retries=2']) {
      assert(command.includes(flag), `frozen install command lacks ${flag}`);
    }
    assert(!/npm (?:install|update)|--force|--ignore-integrity/.test(command));
  }
});

async function fixture(t, { corrupt = false } = {}) {
  const root = await mkdtemp(path.join(os.tmpdir(), 'rin-npm-registry-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const pkg = path.join(root, 'archive/package');
  const project = path.join(root, 'project');
  await mkdir(pkg, { recursive: true });
  await mkdir(project);
  const userConfig = path.join(root, 'npm-user.rc');
  const globalConfig = path.join(root, 'npm-global.rc');
  await writeFile(userConfig, '');
  await writeFile(globalConfig, '');
  const marker = 'postinstall-ran';
  const descriptor = { name: 'rin-registry-fixture', version: '1.0.0', license: 'MIT',
    scripts: { postinstall: `node -e "require('fs').writeFileSync('${marker}','unexpected')"` } };
  await writeFile(path.join(pkg, 'package.json'), JSON.stringify(descriptor));
  await writeFile(path.join(pkg, 'index.js'), 'export const fixture = true;\n');
  const archive = path.join(root, 'fixture.tgz');
  execFileSync('tar', ['-czf', archive, '-C', path.join(root, 'archive'), 'package']);
  const bytes = await readFile(archive);
  const integrity = 'sha512-' + createHash('sha512').update(bytes).digest('base64');
  const packageJSON = { name: 'registry-transport-test', version: '1.0.0', private: true,
    dependencies: { 'rin-registry-fixture': '1.0.0' } };
  const lock = { name: packageJSON.name, version: packageJSON.version, lockfileVersion: 3, requires: true,
    packages: { '': packageJSON, 'node_modules/rin-registry-fixture': {
      version: '1.0.0', resolved: 'https://unreachable.example.invalid/rin-registry-fixture/-/rin-registry-fixture-1.0.0.tgz',
      integrity, license: 'MIT', hasInstallScript: true,
    } } };
  const originalLock = JSON.stringify(lock, null, 2) + '\n';
  await writeFile(path.join(project, 'package.json'), JSON.stringify(packageJSON));
  await writeFile(path.join(project, 'package-lock.json'), originalLock);
  const requests = [];
  const server = http.createServer((request, response) => {
    requests.push(request.url);
    if (request.method !== 'GET' || request.url !== '/rin-registry-fixture/-/rin-registry-fixture-1.0.0.tgz') {
      response.writeHead(404); response.end(); return;
    }
    response.writeHead(200, { 'Content-Type': 'application/octet-stream' });
    response.end(corrupt ? Buffer.from('not the locked package') : bytes);
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => server.close(resolve)));
  const address = server.address();
  assert(address && typeof address === 'object');
  const args = ['ci', '--omit=dev', '--ignore-scripts', '--no-audit', '--no-fund',
    `--registry=http://127.0.0.1:${address.port}`, replacement, '--fetch-timeout=2000', '--fetch-retries=0',
    '--cache', path.join(root, 'empty-cache')];
  const result = await new Promise((resolve, reject) => {
    const child = spawn('npm', args, { cwd: project, env: {
      PATH: process.env.PATH, HOME: root, TMPDIR: root, CI: 'true', LANG: 'C.UTF-8',
      NPM_CONFIG_USERCONFIG: userConfig, NPM_CONFIG_GLOBALCONFIG: globalConfig, NO_PROXY: '127.0.0.1,localhost',
    }, stdio: ['ignore', 'pipe', 'pipe'], signal: AbortSignal.timeout(20_000) });
    let output = '';
    child.stdout.on('data', chunk => { output += chunk; });
    child.stderr.on('data', chunk => { output += chunk; });
    child.on('error', reject);
    child.on('close', code => resolve({ code, output }));
  });
  assert(requests.length > 0 && requests.length <= 3, `real npm must use the configured registry, not a warmed cache: ${result.output}`);
  assert(requests.every(url => url === '/rin-registry-fixture/-/rin-registry-fixture-1.0.0.tgz'));
  assert.equal(await readFile(path.join(project, 'package-lock.json'), 'utf8'), originalLock);
  return { project, marker, ...result };
}

test('real npm replaces an unreachable locked mirror and keeps integrity, versions and lifecycle policy', async t => {
  const result = await fixture(t);
  assert.equal(result.code, 0, result.output);
  const installed = JSON.parse(await readFile(path.join(result.project, 'node_modules/rin-registry-fixture/package.json')));
  assert.equal(installed.version, '1.0.0');
  await assert.rejects(readFile(path.join(result.project, 'node_modules/rin-registry-fixture', result.marker)), { code: 'ENOENT' });
});

test('registry replacement still rejects package bytes that violate the exact lock integrity', async t => {
  const result = await fixture(t, { corrupt: true });
  assert.notEqual(result.code, 0);
  assert.match(result.output, /EINTEGRITY/);
});
