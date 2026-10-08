#!/usr/bin/env node

import fs from 'node:fs';

const [packagePath, imageID, outputPath] = process.argv.slice(2);
if (!packagePath || !imageID || !outputPath) {
  throw new Error('usage: generate-typst-pdf-image-sbom.mjs <dpkg.tsv> <sha256:image-id> <output.spdx.json>');
}
if (!/^sha256:[a-f0-9]{64}$/.test(imageID)) throw new Error('image ID is invalid');

const packages = fs.readFileSync(packagePath, 'utf8').trim().split('\n').filter(Boolean).map((line, index) => {
  const separator = line.indexOf('\t');
  if (separator < 1) throw new Error(`invalid package inventory line ${index + 1}`);
  const name = line.slice(0, separator);
  const version = line.slice(separator + 1);
  if (!name || !version) throw new Error(`invalid package inventory line ${index + 1}`);
  return {
    name,
    SPDXID: `SPDXRef-Package-${safeID(name)}-${index + 1}`,
    versionInfo: version,
    downloadLocation: 'NOASSERTION',
    filesAnalyzed: false,
    licenseConcluded: 'NOASSERTION',
    licenseDeclared: 'NOASSERTION',
    copyrightText: 'NOASSERTION',
    externalRefs: [{
      referenceCategory: 'PACKAGE-MANAGER',
      referenceType: 'purl',
      referenceLocator: `pkg:deb/debian/${encodeURIComponent(name)}@${encodeURIComponent(version)}`,
    }],
  };
});
packages.sort((left, right) => left.name.localeCompare(right.name) || left.versionInfo.localeCompare(right.versionInfo));

const document = {
  spdxVersion: 'SPDX-2.3',
  dataLicense: 'CC0-1.0',
  SPDXID: 'SPDXRef-DOCUMENT',
  name: 'rinspace-typst-pdf-image',
  documentNamespace: `https://rinspace.com/spdx/typst-pdf/image/${imageID.slice(7)}`,
  creationInfo: {
    created: new Date().toISOString().replace(/\.\d{3}Z$/, 'Z'),
    creators: ['Tool: rinspace-generate-typst-pdf-image-sbom/v1'],
  },
  documentDescribes: packages.map((item) => item.SPDXID),
  packages,
  annotations: [{
    annotationDate: new Date().toISOString().replace(/\.\d{3}Z$/, 'Z'),
    annotationType: 'OTHER',
    annotator: 'Tool: rinspace-generate-typst-pdf-image-sbom/v1',
    comment: `Docker image content ID: ${imageID}`,
  }],
};
fs.writeFileSync(outputPath, `${JSON.stringify(document, null, 2)}\n`, { mode: 0o644 });

function safeID(value) {
  return value.replace(/[^A-Za-z0-9.-]/g, '-');
}
