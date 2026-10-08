#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';

const sensitivePaths = [
  /^\.github\/workflows\/rin-renderer-markdown\.yml$/,
  /^(?:rin-renderer\/)?engines\/markdown\/(?:package(?:-lock)?\.json|compiler\.mjs|finalizer\.mjs|pipeline\.mjs|shiki\.mjs|corpus\/)/,
  /^(?:rin-renderer\/)?engines\/mathjax\/(?:package(?:-lock)?\.json|render-mathjax\.mjs)$/,
  /^(?:rin-renderer\/)?api\/internal\/(?:mathservice|diagramservice|codeservice)\//,
  /^(?:rin-renderer\/)?api\/internal\/orchestration\/(?:math|diagram|code|work_resolver)\.go$/,
  /^(?:rin-renderer\/)?deploy\/(?:Dockerfile\.latexml|rin-renderer\.env\.example)$/,
];

main();

function main() {
  const options = parseArgs(process.argv.slice(2));
  const report = JSON.parse(fs.readFileSync(path.resolve(options.report || '.rin-markdown-corpus/report.json'), 'utf8'));
  const changedFiles = readLines(options.changedFiles || '');
  const force = truthy(options.force || process.env.RIN_RENDERER_MARKDOWN_CORPUS_FORCE_GATE || '');
  const sensitive = force ? ['<forced>'] : changedFiles.filter((file) => sensitivePaths.some((pattern) => pattern.test(file)));
  validateReport(report);
  if (!sensitive.length) {
    console.log('Markdown upgrade gate not required for this change set.');
    return;
  }
  const deltaTotal = report.deltas?.summary?.total;
  if (!Number.isInteger(deltaTotal)) throw new Error('Markdown corpus report has no deterministic delta summary');
  if (deltaTotal > 0) {
    const accepted = readAcceptance(options.acceptedDeltas || process.env.RIN_RENDERER_MARKDOWN_CORPUS_ACCEPTED_DELTAS || '');
    if (!accepted) {
      throw new Error(`Markdown upgrade gate found ${deltaTotal} semantic delta(s); provide RIN_RENDERER_MARKDOWN_CORPUS_ACCEPTED_DELTAS after human review (hash ${report.deltas.hash})`);
    }
    if (accepted.schemaVersion !== 'rin-markdown-corpus-delta-acceptance/v1' || accepted.deltaHash !== report.deltas.hash) {
      throw new Error('Markdown corpus delta acceptance schema or hash does not match the report');
    }
    for (const field of ['reviewedBy', 'reviewedAt', 'reason']) {
      if (!String(accepted[field] || '').trim()) throw new Error(`Markdown corpus delta acceptance is missing ${field}`);
    }
    console.log(`Accepted ${deltaTotal} reviewed Markdown semantic delta(s) by ${accepted.reviewedBy} at ${accepted.reviewedAt}.`);
  }
  console.log(`Markdown upgrade gate passed for: ${sensitive.join(', ')}`);
}

function validateReport(report) {
  if (report?.schemaVersion !== 'rin-markdown-corpus-report/v1') throw new Error('invalid Markdown corpus report schema');
  if (report.summary?.failed || report.summary?.deterministic !== true) throw new Error('Markdown corpus is failed or nondeterministic');
  if (report.coverage?.missing?.length) throw new Error(`Markdown corpus coverage is incomplete: ${report.coverage.missing.join(', ')}`);
  for (const fixture of report.fixtures || []) {
    if (fixture.status !== 'passed' || fixture.invariantErrors?.length || !fixture.semantic) {
      throw new Error(`Markdown corpus fixture ${fixture.id || '<unknown>'} did not pass`);
    }
  }
}

function readAcceptance(source) {
  const value = String(source || '').trim();
  if (!value) return null;
  return JSON.parse(value.startsWith('{') ? value : fs.readFileSync(path.resolve(value), 'utf8'));
}

function readLines(file) {
  if (!file || !fs.existsSync(path.resolve(file))) return [];
  return fs.readFileSync(path.resolve(file), 'utf8').split(/\r?\n/).map((line) => line.trim()).filter(Boolean);
}

function truthy(value) {
  return ['1', 'true', 'yes', 'on'].includes(String(value).toLowerCase());
}

function parseArgs(args) {
  const options = {};
  for (let index = 0; index < args.length; index += 1) {
    const argument = args[index];
    if (!argument.startsWith('--')) throw new Error(`unexpected argument ${argument}`);
    const [raw, inline] = argument.slice(2).split('=', 2);
    const key = raw.replace(/-([a-z])/g, (_match, character) => character.toUpperCase());
    if (inline !== undefined) options[key] = inline;
    else if (args[index + 1] && !args[index + 1].startsWith('--')) options[key] = args[++index];
    else options[key] = 'true';
  }
  return options;
}
