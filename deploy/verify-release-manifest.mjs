#!/usr/bin/env node
// A release lock is data, never a command or a mutable tag. Keep this verifier
// dependency-free so the public source and private consumer can run it alike.
import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const digest = /^[a-f0-9]{64}$/;
const commit = /^[a-f0-9]{40}$/;
const version = /^v[0-9]+\.[0-9]+\.[0-9]+(?:-[a-z0-9.-]+)?$/;
const image = /^ghcr\.io\/rinspacehq\/rinspace-renderer\/(runtime|latex-pdf|typst-pdf)@sha256:([a-f0-9]{64})$/;
const artifactNames = ['runtimeImage', 'latexPdfImage', 'typstPdfImage'];
const imageNames = ['runtime', 'latex-pdf', 'typst-pdf'];

function requireObject(value, name) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${name} must be an object`);
  return value;
}
function requireKeys(value, allowed, name) {
  for (const key of Object.keys(value)) if (!allowed.includes(key)) throw new Error(`${name}.${key} is unknown`);
  for (const key of allowed) if (!(key in value)) throw new Error(`${name}.${key} is missing`);
}
function requireDigest(value, name) {
  if (typeof value !== 'string' || !digest.test(value)) throw new Error(`${name} must be lowercase SHA-256`);
}

export function verifyReleaseManifest(input) {
  const manifest = requireObject(input, 'manifest');
  requireKeys(manifest, ['schemaVersion', 'version', 'sourceCommit', 'platform', 'protocol', 'artifacts', 'capabilities'], 'manifest');
  if (manifest.schemaVersion !== 'rin-renderer-release/v1') throw new Error('unsupported release manifest schema');
  if (!version.test(manifest.version)) throw new Error('invalid immutable release version');
  if (!commit.test(manifest.sourceCommit)) throw new Error('invalid source commit');
  if (manifest.platform !== 'linux/amd64') throw new Error('unsupported platform');
  const protocol = requireObject(manifest.protocol, 'protocol');
  requireKeys(protocol, ['completionSchemaSha256', 'openapiSha256'], 'protocol');
  for (const [key, value] of Object.entries(protocol)) requireDigest(value, `protocol.${key}`);
  const artifacts = requireObject(manifest.artifacts, 'artifacts');
  requireKeys(artifacts, [...artifactNames, 'typstHtmlToolchain'], 'artifacts');
  for (let i = 0; i < artifactNames.length; i++) {
    const key = artifactNames[i];
    const entry = requireObject(artifacts[key], key);
    requireKeys(entry, ['reference', 'sbomSha256', 'noticeSha256'], key);
    const match = image.exec(entry.reference);
    if (!match || match[1] !== imageNames[i]) throw new Error(`${key}.reference must be its exact GHCR digest`);
    requireDigest(entry.sbomSha256, `${key}.sbomSha256`);
    requireDigest(entry.noticeSha256, `${key}.noticeSha256`);
  }
  const toolchain = requireObject(artifacts.typstHtmlToolchain, 'typstHtmlToolchain');
  requireKeys(toolchain, ['file', 'sha256', 'sbomSha256', 'noticeSha256'], 'typstHtmlToolchain');
  if (toolchain.file !== 'rin-renderer-typst-html-toolchain.tar.gz') throw new Error('unexpected Typst toolchain filename');
  for (const key of ['sha256', 'sbomSha256', 'noticeSha256']) requireDigest(toolchain[key], `typstHtmlToolchain.${key}`);
  const capabilities = requireObject(manifest.capabilities, 'capabilities');
  requireKeys(capabilities, ['markdown', 'latexml', 'texsvg', 'latexPdf', 'typstPdf', 'typstHtml', 'localStorage', 'cloudbaseStorage', 'controlPlaneCompletion'], 'capabilities');
  for (const [key, enabled] of Object.entries(capabilities)) if (enabled !== true) throw new Error(`release capability ${key} must be verified before inclusion`);
  return manifest;
}

export async function verifyLocalFiles(manifest, directory) {
  const files = [
    ['protocol.completionSchemaSha256', 'packages/protocol/contracts/rin-control-plane/v1/renderer-completion.schema.json', manifest.protocol.completionSchemaSha256],
    ['protocol.openapiSha256', 'packages/protocol/openapi.yaml', manifest.protocol.openapiSha256],
  ];
  for (const [name, file, expected] of files) {
    const data = await readFile(resolve(directory, file));
    if (createHash('sha256').update(data).digest('hex') !== expected) throw new Error(`${name} differs from source`);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    if (process.argv.length !== 4) throw new Error('usage: node deploy/verify-release-manifest.mjs MANIFEST SOURCE_ROOT');
    const manifest = verifyReleaseManifest(JSON.parse(await readFile(process.argv[2], 'utf8')));
    await verifyLocalFiles(manifest, resolve(process.argv[3]));
    console.log(`Verified ${manifest.version} source contracts and immutable artifact references`);
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
