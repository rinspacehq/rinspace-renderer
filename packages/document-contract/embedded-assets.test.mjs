import assert from 'node:assert/strict';
import fs from 'node:fs';
import { test } from 'node:test';
import { validateDocumentBundle, embeddedBundleHash } from './validate-contract.mjs';

const vector = JSON.parse(fs.readFileSync(new URL('./fixtures/document-bundle/typst-final-images-v3.json',import.meta.url)));
test('resource-bearing bundle uses the cross-language hash vector',()=>{
  validateDocumentBundle(vector,'positive');
  assert.equal(embeddedBundleHash(vector),'d9c0472869a84e85009babc5979d191b50e98a8e0171dfb7f2cc8f1ff4328af4');
});
for (const [name,mutate] of [
  ['missing bytes',v=>v.assets[0].bodyBase64='invalid'],
  ['length',v=>v.assets[0].bytes++],
  ['traversal',v=>v.assets[0].projectPath='../bad.svg'],
  ['undeclared image',v=>v.pages[0].fragment='<img src="wrong.svg"/>'],
  ['unused image',v=>v.pages[0].fragment='<p>no image</p>'],
  ['duplicate payload',v=>v.assets.push({...v.assets[0],id:'asset-2'})],
  ['v1 version lie',v=>v.schemaVersion='rin-document-bundle/v1'],
  ['wrong source kind',v=>v.contentKind='markdown'],
]) test(`reject ${name} even if a sender recomputes the outer hash`,()=>{
  const value=structuredClone(vector);mutate(value);value.bundleHash=embeddedBundleHash(value);
  assert.throws(()=>validateDocumentBundle(value,name));
});
