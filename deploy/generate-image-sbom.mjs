#!/usr/bin/env node
import { createHash } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';

const root = resolve(import.meta.dirname, '..');
const imagePattern = /^ghcr\.io\/rinspacehq\/rinspace-renderer\/(runtime|latex-pdf|typst-pdf)@sha256:([a-f0-9]{64})$/;
const sha = value => createHash('sha256').update(value).digest('hex');
const packageID = (kind, name, version) => `SPDXRef-${kind}-${sha(`${name}@${version}`).slice(0, 20)}`;

function packageRecord(kind, name, version, license, purl) {
  if (!name || !version || /[\r\n]/.test(name + version)) throw new Error(`invalid ${kind} package`);
  return {
    name, SPDXID: packageID(kind, name, version), versionInfo: version,
    downloadLocation: 'NOASSERTION', filesAnalyzed: false,
    licenseConcluded: 'NOASSERTION', licenseDeclared: license || 'NOASSERTION',
    copyrightText: 'NOASSERTION',
    externalRefs: [{ referenceCategory: 'PACKAGE-MANAGER', referenceType: 'purl', referenceLocator: purl }],
  };
}

export async function generateImageSBOM({ component, reference, dpkgPath, goModulesPath }) {
  const match = imagePattern.exec(reference);
  if (!match || match[1] !== component) throw new Error('SBOM requires the exact component OCI digest');
  const packages = [];
  const rows = (await readFile(dpkgPath, 'utf8')).trim().split('\n').filter(Boolean);
  if (rows.length < 2 || rows.length > 2000) throw new Error('unexpected Debian package closure');
  for (const row of rows) {
    const fields = row.split('\t');
    if (fields.length !== 2) throw new Error('malformed Debian package row');
    const [name, version] = fields;
    packages.push(packageRecord('Deb', name, version, 'NOASSERTION', `pkg:deb/debian/${encodeURIComponent(name)}@${encodeURIComponent(version)}`));
  }
  if (component === 'runtime') {
    if (!goModulesPath) throw new Error('runtime SBOM requires Go module inventory');
    const modules = (await readFile(goModulesPath, 'utf8')).trim().split('\n').filter(Boolean);
    for (const row of modules) {
      const [spec] = row.split('|');
      if (spec.endsWith('@')) continue;
      const at = spec.lastIndexOf('@');
      if (at < 1) throw new Error('malformed Go module row');
      const name = spec.slice(0, at), version = spec.slice(at + 1);
      packages.push(packageRecord('Go', name, version, 'NOASSERTION', `pkg:golang/${name}@${version}`));
    }
    for (const engine of ['markdown', 'mathjax', 'katex']) {
      const lock = JSON.parse(await readFile(join(root, 'engines', engine, 'package-lock.json'), 'utf8'));
      for (const [path, item] of Object.entries(lock.packages)) {
        if (!path) continue;
        const name = path.slice(path.lastIndexOf('node_modules/') + 'node_modules/'.length);
        if (!item.version || !item.license) throw new Error(`missing npm license metadata: ${engine}/${name}`);
        packages.push(packageRecord('Npm', `${engine}/${name}`, item.version, item.license,
          `pkg:npm/${encodeURIComponent(name)}@${encodeURIComponent(item.version)}`));
      }
    }
    const latexml = JSON.parse(await readFile(join(root, 'engines/latexml/latexml.lock'), 'utf8'));
    packages.push(packageRecord('Engine', 'LaTeXML', latexml.commit, 'NOASSERTION', `pkg:github/brucemiller/LaTeXML@${latexml.commit}`));
    packages.push(packageRecord('Engine', 'Node.js', '22.22.3', 'NOASSERTION', 'pkg:generic/node@22.22.3'));
  } else if (component === 'latex-pdf') {
    const lock = JSON.parse(await readFile(join(root, 'deploy/latex-pdf.lock.json'), 'utf8'));
    packages.push(packageRecord('Engine', 'XeTeX', lock.compiler.engineVersion, 'NOASSERTION', 'pkg:generic/xetex@2022'));
  } else {
    const lock = JSON.parse(await readFile(join(root, 'deploy/typst-pdf.lock.json'), 'utf8'));
    packages.push(packageRecord('Engine', 'Typst', lock.compiler.version, lock.compiler.license, `pkg:generic/typst@${lock.compiler.version}`));
  }
  const ids = new Set();
  for (const item of packages) {
    if (ids.has(item.SPDXID)) throw new Error(`duplicate SBOM package ${item.name}`);
    ids.add(item.SPDXID);
  }
  packages.sort((a, b) => a.SPDXID.localeCompare(b.SPDXID));
  const created = new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');
  return {
    spdxVersion: 'SPDX-2.3', dataLicense: 'CC0-1.0', SPDXID: 'SPDXRef-DOCUMENT',
    name: `rinspace-renderer-${component}`, documentNamespace: `https://github.com/rinspacehq/rinspace-renderer/spdx/${component}/${match[2]}`,
    creationInfo: { created, creators: ['Tool: rin-renderer-generate-image-sbom/v1'] },
    documentDescribes: packages.map(item => item.SPDXID), packages,
    annotations: [{ annotationDate: created, annotationType: 'OTHER', annotator: 'Tool: rin-renderer-generate-image-sbom/v1',
      comment: `OCI reference ${reference}; Debian licenses are retained in the matching notice bundle.` }],
  };
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    const flags = Object.fromEntries(process.argv.slice(2).reduce((pairs, flag, index, args) => {
      if (index % 2 === 0) pairs.push([flag, args[index + 1]]);
      return pairs;
    }, []));
    const expected = ['--component', '--reference', '--dpkg', '--output'];
    if (expected.some(key => !flags[key]) || !['runtime', 'latex-pdf', 'typst-pdf'].includes(flags['--component'])) {
      throw new Error('usage: node deploy/generate-image-sbom.mjs --component COMPONENT --reference OCI_DIGEST --dpkg PACKAGES_TSV --output SPDX_JSON [--go-modules MODULES_TSV]');
    }
    const document = await generateImageSBOM({ component: flags['--component'], reference: flags['--reference'],
      dpkgPath: flags['--dpkg'], goModulesPath: flags['--go-modules'] });
    await writeFile(flags['--output'], JSON.stringify(document, null, 2) + '\n');
    console.log(`Generated SPDX for ${document.packages.length} pinned and installed packages`);
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
