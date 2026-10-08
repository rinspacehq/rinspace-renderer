import { readFile } from 'node:fs/promises';

const packageDirectory = new URL('.', import.meta.url);
const rendererRoot = new URL('../../', packageDirectory);
const manifest = JSON.parse(await readFile(new URL('package.json', packageDirectory), 'utf8'));
const lock = JSON.parse(await readFile(new URL('package-lock.json', packageDirectory), 'utf8'));
const latexmlDockerfile = await readFile(new URL('deploy/Dockerfile.latexml', rendererRoot), 'utf8');

if (latexmlDockerfile.includes('engines/markdown')) {
  throw new Error('Markdown worker package leaked into the heavy LaTeXML image');
}
if (lock.lockfileVersion !== 3 || lock.packages?.['']?.name !== manifest.name) {
  throw new Error('Markdown worker lock file is not an independent npm v3 graph');
}
if (Object.keys(manifest.devDependencies || {}).length !== 0) {
  throw new Error('Markdown production worker unexpectedly has development dependencies');
}

console.log('Markdown dependency isolation ok: independent package graph and LaTeXML image boundary');
