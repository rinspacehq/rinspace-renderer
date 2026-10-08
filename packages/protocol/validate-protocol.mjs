#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { validateDocumentBundle } from '../document-contract/validate-contract.mjs';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const root = __dirname;
const required = [
  'openapi.yaml',
  'schemas/capabilities.schema.json',
  'schemas/diagnostic.schema.json',
  'schemas/diagram-render-request.schema.json',
  'schemas/diagram-render-response.schema.json',
  'schemas/job-event.schema.json',
  'schemas/job-submit-request.schema.json',
  'schemas/job-support.schema.json',
  'schemas/job-status.schema.json',
  'schemas/job-submit-response.schema.json',
  'schemas/latex-pdf-preview-error.schema.json',
  'schemas/latex-pdf-preview-result.schema.json',
  'schemas/typst-pdf-result.schema.json',
  'schemas/pdf-inspection.schema.json',
  'schemas/project-render-response.schema.json',
  'schemas/project-render-accepted.schema.json',
  'schemas/queue-status.schema.json',
  'schemas/readiness.schema.json',
  'schemas/render-result.schema.json',
  'src/index.ts',
];

for (const file of required) {
  const target = path.join(root, file);
  if (!fs.existsSync(target)) {
    throw new Error(`Missing protocol file: ${target}`);
  }
}

const schemas = new Map();
for (const file of required.filter((name) => name.endsWith('.json'))) {
  schemas.set(file, JSON.parse(fs.readFileSync(path.join(root, file), 'utf8')));
}

const openapi = fs.readFileSync(path.join(root, 'openapi.yaml'), 'utf8');
for (const route of [
  '/api/render/capabilities',
  '/api/render/pdf/inspect',
  '/ready',
  '/metrics',
  '/api/render/projects',
  '/api/render/diagrams/{type}',
  '/api/render/jobs',
  '/api/render/jobs/{jobId}',
  '/api/render/jobs/{jobId}/result',
  '/api/render/jobs/{jobId}/artifacts/{artifactKind}',
  '/api/render/jobs/{jobId}/events',
  '/api/render/jobs/{jobId}/support',
  '/api/render/jobs/{jobId}/retry',
  '/api/render/jobs/{jobId}/expire',
  '/api/render/queue',
]) {
  if (!openapi.includes(route)) {
    throw new Error(`OpenAPI is missing ${route}`);
  }
}

const tsProtocol = fs.readFileSync(path.join(root, 'src/index.ts'), 'utf8');
if (!tsProtocol.includes("RIN_FINAL_OUTPUT_CONTRACT_VERSION = 'rin-final-output/v1'")) {
  throw new Error('Protocol TypeScript is missing the final-output contract version');
}
for (const symbol of [
  'RinRendererProjectResponse',
  'RinRendererProjectManifest',
  'RinRendererDiagramPlaceholder',
  'RinRendererAssetManifest',
  'RinRendererGeneratedListInventory',
  'RinRendererDiagramResponse',
  'RinDiagramBatchV1',
  'RinDiagramBatchResultV1',
  'RinResolvedDiagramUnit',
  'RinRendererCapabilities',
  'RinRendererNodeWorkers',
  'RinArtifactReference',
  'RinPDFInspectionV1',
  'RinRenderResultV1',
  'RinRendererJobStatus',
  'RinRendererJobSubmission',
  'RinMarkdownBookPage',
  'RinRendererJobSubmitFields',
  'RinRendererLatexPDFPreviewJobSubmitFields',
  'RinRendererLatexPDFPreviewSubmission',
  'RinLatexPDFPreviewArtifact',
  'RinLatexPDFPreviewError',
  'RinLatexPDFPreviewResultV1',
  'RinTypstPDFResultV1',
  'RinTypstPDFArtifact',
  'RinRendererTypstPDFJobSubmitFields',
  'RinRendererTypstPDFSubmission',
  'RinRendererProjectAccepted',
  'RinRendererCompletionV2',
  'RinRendererJobEvent',
  'RinRendererJobSupport',
  'RinRendererQueueStatus',
  'RinRendererReadiness',
  'RinRendererWaitEstimate',
]) {
  if (!tsProtocol.includes(`interface ${symbol}`)) {
    throw new Error(`Protocol TypeScript is missing ${symbol}`);
  }
}

const sharedCompletionSchemaPath = path.join(root, 'contracts/rin-control-plane/v1/renderer-completion.schema.json');
if (!fs.existsSync(sharedCompletionSchemaPath)) {
  throw new Error(`Missing shared Renderer completion contract: ${sharedCompletionSchemaPath}`);
}
const sharedCompletionSchema = JSON.parse(fs.readFileSync(sharedCompletionSchemaPath, 'utf8'));
for (const field of ['schema', 'event_id', 'type', 'occurred_at', 'producer', 'correlation_id', 'subject', 'data']) {
  assertRequired(sharedCompletionSchema, field);
}
for (const field of ['schemaVersion', 'protocolVersion', 'terminalVersion', 'rendererJobId', 'requestId', 'controlProjectId', 'sourceCommit', 'controlProjectHash', 'rendererProjectHash', 'terminalState']) {
  assertRequired(sharedCompletionSchema.properties.data, field);
}
if (sharedCompletionSchema.additionalProperties !== false || sharedCompletionSchema.properties.subject.additionalProperties !== false || sharedCompletionSchema.properties.data.additionalProperties !== false) {
  throw new Error('Shared Renderer completion contract must reject unknown fields');
}

const projectSchema = schemas.get('schemas/project-render-response.schema.json');
const defs = projectSchema.$defs || {};
assertObject(defs.projectManifest, 'projectManifest schema');
assertObject(defs.mathSummary, 'mathSummary schema');
assertObject(defs.diagramPlaceholder, 'diagramPlaceholder schema');
assertObject(defs.assetManifest, 'assetManifest schema');
assertObject(defs.generatedListInventory, 'generatedListInventory schema');
assertObject(defs.generatedListReference, 'generatedListReference schema');
assertRequired(defs.projectManifest, 'diagramPlaceholders');
assertRequired(defs.projectManifest, 'generatedLists');
assertPropertyRef(defs.projectManifest, 'diagramPlaceholders', '#/$defs/diagramPlaceholder');
assertPropertyRef(defs.generatedListInventory, 'references', '#/$defs/generatedListReference');
assertPropertyRef(defs.generatedListInventory, 'missing', '#/$defs/generatedListReference');
for (const field of ['id', 'type', 'body', 'source', 'sourceFile', 'sourceLine', 'sourceColumn', 'placeholder', 'layout']) {
  assertRequired(defs.diagramPlaceholder, field);
}
for (const field of ['kind', 'listType', 'command', 'label', 'resolved', 'expectedExtensions', 'sourceFile', 'line']) {
  assertRequired(defs.generatedListReference, field);
}
for (const field of ['version', 'files', 'references', 'assets', 'missing', 'unused', 'graphicsPaths', 'graphicsExtensions']) {
  assertRequired(defs.assetManifest, field);
}
assertPropertyRef(defs.assetManifest, 'files', '#/$defs/assetFile');
for (const field of ['count', 'mathJaxCount', 'katexCount', 'svgFallbackCount', 'failedCount']) {
  assertRequired(defs.mathSummary, field);
}

const tsProtocolBody = tsProtocol;
for (const field of ['mathjax?: string', 'mathjaxOutput?: string', 'mathjaxFont?: string', 'mathJaxCount: number']) {
  if (!tsProtocolBody.includes(field)) {
    throw new Error(`Protocol TypeScript is missing ${field}`);
  }
}

const capabilitiesSchema = schemas.get('schemas/capabilities.schema.json');
assertRequired(capabilitiesSchema, 'async');
assertRequired(capabilitiesSchema, 'nodeWorkers');
assertRequired(capabilitiesSchema.properties.limits, 'finalOutputMaxBytes');
assertRequired(capabilitiesSchema.properties.versions, 'finalOutput');
for (const field of ['enabled', 'transport', 'contractVersion', 'mathJaxMode', 'count', 'maxRequestBytes', 'maxResponseBytes', 'maxTasks', 'maxRssBytes']) {
  assertRequired(capabilitiesSchema.properties.nodeWorkers, field);
}

const progressJobStatusSchema = schemas.get('schemas/job-status.schema.json');
assertRequired(progressJobStatusSchema, 'progress');
assertRequired(progressJobStatusSchema.properties.progress, 'completedStages');
assertRequired(progressJobStatusSchema.properties.progress, 'totalStages');
if (!progressJobStatusSchema.properties.elapsedSeconds) {
  throw new Error('Job status schema is missing running elapsedSeconds');
}
for (const field of ['outputKind', 'resourceClass', 'priorityClass', 'snapshotHash', 'sessionId', 'draftRevision', 'enginePolicyId', 'imageDigest', 'entrypoint', 'error']) {
  if (!progressJobStatusSchema.properties[field]) {
    throw new Error(`Job status schema is missing PDF preview field ${field}`);
  }
}
for (const field of ['documentMode', 'bookPages']) {
  if (!openapi.includes(`${field}:`) && !schemas.get('schemas/job-submit-request.schema.json')?.properties?.[field]) {
    throw new Error(`OpenAPI is missing Markdown Book field ${field}`);
  }
}
for (const field of ['enabled', 'queueScope', 'eventReplay', 'pollingFallback', 'cancellation', 'readiness', 'degraded']) {
  assertRequired(capabilitiesSchema.properties.async, field);
}

const finalOutputFixturesPath = path.join(root, '../final-output/fixtures/security-output.json');
const finalOutputFixtures = JSON.parse(fs.readFileSync(finalOutputFixturesPath, 'utf8'));
if (finalOutputFixtures.schemaVersion !== 'rin-final-output-fixtures/v1' ||
    finalOutputFixtures.validatorVersion !== 'rin-final-output/v1' ||
    !Array.isArray(finalOutputFixtures.cases) || finalOutputFixtures.cases.length < 15) {
  throw new Error('Shared final-output fixtures have an invalid contract');
}

const jobStatusSchema = schemas.get('schemas/job-status.schema.json');
for (const field of ['jobId', 'contentKind', 'documentEngine', 'state', 'createdAt', 'updatedAt', 'expiresAt', 'cancelRequested']) {
  assertRequired(jobStatusSchema, field);
}
// The job-status error projection is shared by draft previews and committed
// document jobs, so the bounded vocabulary must keep the codes a failed
// document job classifies: a rejected project source and a compiler rejection.
const jobFailureSchema = schemas.get('schemas/latex-pdf-preview-error.schema.json');
for (const code of ['compile_failed', 'timeout', 'source_invalid', 'canceled', 'unavailable']) {
  if (!jobFailureSchema.properties.code.enum.includes(code)) {
    throw new Error(`Job failure vocabulary is missing document failure code ${code}`);
  }
}
const jobSubmissionSchema = schemas.get('schemas/job-submit-response.schema.json');
for (const field of ['job', 'queue', 'location', 'reused']) {
  assertRequired(jobSubmissionSchema, field);
}
const projectAcceptedSchema = schemas.get('schemas/project-render-accepted.schema.json');
for (const field of ['requestId', 'jobId', 'state', 'location']) {
  assertRequired(projectAcceptedSchema, field);
}
for (const marker of ['Prefer', 'respond-async', 'Idempotency-Key', "'202'", "'504'"]) {
  if (!openapi.includes(marker)) throw new Error(`OpenAPI compatibility route is missing ${marker}`);
}
for (const marker of ['latex-pdf-preview', 'latex-pdf', 'preview', 'snapshotHash', 'sessionId', 'draftRevision', 'enginePolicyId', 'imageDigest']) {
  if (!openapi.includes(marker) && !JSON.stringify(schemas.get('schemas/job-submit-request.schema.json')).includes(`"${marker}"`)) {
    throw new Error(`PDF preview protocol is missing ${marker}`);
  }
}
const queueStatusSchema = schemas.get('schemas/queue-status.schema.json');
assertRequired(queueStatusSchema, 'estimate');
for (const field of ['estimatedStartAt', 'estimatedStartRange', 'confidence', 'sampleCount', 'estimatorVersion', 'scope', 'calculatedAt']) {
  if (!JSON.stringify(queueStatusSchema.properties.estimate).includes(`"${field}"`)) {
    throw new Error(`Queue estimate schema is missing ${field}`);
  }
}
const readinessSchema = schemas.get('schemas/readiness.schema.json');
for (const field of ['status', 'ready', 'degraded', 'components', 'checkedAt']) {
  assertRequired(readinessSchema, field);
}
for (const field of ['queuedProjects', 'activeProjects', 'scope', 'calculatedAt']) {
  assertRequired(queueStatusSchema, field);
}
const jobEventSchema = schemas.get('schemas/job-event.schema.json');
for (const field of ['id', 'type', 'stage', 'payload', 'createdAt']) {
  assertRequired(jobEventSchema, field);
}
const jobSupportSchema = schemas.get('schemas/job-support.schema.json');
for (const field of ['job', 'attempts', 'estimator', 'artifacts', 'scope', 'inspectedAt']) {
  assertRequired(jobSupportSchema, field);
}
const supportPropertyNames = new Set([
  ...Object.keys(jobSupportSchema.properties || {}),
  ...Object.keys(jobSupportSchema.properties.job?.properties || {}),
  ...Object.keys(jobSupportSchema.properties.attempts?.items?.properties || {}),
  ...Object.keys(jobSupportSchema.properties.estimator?.oneOf?.[1]?.properties || {}),
  ...Object.keys(jobSupportSchema.properties.artifacts?.items?.properties || {}),
]);
for (const forbidden of ['principalId', 'ownerScope', 'title', 'source', 'sourcePath', 'storageKey', 'workerId', 'leaseToken']) {
  if (supportPropertyNames.has(forbidden)) {
    throw new Error(`Job support schema exposes forbidden field ${forbidden}`);
  }
}

const resultSchema = schemas.get('schemas/render-result.schema.json');
if (resultSchema.$id !== 'https://rinspace.com/schemas/rin-renderer/rin-render-result-v1.schema.json') {
  throw new Error('Render Result schema has an unexpected $id');
}
for (const field of ['schemaVersion', 'jobId', 'requestId', 'projectHash', 'resultHash', 'contentKind', 'engine', 'assets', 'diagnostics', 'versions', 'cache']) {
  assertRequired(resultSchema, field);
}
if (!Array.isArray(resultSchema.oneOf) || resultSchema.oneOf.length !== 2) {
  throw new Error('Render Result schema must require exactly one result representation');
}
const resultFixtureDir = path.join(root, 'fixtures/render-result');
const resultFixtureFiles = fs.readdirSync(resultFixtureDir).filter((name) => name.endsWith('.json')).sort();
if (JSON.stringify(resultFixtureFiles) !== JSON.stringify(['artifact.json', 'inline.json'])) {
  throw new Error(`Unexpected Render Result fixtures: ${resultFixtureFiles.join(', ')}`);
}
for (const name of resultFixtureFiles) {
  validateRenderResult(JSON.parse(fs.readFileSync(path.join(resultFixtureDir, name), 'utf8')), name);
}

const pdfInspectionSchema = schemas.get('schemas/pdf-inspection.schema.json');
if (pdfInspectionSchema.properties?.contentKind?.const !== 'pdf' ||
    pdfInspectionSchema.properties?.engine?.const !== 'pdf-inspect' ||
    pdfInspectionSchema.properties?.originalReencoded?.const !== false ||
    !openapi.includes("$ref: './schemas/pdf-inspection.schema.json'")) {
  throw new Error('Existing pdf-inspect semantics changed while adding PDF preview');
}

const previewRequestSchema = schemas.get('schemas/job-submit-request.schema.json');
for (const field of ['source', 'contentKind', 'documentEngine', 'priorityIntent']) assertRequired(previewRequestSchema, field);
const requestFixtureDir = path.join(root, 'fixtures/job-submit');
for (const name of ['legacy-latex.json', 'legacy-markdown-book.json']) {
  validateJobSubmitRequest(JSON.parse(fs.readFileSync(path.join(requestFixtureDir, name), 'utf8')), name);
}
const previewSubmission = JSON.parse(fs.readFileSync(path.join(requestFixtureDir, 'latex-pdf-preview.json'), 'utf8'));
validatePreviewSubmission(previewSubmission, 'latex-pdf-preview.json');

const previewResultSchema = schemas.get('schemas/latex-pdf-preview-result.schema.json');
for (const field of ['schemaVersion', 'outputKind', 'resourceClass', 'priorityClass', 'snapshotHash', 'sessionId', 'draftRevision', 'enginePolicyId', 'imageDigest', 'idempotencyKey', 'entrypoint', 'workspaceRoot', 'artifacts', 'totalArtifactBytes']) {
  assertRequired(previewResultSchema, field);
}
const previewFixtureDir = path.join(root, 'fixtures/latex-pdf-preview');
const validPreviewResult = JSON.parse(fs.readFileSync(path.join(previewFixtureDir, 'result.json'), 'utf8'));
validateLatexPDFPreviewResult(validPreviewResult, 'latex-pdf-preview/result.json');
const invalidPreviewFixtures = JSON.parse(fs.readFileSync(path.join(previewFixtureDir, 'invalid-cases.json'), 'utf8'));
if (invalidPreviewFixtures.schemaVersion !== 'rin-latex-pdf-preview-invalid-fixtures/v1' || invalidPreviewFixtures.base !== 'result.json') {
  throw new Error('PDF preview invalid fixture manifest is malformed');
}
for (const fixture of invalidPreviewFixtures.cases || []) {
  const candidate = structuredClone(validPreviewResult);
  setFixturePath(candidate, fixture.path, fixture.value);
  let actualError = '';
  try {
    validateLatexPDFPreviewResult(candidate, fixture.name);
  } catch (error) {
    actualError = error?.contractCode || '';
  }
  if (actualError !== fixture.expectedError) {
    throw new Error(`${fixture.name}: expected ${fixture.expectedError}, received ${actualError || 'no rejection'}`);
  }
}

const typstResultSchema = schemas.get('schemas/typst-pdf-result.schema.json');
if (typstResultSchema.$id !== 'https://rinspace.com/schemas/rin-renderer/typst-pdf-result.schema.json') {
  throw new Error('Typst PDF result schema has an unexpected $id');
}
for (const field of ['schemaVersion', 'contentKind', 'outputKind', 'documentEngine', 'resourceClass', 'priorityClass', 'idempotencyKey', 'renderProfileId', 'entrypoint', 'workspaceRoot', 'artifacts', 'totalArtifactBytes']) {
  assertRequired(typstResultSchema, field);
}
validateJobSubmitRequest(JSON.parse(fs.readFileSync(path.join(requestFixtureDir, 'typst-document.json'), 'utf8')), 'typst-document.json');
validateTypstSubmission(JSON.parse(fs.readFileSync(path.join(requestFixtureDir, 'typst-pdf-preview.json'), 'utf8')), 'typst-pdf-preview.json');
const validTypstExportSubmission = JSON.parse(fs.readFileSync(path.join(requestFixtureDir, 'typst-pdf-export.json'), 'utf8'));
validateTypstExportSubmission(validTypstExportSubmission, 'typst-pdf-export.json');
const driftedExportSubmission = structuredClone(validTypstExportSubmission);
driftedExportSubmission.idempotencyKey = '0'.repeat(64);
let exportKeyRejected = false;
try {
  validateTypstExportSubmission(driftedExportSubmission, 'typst-pdf-export-drift.json');
} catch (error) {
  exportKeyRejected = error?.contractCode === 'idempotency_key';
}
if (!exportKeyRejected) throw new Error('Typst PDF export submission with a drifted Idempotency-Key was accepted');

const typstFixtureDir = path.join(root, 'fixtures/typst-pdf');
const validTypstPreview = JSON.parse(fs.readFileSync(path.join(typstFixtureDir, 'result-preview.json'), 'utf8'));
validateTypstPDFResult(validTypstPreview, 'typst-pdf/result-preview.json');
const validTypstExport = JSON.parse(fs.readFileSync(path.join(typstFixtureDir, 'result-export.json'), 'utf8'));
validateTypstPDFResult(validTypstExport, 'typst-pdf/result-export.json');
const invalidTypstFixtures = JSON.parse(fs.readFileSync(path.join(typstFixtureDir, 'invalid-cases.json'), 'utf8'));
if (invalidTypstFixtures.schemaVersion !== 'rin-typst-pdf-invalid-fixtures/v1' || invalidTypstFixtures.base !== 'result-preview.json') {
  throw new Error('Typst PDF invalid fixture manifest is malformed');
}
for (const fixture of invalidTypstFixtures.cases || []) {
  const candidate = structuredClone(validTypstPreview);
  setFixturePath(candidate, fixture.path, fixture.value);
  let actualError = '';
  try {
    validateTypstPDFResult(candidate, fixture.name);
  } catch (error) {
    actualError = error?.contractCode || '';
  }
  if (actualError !== fixture.expectedError) {
    throw new Error(`${fixture.name}: expected ${fixture.expectedError}, received ${actualError || 'no rejection'}`);
  }
}

console.log('Rin Renderer protocol files are consistent.');

function validateJobSubmitRequest(request, label) {
  const allowed = new Set([
    'source', 'contentKind', 'outputKind', 'documentEngine', 'entrypoint', 'documentMode', 'bookPages',
    'priorityIntent', 'resourceClass', 'priorityClass', 'snapshotHash', 'sessionId', 'draftRevision',
    'enginePolicyId', 'renderProfileId', 'imageDigest', 'previewContractVersion', 'controlProjectId', 'sourceCommit',
    'controlProjectHash', 'options',
  ]);
  if (!request || typeof request !== 'object' || Array.isArray(request)) throw contractError('request_type', `${label}: request must be an object`);
  for (const field of Object.keys(request)) if (!allowed.has(field)) throw contractError('request_field', `${label}: unknown field ${field}`);
  for (const field of ['source', 'contentKind', 'documentEngine', 'priorityIntent']) {
    if (typeof request[field] !== 'string' || !request[field]) throw contractError('request_field', `${label}: invalid ${field}`);
  }
  if (!['latex', 'markdown', 'typst'].includes(request.contentKind)) throw contractError('request_field', `${label}: invalid contentKind`);
  if (request.entrypoint !== undefined) assertCanonicalPath(request.entrypoint, `${label}: entrypoint`);
  if (request.outputKind === 'latex-pdf-preview') validatePreviewFields(request, label);
  else if (request.outputKind === 'typst-pdf-preview') validateTypstPreviewFields(request, label);
  else if (request.outputKind === 'typst-pdf-export') validateTypstExportFields(request, label);
  else if (request.contentKind === 'typst') validateTypstDocumentFields(request, label);
  else {
    if (!['publish', 'rebuild', 'migration'].includes(request.priorityIntent)) throw contractError('request_field', `${label}: legacy priority changed`);
    if (request.renderProfileId !== undefined) throw contractError('request_field', `${label}: renderProfileId is only valid for Typst document jobs`);
  }
}

function validatePreviewSubmission(submission, label) {
  if (!submission || typeof submission !== 'object' || typeof submission.idempotencyKey !== 'string') {
    throw contractError('idempotency_key', `${label}: invalid submission envelope`);
  }
  validateJobSubmitRequest(submission.fields, `${label}: fields`);
  const expected = previewIdempotencyKey(submission.fields);
  if (submission.idempotencyKey !== expected) throw contractError('idempotency_key', `${label}: Idempotency-Key does not match preview identity`);
}

function validatePreviewFields(fields, label) {
  const exact = {
    contentKind: 'latex',
    outputKind: 'latex-pdf-preview',
    documentEngine: 'latexmk',
    priorityIntent: 'preview',
    resourceClass: 'latex-pdf',
    priorityClass: 'preview',
    previewContractVersion: 'rin-latex-pdf-preview/v1',
  };
  for (const [field, value] of Object.entries(exact)) {
    if (fields[field] !== value) throw contractError('request_field', `${label}: ${field} must be ${value}`);
  }
  for (const field of ['snapshotHash', 'sessionId', 'enginePolicyId', 'imageDigest', 'entrypoint']) {
    if (typeof fields[field] !== 'string' || !fields[field]) throw contractError('request_field', `${label}: missing ${field}`);
  }
  if (!/^[a-f0-9]{64}$/.test(fields.snapshotHash) || !/^sha256:[a-f0-9]{64}$/.test(fields.imageDigest)) {
    throw contractError('request_field', `${label}: invalid snapshot or image digest`);
  }
  if (!/^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$/.test(fields.sessionId) || !/^[a-z0-9][a-z0-9._@-]{2,127}$/.test(fields.enginePolicyId)) {
    throw contractError('request_field', `${label}: invalid session or engine policy identity`);
  }
  if (!Number.isSafeInteger(fields.draftRevision) || fields.draftRevision < 0) throw contractError('request_field', `${label}: invalid draftRevision`);
  assertCanonicalPath(fields.entrypoint, `${label}: entrypoint`);
}

function previewIdempotencyKey(value) {
  return crypto.createHash('sha256')
    .update(value.sessionId + value.snapshotHash + value.entrypoint + value.enginePolicyId + value.imageDigest)
    .digest('hex');
}

function validateLatexPDFPreviewResult(result, label) {
  const exact = {
    schemaVersion: 'rin-latex-pdf-preview/v1', contentKind: 'latex', outputKind: 'latex-pdf-preview',
    documentEngine: 'latexmk', resourceClass: 'latex-pdf', priorityClass: 'preview', workspaceRoot: '/workspace',
  };
  for (const [field, value] of Object.entries(exact)) {
    if (result?.[field] !== value) throw contractError('result_identity', `${label}: ${field} must be ${value}`);
  }
  if (!/^[a-f0-9]{64}$/.test(result.snapshotHash) || !/^sha256:[a-f0-9]{64}$/.test(result.imageDigest)) {
    throw contractError('result_identity', `${label}: invalid snapshot or image digest`);
  }
  if (!/^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$/.test(result.sessionId) || !/^[a-z0-9][a-z0-9._@-]{2,127}$/.test(result.enginePolicyId)) {
    throw contractError('result_identity', `${label}: invalid session or engine policy identity`);
  }
  if (!Number.isSafeInteger(result.draftRevision) || result.draftRevision < 0) throw contractError('result_identity', `${label}: invalid draftRevision`);
  assertCanonicalPath(result.entrypoint, `${label}: entrypoint`);
  if (result.idempotencyKey !== previewIdempotencyKey(result)) throw contractError('idempotency_key', `${label}: invalid idempotencyKey`);
  if (!Array.isArray(result.artifacts) || result.artifacts.length < 5 || result.artifacts.length > 16) throw contractError('artifact_count', `${label}: invalid artifact count`);

  const allowedKinds = new Set(['pdf', 'synctex', 'log', 'aux', 'fls', 'bbl', 'blg', 'toc', 'out']);
  const suffixes = { pdf: '.pdf', synctex: '.synctex.gz', log: '.log', aux: '.aux', fls: '.fls', bbl: '.bbl', blg: '.blg', toc: '.toc', out: '.out' };
  const mediaTypes = { pdf: 'application/pdf', synctex: 'application/gzip', log: 'text/plain; charset=utf-8', aux: 'text/plain; charset=utf-8', fls: 'text/plain; charset=utf-8', bbl: 'text/plain; charset=utf-8', blg: 'text/plain; charset=utf-8', toc: 'text/plain; charset=utf-8', out: 'text/plain; charset=utf-8' };
  const entrypointDirectory = path.posix.dirname(result.entrypoint);
  const outputPrefix = entrypointDirectory === '.' ? '.rinspace/' : `${entrypointDirectory}/.rinspace/`;
  const kinds = new Set();
  let total = 0;
  for (const [index, artifact] of result.artifacts.entries()) {
    if (!artifact || !allowedKinds.has(artifact.kind)) throw contractError('artifact_kind', `${label}: artifact ${index} has invalid kind`);
    if (kinds.has(artifact.kind)) throw contractError('artifact_kind', `${label}: duplicate ${artifact.kind} artifact`);
    kinds.add(artifact.kind);
    try {
      assertCanonicalPath(artifact.artifactId, `${label}: artifact ${index} id`);
      assertCanonicalPath(artifact.path, `${label}: artifact ${index} path`);
    } catch {
      throw contractError('artifact_path', `${label}: artifact ${index} path is not canonical`);
    }
    if (!artifact.path.startsWith(outputPrefix) || artifact.path.slice(outputPrefix.length).includes('/') || !artifact.path.endsWith(suffixes[artifact.kind])) {
      throw contractError('artifact_path', `${label}: artifact ${index} must stay in root-adjacent .rinspace`);
    }
    if (artifact.mediaType !== mediaTypes[artifact.kind] || artifact.visibility !== 'private' || !/^[a-f0-9]{64}$/.test(artifact.sha256)) {
      throw contractError('artifact_kind', `${label}: artifact ${index} metadata does not match kind`);
    }
    const maxBytes = artifact.kind === 'log' ? 2 * 1024 * 1024 : artifact.kind === 'pdf' ? 32 * 1024 * 1024 : artifact.kind === 'synctex' ? 32 * 1024 * 1024 : 8 * 1024 * 1024;
    if (!Number.isSafeInteger(artifact.bytes) || artifact.bytes < 1 || artifact.bytes > maxBytes) {
      throw contractError('artifact_size', `${label}: artifact ${index} exceeds its size contract`);
    }
    total += artifact.bytes;
  }
  for (const requiredKind of ['pdf', 'synctex', 'log', 'aux', 'fls']) {
    if (!kinds.has(requiredKind)) throw contractError('artifact_kind', `${label}: ${requiredKind} artifact is required`);
  }
  if (!Number.isSafeInteger(result.totalArtifactBytes) || result.totalArtifactBytes !== total || total > 48 * 1024 * 1024) {
    throw contractError('artifact_total', `${label}: artifact total is invalid`);
  }
  if (!Array.isArray(result.diagnostics) || !result.cache || typeof result.cache.hit !== 'boolean' || !result.versions) {
    throw contractError('result_shape', `${label}: result metadata is invalid`);
  }
}

function validateTypstPreviewFields(fields, label) {
  const exact = {
    contentKind: 'typst', outputKind: 'typst-pdf-preview', documentEngine: 'typst',
    priorityIntent: 'preview', resourceClass: 'typst-pdf', priorityClass: 'preview',
    previewContractVersion: 'rin-typst-pdf-preview/v1',
  };
  for (const [field, value] of Object.entries(exact)) {
    if (fields[field] !== value) throw contractError('request_field', `${label}: ${field} must be ${value}`);
  }
  for (const field of ['snapshotHash', 'sessionId', 'enginePolicyId', 'imageDigest', 'entrypoint']) {
    if (typeof fields[field] !== 'string' || !fields[field]) throw contractError('request_field', `${label}: missing ${field}`);
  }
  if (!/^[a-f0-9]{64}$/.test(fields.snapshotHash) || !/^sha256:[a-f0-9]{64}$/.test(fields.imageDigest)) {
    throw contractError('request_field', `${label}: invalid snapshot or image digest`);
  }
  if (!/^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$/.test(fields.sessionId) || !/^[a-z0-9][a-z0-9._@-]{2,127}$/.test(fields.enginePolicyId)) {
    throw contractError('request_field', `${label}: invalid session or engine policy identity`);
  }
  if (!Number.isSafeInteger(fields.draftRevision) || fields.draftRevision < 0) throw contractError('request_field', `${label}: invalid draftRevision`);
  assertTypstEntrypoint(fields.entrypoint, `${label}: entrypoint`);
}

function validateTypstExportFields(fields, label) {
  const exact = { contentKind: 'typst', outputKind: 'typst-pdf-export', documentEngine: 'typst', resourceClass: 'typst-pdf' };
  for (const [field, value] of Object.entries(exact)) {
    if (fields[field] !== value) throw contractError('request_field', `${label}: ${field} must be ${value}`);
  }
  if (!['publish', 'rebuild'].includes(fields.priorityIntent) || !['publish', 'rebuild'].includes(fields.priorityClass)) {
    throw contractError('request_field', `${label}: export priority must be publish or rebuild`);
  }
  for (const field of ['controlProjectId', 'sourceCommit', 'controlProjectHash', 'entrypoint', 'enginePolicyId', 'imageDigest']) {
    if (typeof fields[field] !== 'string' || !fields[field]) throw contractError('request_field', `${label}: missing ${field}`);
  }
  if (!/^[a-f0-9]{40}(?:[a-f0-9]{24})?$/.test(fields.sourceCommit) || !/^[a-f0-9]{64}$/.test(fields.controlProjectHash)) {
    throw contractError('request_field', `${label}: invalid export commit identity`);
  }
  if (!/^[a-z0-9][a-z0-9._@-]{2,127}$/.test(fields.enginePolicyId) || !/^sha256:[a-f0-9]{64}$/.test(fields.imageDigest)) {
    throw contractError('request_field', `${label}: invalid export engine policy or image digest`);
  }
  assertCanonicalPath(fields.controlProjectId, `${label}: controlProjectId`);
  assertTypstEntrypoint(fields.entrypoint, `${label}: entrypoint`);
}

function validateTypstDocumentFields(fields, label) {
  const exact = { contentKind: 'typst', documentEngine: 'typst', resourceClass: 'document-typst' };
  for (const [field, value] of Object.entries(exact)) {
    if (fields[field] !== value) throw contractError('request_field', `${label}: ${field} must be ${value}`);
  }
  if (typeof fields.renderProfileId !== 'string' || !/^[a-z0-9][a-z0-9._@-]{2,127}$/.test(fields.renderProfileId)) {
    throw contractError('request_field', `${label}: renderProfileId must pin the resolved Typst HTML render profile`);
  }
  assertTypstEntrypoint(fields.entrypoint, `${label}: entrypoint`);
}

function validateTypstSubmission(submission, label) {
  if (!submission || typeof submission !== 'object' || typeof submission.idempotencyKey !== 'string') {
    throw contractError('idempotency_key', `${label}: invalid submission envelope`);
  }
  validateJobSubmitRequest(submission.fields, `${label}: fields`);
  if (submission.fields.outputKind !== 'typst-pdf-preview') throw contractError('request_field', `${label}: not a Typst PDF preview submission`);
  const expected = previewIdempotencyKey(submission.fields);
  if (submission.idempotencyKey !== expected) throw contractError('idempotency_key', `${label}: Idempotency-Key does not match preview identity`);
}

function validateTypstExportSubmission(submission, label) {
  if (!submission || typeof submission !== 'object' || typeof submission.idempotencyKey !== 'string') {
    throw contractError('idempotency_key', `${label}: invalid submission envelope`);
  }
  validateJobSubmitRequest(submission.fields, `${label}: fields`);
  if (submission.fields.outputKind !== 'typst-pdf-export') throw contractError('request_field', `${label}: not a Typst PDF export submission`);
  const expected = exportIdempotencyKey(submission.fields);
  if (submission.idempotencyKey !== expected) throw contractError('idempotency_key', `${label}: Idempotency-Key does not match export identity`);
}

function assertTypstEntrypoint(value, label) {
  assertCanonicalPath(value, label);
  if (!value.toLowerCase().endsWith('.typ')) throw contractError('request_field', `${label}: Typst entrypoint must end with .typ`);
}

function isCanonicalPath(value) {
  if (typeof value !== 'string' || !value || value.includes('\\') || value.startsWith('/') || /^[A-Za-z]:/.test(value)) return false;
  return !value.split('/').some((part) => !part || part === '.' || part === '..');
}

// exportIdempotencyKey mirrors the Go contract: a committed export is keyed by
// its published commit identity and its pinned render profile so a rebuild
// under a new profile produces a new attempt.
function exportIdempotencyKey(value) {
  return crypto.createHash('sha256')
    .update([value.controlProjectId, value.sourceCommit, value.entrypoint, value.enginePolicyId, value.imageDigest].join('\u0000'))
    .digest('hex');
}

function validateTypstPDFResult(result, label) {
  const exact = {
    schemaVersion: 'rin-typst-pdf/v1', contentKind: 'typst', documentEngine: 'typst',
    resourceClass: 'typst-pdf', workspaceRoot: '/workspace',
  };
  for (const [field, value] of Object.entries(exact)) {
    if (result?.[field] !== value) throw contractError('result_identity', `${label}: ${field} must be ${value}`);
  }
  if (!['typst-pdf-preview', 'typst-pdf-export'].includes(result.outputKind)) throw contractError('result_identity', `${label}: invalid outputKind`);
  if (typeof result.renderProfileId !== 'string' || !/^[a-z0-9][a-z0-9._@-]{2,127}$/.test(result.renderProfileId)) {
    throw contractError('result_identity', `${label}: invalid renderProfileId`);
  }
  if (typeof result.entrypoint !== 'string' || !isCanonicalPath(result.entrypoint) || !result.entrypoint.toLowerCase().endsWith('.typ')) {
    throw contractError('result_identity', `${label}: invalid entrypoint`);
  }
  if (typeof result.idempotencyKey !== 'string' || !/^[a-f0-9]{64}$/.test(result.idempotencyKey)) throw contractError('idempotency_key', `${label}: invalid idempotencyKey`);
  if (result.outputKind === 'typst-pdf-preview') {
    if (result.priorityClass !== 'preview') throw contractError('result_identity', `${label}: preview priority must be preview`);
    if (!/^[a-f0-9]{64}$/.test(result.snapshotHash || '') || !/^sha256:[a-f0-9]{64}$/.test(result.imageDigest || '')) {
      throw contractError('result_identity', `${label}: invalid snapshot or image digest`);
    }
    if (!/^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$/.test(result.sessionId || '') || !/^[a-z0-9][a-z0-9._@-]{2,127}$/.test(result.enginePolicyId || '')) {
      throw contractError('result_identity', `${label}: invalid session or engine policy identity`);
    }
    if (!Number.isSafeInteger(result.draftRevision) || result.draftRevision < 0) throw contractError('result_identity', `${label}: invalid draftRevision`);
    if (result.idempotencyKey !== previewIdempotencyKey(result)) throw contractError('idempotency_key', `${label}: invalid preview idempotencyKey`);
    if (result.sourceCommit !== undefined || result.controlProjectId !== undefined || result.controlProjectHash !== undefined) {
      throw contractError('result_identity', `${label}: preview must not carry a project commit identity`);
    }
  } else {
    if (!['publish', 'rebuild'].includes(result.priorityClass)) throw contractError('result_identity', `${label}: export priority must be publish or rebuild`);
    if (!/^[a-f0-9]{40}(?:[a-f0-9]{24})?$/.test(result.sourceCommit || '') || !/^[a-f0-9]{64}$/.test(result.controlProjectHash || '')) {
      throw contractError('result_identity', `${label}: invalid export commit identity`);
    }
    if (!isCanonicalPath(result.controlProjectId)) throw contractError('result_identity', `${label}: invalid controlProjectId`);
    if (!/^[a-z0-9][a-z0-9._@-]{2,127}$/.test(result.enginePolicyId || '') || !/^sha256:[a-f0-9]{64}$/.test(result.imageDigest || '')) {
      throw contractError('result_identity', `${label}: invalid export engine policy or image digest`);
    }
    if (result.sessionId !== undefined || result.snapshotHash !== undefined || result.draftRevision !== undefined) {
      throw contractError('result_identity', `${label}: export must not carry a session identity`);
    }
    if (result.idempotencyKey !== exportIdempotencyKey(result)) throw contractError('idempotency_key', `${label}: invalid export idempotencyKey`);
  }
  if (!Array.isArray(result.artifacts) || result.artifacts.length < 1 || result.artifacts.length > 8) throw contractError('artifact_count', `${label}: invalid artifact count`);
  const rules = {
    pdf: { suffix: '.pdf', mediaType: 'application/pdf', max: 33554432 },
    log: { suffix: '.log', mediaType: 'text/plain; charset=utf-8', max: 2097152 },
  };
  const kinds = new Set();
  let total = 0;
  for (const artifact of result.artifacts) {
    const rule = rules[artifact.kind];
    if (!rule) throw contractError('artifact_kind', `${label}: unknown artifact kind ${artifact.kind}`);
    if (kinds.has(artifact.kind)) throw contractError('artifact_kind', `${label}: duplicate artifact kind ${artifact.kind}`);
    kinds.add(artifact.kind);
    if (typeof artifact.path !== 'string' || !isCanonicalPath(artifact.path) ||
        !/^(?:[^\/]+\/)*\.rinspace\/typst\/[^\/]+\.(?:pdf|log)$/.test(artifact.path) || !artifact.path.endsWith(rule.suffix)) {
      throw contractError('artifact_path', `${label}: invalid artifact path`);
    }
    if (!isCanonicalPath(artifact.artifactId)) throw contractError('artifact_path', `${label}: invalid artifactId`);
    if (!/^[a-f0-9]{64}$/.test(artifact.sha256 || '') || !Number.isSafeInteger(artifact.bytes) || artifact.bytes < 1 || artifact.bytes > rule.max) {
      throw contractError('artifact_size', `${label}: artifact bytes exceed its size contract`);
    }
    if (artifact.mediaType !== rule.mediaType || artifact.visibility !== 'private' || typeof artifact.expiresAt !== 'string') {
      throw contractError('artifact_media', `${label}: invalid artifact type or visibility`);
    }
    total += artifact.bytes;
  }
  if (!kinds.has('pdf')) throw contractError('artifact_kind', `${label}: pdf artifact is required`);
  if (!Number.isSafeInteger(result.totalArtifactBytes) || result.totalArtifactBytes !== total || total > 48 * 1024 * 1024) {
    throw contractError('artifact_total', `${label}: artifact total is invalid`);
  }
  if (!Array.isArray(result.diagnostics) || !result.cache || typeof result.cache.hit !== 'boolean' || !result.versions) {
    throw contractError('result_shape', `${label}: result metadata is invalid`);
  }
  for (const field of ['rinRenderer', 'typst', 'compilerImage']) {
    if (typeof result.versions[field] !== 'string' || !result.versions[field]) throw contractError('result_shape', `${label}: versions.${field} is required`);
  }
  if (!/^sha256:[a-f0-9]{64}$/.test(result.versions.compilerImage)) throw contractError('result_shape', `${label}: compilerImage must be a sha256 digest`);
}

function setFixturePath(target, parts, value) {
  if (!Array.isArray(parts) || parts.length === 0) throw new Error('Invalid fixture mutation path');
  let cursor = target;
  for (const part of parts.slice(0, -1)) cursor = cursor[part];
  cursor[parts.at(-1)] = value;
}

function contractError(contractCode, message) {
  const error = new Error(message);
  error.contractCode = contractCode;
  return error;
}

function validateRenderResult(result, label) {
  if (result.schemaVersion !== 'rin-render-result/v1') throw new Error(`${label}: invalid schemaVersion`);
  for (const field of ['jobId', 'requestId', 'engine']) {
    if (typeof result[field] !== 'string' || !result[field].trim()) throw new Error(`${label}: invalid ${field}`);
  }
  for (const field of ['projectHash', 'resultHash']) {
    if (typeof result[field] !== 'string' || !/^[a-f0-9]{64}$/.test(result[field])) throw new Error(`${label}: invalid ${field}`);
  }
  if (!['latex', 'markdown', 'typst'].includes(result.contentKind)) throw new Error(`${label}: invalid contentKind`);
  if ((result.inline === undefined) === (result.resultArtifact === undefined)) {
    throw new Error(`${label}: exactly one of inline and resultArtifact is required`);
  }
  if (result.inline !== undefined) {
    validateDocumentBundle(result.inline, `${label}: inline`);
    if (result.inline.state !== 'final' || result.inline.projectHash !== result.projectHash || result.inline.contentKind !== result.contentKind) {
      throw new Error(`${label}: inline bundle identity does not match result`);
    }
    if (result.inline.bundleHash !== result.resultHash) throw new Error(`${label}: inline bundle hash does not match result`);
  } else {
    validateArtifact(result.resultArtifact, `${label}: resultArtifact`);
    if (result.resultArtifact.visibility !== 'private') throw new Error(`${label}: resultArtifact must be private`);
    if (result.resultArtifact.sha256 !== result.resultHash || !result.resultArtifact.expiresAt) throw new Error(`${label}: resultArtifact identity or expiry is invalid`);
  }
  if (!Array.isArray(result.assets) || !Array.isArray(result.diagnostics)) throw new Error(`${label}: invalid result arrays`);
  result.assets.forEach((artifact, index) => validateArtifact(artifact, `${label}: asset ${index}`));
  if (!result.versions || typeof result.versions !== 'object' || Object.keys(result.versions).length === 0) throw new Error(`${label}: versions are required`);
  if (!result.cache || typeof result.cache.hit !== 'boolean' || !Array.isArray(result.cache.reusedStages)) throw new Error(`${label}: invalid cache summary`);
  if (new Set(result.cache.reusedStages).size !== result.cache.reusedStages.length) throw new Error(`${label}: duplicate reused stage`);
}

function validateArtifact(artifact, label) {
  if (!artifact || typeof artifact.artifactId !== 'string' || !artifact.artifactId.trim()) throw new Error(`${label}: invalid artifactId`);
  assertCanonicalPath(artifact.artifactId, `${label}: artifactId`);
  if (!/^[a-f0-9]{64}$/.test(artifact.sha256) || !Number.isSafeInteger(artifact.bytes) || artifact.bytes < 0) throw new Error(`${label}: invalid artifact metadata`);
  if (typeof artifact.mediaType !== 'string' || !artifact.mediaType.trim() || !['private', 'public'].includes(artifact.visibility)) throw new Error(`${label}: invalid artifact type`);
}

function assertCanonicalPath(value, label) {
  if (typeof value !== 'string' || !value || value.includes('\\') || value.startsWith('/') || /^[A-Za-z]:/.test(value)) {
    throw new Error(`${label} is not canonical`);
  }
  if (value.split('/').some((part) => !part || part === '.' || part === '..')) {
    throw new Error(`${label} is not canonical`);
  }
}

function assertObject(value, label) {
  if (!value || typeof value !== 'object') {
    throw new Error(`${label} is missing`);
  }
}

function assertRequired(schema, field) {
  if (!Array.isArray(schema.required) || !schema.required.includes(field)) {
    throw new Error(`${schema.title || 'schema'} is missing required field: ${field}`);
  }
}

function assertPropertyRef(schema, field, ref) {
  const property = schema.properties?.[field];
  const items = property?.items;
  if (!property || property.type !== 'array' || items?.$ref !== ref) {
    throw new Error(`${field} must be an array of ${ref}`);
  }
}
