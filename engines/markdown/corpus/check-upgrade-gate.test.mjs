import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'rin-markdown-gate-'));
const reportPath = path.join(directory, 'report.json');
const changedPath = path.join(directory, 'changed.txt');
const script = new URL('./check-upgrade-gate.mjs', import.meta.url);

try {
  writeReport(0, '0'.repeat(64));
  fs.writeFileSync(changedPath, 'rin-renderer/engines/markdown/finalizer.mjs\n');
  assert.match(run(), /Markdown upgrade gate passed/);

  fs.writeFileSync(changedPath, 'engines/markdown/finalizer.mjs\n');
  assert.match(run(), /Markdown upgrade gate passed/);

  writeReport(2, 'a'.repeat(64));
  assert.throws(() => run(), /after human review/);
  assert.throws(() => run(JSON.stringify(acceptance('b'.repeat(64)))), /does not match/);
  assert.match(run(JSON.stringify(acceptance('a'.repeat(64)))), /Accepted 2 reviewed/);

  writeReport(0, '0'.repeat(64), { failed: 1, deterministic: false });
  assert.throws(() => run(), /failed or nondeterministic/);

  writeReport(0, '0'.repeat(64));
  fs.writeFileSync(changedPath, 'docs/notes.md\n');
  assert.match(run(), /not required/);
} finally {
  fs.rmSync(directory, { recursive: true, force: true });
}

console.log('Markdown semantic upgrade gate ok: fail-closed delta review contract');

function run(accepted = '') {
  return execFileSync(process.execPath, [fileURLToPath(script), '--report', reportPath, '--changed-files', changedPath], {
    encoding: 'utf8', env: { ...process.env, RIN_RENDERER_MARKDOWN_CORPUS_ACCEPTED_DELTAS: accepted },
  });
}

function writeReport(total, hash, summary = { failed: 0, deterministic: true }) {
  fs.writeFileSync(reportPath, JSON.stringify({
    schemaVersion: 'rin-markdown-corpus-report/v1', summary,
    coverage: { missing: [] },
    fixtures: [{ id: 'fixture', status: 'passed', invariantErrors: [], semantic: {} }],
    deltas: { summary: { total }, hash },
  }));
}

function acceptance(deltaHash) {
  return {
    schemaVersion: 'rin-markdown-corpus-delta-acceptance/v1', deltaHash,
    reviewedBy: 'maintainer', reviewedAt: '2026-08-10', reason: 'Expected synthetic test delta.',
  };
}
