import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { gzipSync } from 'node:zlib';
import { unpackPackage, packageSnapshotHash } from './typst-packages.mjs';

function archive(entries) {
  const blocks = [];
  for (const [name, body = '', type = '0'] of entries) {
    const header = Buffer.alloc(512);
    header.write(name, 0, 100);
    const content = Buffer.from(body);
    header.write(content.length.toString(8).padStart(11, '0') + '\0', 124);
    header.fill(32, 148, 156);
    header.write(type, 156);
    const sum = header.reduce((total, value) => total + value, 0);
    header.write(sum.toString(8).padStart(6, '0') + '\0 ', 148);
    blocks.push(header, content, Buffer.alloc((512 - content.length % 512) % 512));
  }
  return gzipSync(Buffer.concat([...blocks, Buffer.alloc(1024)]));
}
const entries = [['typst.toml', '[package]\nname = "example"\n'], ['LICENSE', 'Test license'], ['lib.typ', '#let x = 1\n']];

test('keeps exact package source and licenses', () => {
  const files = unpackPackage(archive(entries));
  assert.equal(files.size, 3);
  assert.equal(files.get('lib.typ').toString(), '#let x = 1\n');
  const reordered = new Map([...files].reverse());
  assert.equal(packageSnapshotHash(files), packageSnapshotHash(reordered));
  assert.equal(packageSnapshotHash(new Map()), createHash('sha256').digest('hex'));
  reordered.set('lib.typ', Buffer.from('changed'));
  assert.notEqual(packageSnapshotHash(files), packageSnapshotHash(reordered));
});

test('rejects traversal, links, duplicates, missing licenses and oversized members', () => {
  for (const extra of [['../escape', 'bad'], ['/absolute', 'bad'], ['dir/../../escape', 'bad'], ['link.typ', '', '2'], ['hard.typ', '', '1'], ['lib.typ', 'duplicate']]) {
    assert.throws(() => unpackPackage(archive([...entries, extra])));
  }
  assert.throws(() => unpackPackage(archive([['typst.toml', 'metadata']])));
  assert.throws(() => unpackPackage(archive([...entries, ['big.typ', 'x'.repeat((4 << 20) + 1)]])));
});
