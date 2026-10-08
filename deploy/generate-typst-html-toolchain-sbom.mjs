#!/usr/bin/env node

import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';

const usage = 'usage: generate-typst-html-toolchain-sbom.mjs --lock <lock.json> --artifact-root <dir> --commit <sha> --output <file.spdx.json>';

const flags = new Map();
const argv = process.argv.slice(2);
for (let index = 0; index < argv.length; index += 1) {
  const flag = argv[index];
  if (!flag.startsWith('--')) throw new Error(usage);
  const value = argv[index + 1];
  if (value === undefined || value.startsWith('--')) throw new Error(usage);
  flags.set(flag.slice(2), value);
  index += 1;
}

const lockPath = flags.get('lock');
const artifactRoot = flags.get('artifact-root');
const commit = flags.get('commit');
const outputPath = flags.get('output');
if (!lockPath || !artifactRoot || !commit || !outputPath) throw new Error(usage);
if (!/^[0-9a-f]{40}$/.test(commit)) throw new Error('commit must be a 40-character Git SHA');

const lock = JSON.parse(fs.readFileSync(lockPath, 'utf8'));
if (lock.schemaVersion !== 'rin-typst-html-toolchain-lock/v1') throw new Error('unexpected lock schema');

const resolvedRoot = fs.realpathSync(path.resolve(artifactRoot));
const archiveRoot = lock.artifact.archiveRoot.replace(/\/+$/, '');
const relocatable = (relative) => {
  const candidate = path.resolve(resolvedRoot, archiveRoot, relative);
  if (!candidate.startsWith(`${resolvedRoot}${path.sep}`)) throw new Error(`artifact path escapes the root: ${relative}`);
  const real = fs.realpathSync(candidate);
  if (!real.startsWith(`${resolvedRoot}${path.sep}`)) throw new Error(`artifact path escapes the root: ${relative}`);
  return real;
};

const binary = relocatable(lock.compiler.binaryRelativePath);
const font = relocatable(lock.font.relativePath);

const digestOf = (file) => crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex');
if (digestOf(binary) !== lock.compiler.binarySha256) throw new Error('pinned Typst binary digest mismatch');
if (digestOf(font) !== lock.font.sha256) throw new Error('pinned font digest mismatch');

const packages = [
  {
    name: 'typst',
    SPDXID: 'SPDXRef-Package-typst',
    versionInfo: lock.compiler.version,
    downloadLocation: lock.compiler.release,
    filesAnalyzed: true,
    licenseConcluded: lock.compiler.license,
    licenseDeclared: lock.compiler.license,
    copyrightText: 'NOASSERTION',
    checksums: [{ algorithm: 'SHA256', checksumValue: lock.compiler.binarySha256 }],
    externalRefs: [{
      referenceCategory: 'PACKAGE-MANAGER',
      referenceType: 'purl',
      referenceLocator: `pkg:generic/typst@${lock.compiler.version}?download_url=${encodeURIComponent(lock.compiler.release)}`,
    }],
  },
  {
    name: lock.font.package,
    SPDXID: 'SPDXRef-Package-fonts-wqy-zenhei',
    versionInfo: lock.font.packageVersion,
    downloadLocation: lock.font.release,
    filesAnalyzed: true,
    licenseConcluded: lock.font.license,
    licenseDeclared: lock.font.license,
    copyrightText: 'NOASSERTION',
    checksums: [{ algorithm: 'SHA256', checksumValue: lock.font.sha256 }],
    externalRefs: [{
      referenceCategory: 'PACKAGE-MANAGER',
      referenceType: 'purl',
      referenceLocator: `pkg:deb/debian/${lock.font.package}@${lock.font.packageVersion}`,
    }],
  },
];

const created = new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');
const document = {
  spdxVersion: 'SPDX-2.3',
  dataLicense: 'CC0-1.0',
  SPDXID: 'SPDXRef-DOCUMENT',
  name: 'rinspace-typst-html-toolchain',
  documentNamespace: `https://rinspace.com/spdx/typst-html-toolchain/${commit}`,
  creationInfo: {
    created,
    creators: ['Tool: rinspace-generate-typst-html-toolchain-sbom/v1'],
  },
  documentDescribes: packages.map((item) => item.SPDXID),
  packages,
  annotations: [{
    annotationDate: created,
    annotationType: 'OTHER',
    annotator: 'Tool: rinspace-generate-typst-html-toolchain-sbom/v1',
    comment: `Immutable Typst HTML toolchain for commit ${commit}; Renderer profile ${lock.profile.id}.`,
  }],
};

fs.mkdirSync(path.dirname(path.resolve(outputPath)), { recursive: true });
fs.writeFileSync(outputPath, `${JSON.stringify(document, null, 2)}\n`, { mode: 0o644 });
