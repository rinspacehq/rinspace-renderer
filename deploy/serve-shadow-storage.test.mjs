import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import test from 'node:test';

import { createShadowStorage } from './serve-shadow-storage.mjs';

test('stores and verifies one immutable content-addressed SVG', async (t) => {
  const server = createShadowStorage({ token: 'test-token' });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise((resolve) => server.close(resolve)));
  const address = server.address();
  const root = `http://127.0.0.1:${address.port}`;
  const body = Buffer.from('<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0"/></svg>');
  const digest = crypto.createHash('sha256').update(body).digest('hex');
  const objectID = `diagrams/v1/svg-sha256/${digest.slice(0, 2)}/${digest}.svg`;
  const upload = await fetch(`${root}/v1/storages/object/rin-renderer/${objectID}`, {
    method: 'POST', headers: { Authorization: 'Bearer test-token', 'X-Upsert': 'true', 'Content-Type': 'image/svg+xml; charset=utf-8' }, body,
  });
  assert.equal(upload.status, 200);
  const read = await fetch(`${root}/v1/storages/object/public/rin-renderer/${objectID}`);
  assert.equal(read.status, 200);
  assert.deepEqual(Buffer.from(await read.arrayBuffer()), body);
});

test('rejects unauthorized and hash-mismatched writes', async (t) => {
  const server = createShadowStorage({ token: 'test-token' });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise((resolve) => server.close(resolve)));
  const address = server.address();
  const root = `http://127.0.0.1:${address.port}`;
  const objectID = `diagrams/v1/svg-sha256/aa/${'a'.repeat(64)}.svg`;
  const unauthorized = await fetch(`${root}/v1/storages/object/rin-renderer/${objectID}`, { method: 'POST', body: '<svg/>' });
  assert.equal(unauthorized.status, 401);
  const mismatch = await fetch(`${root}/v1/storages/object/rin-renderer/${objectID}`, {
    method: 'POST', headers: { Authorization: 'Bearer test-token', 'X-Upsert': 'true' }, body: '<svg/>',
  });
  assert.equal(mismatch.status, 400);
});
