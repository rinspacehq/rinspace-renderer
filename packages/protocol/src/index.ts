export type RinRendererSeverity = 'info' | 'warning' | 'error';

import type { RinDiagnostic, RinDocumentBundle } from '../../document-contract/src/index';

export const RIN_RENDER_RESULT_SCHEMA_VERSION = 'rin-render-result/v1' as const;
export const RIN_FINAL_OUTPUT_CONTRACT_VERSION = 'rin-final-output/v1' as const;
export const RIN_DIAGRAM_BATCH_CONTRACT_VERSION = 'rin-diagram-batch/v1' as const;
export const RIN_DIAGRAM_RESULT_CONTRACT_VERSION = 'rin-diagram-result/v1' as const;
export const RIN_DIAGRAM_CACHE_SCHEMA_VERSION = 'rin-diagram-cache/v1' as const;
export const RIN_DIAGRAM_ARTIFACT_CONTRACT_VERSION = 'rin-diagram-artifact/v1' as const;
export const RIN_PDF_INSPECTION_SCHEMA_VERSION = 'rin-pdf-inspection/v1' as const;
export const RIN_LATEX_PDF_PREVIEW_SCHEMA_VERSION = 'rin-latex-pdf-preview/v1' as const;
export const RIN_TYPST_PDF_SCHEMA_VERSION = 'rin-typst-pdf/v1' as const;

export interface RinPDFTOCEntry {
  title: string;
  page?: number;
}

export interface RinPDFPageDerivative {
  sourcePage: number;
  mediaType: 'image/webp' | string;
  purpose: 'cover' | `preview-${number}`;
}

export interface RinPDFInspectionV1 {
  schemaVersion: typeof RIN_PDF_INSPECTION_SCHEMA_VERSION;
  contentKind: 'pdf';
  engine: 'pdf-inspect';
  sourceSha256: string;
  sourceBytes: number;
  pdfVersion: string;
  pageCount: number;
  toc: RinPDFTOCEntry[];
  derivatives: RinPDFPageDerivative[];
  originalReencoded: false;
}

export interface RinArtifactReference {
  artifactId: string;
  sha256: string;
  bytes: number;
  mediaType: string;
  visibility: 'private' | 'public';
  expiresAt?: string;
}

export interface RinRenderCacheSummary {
  hit: boolean;
  reusedStages: string[];
}

export interface RinRenderResultV1 {
  schemaVersion: typeof RIN_RENDER_RESULT_SCHEMA_VERSION;
  jobId: string;
  requestId: string;
  projectHash: string;
  resultHash: string;
  contentKind: 'latex' | 'markdown' | 'typst' | 'pdf';
  engine: string;
  inline?: RinDocumentBundle;
  resultArtifact?: RinArtifactReference;
  assets: RinArtifactReference[];
  diagnostics: RinDiagnostic[];
  versions: Record<string, string>;
  cache: RinRenderCacheSummary;
}

export interface RinRendererDiagnostic {
  severity: RinRendererSeverity;
  code: string;
  message: string;
  engine?: string;
  source?: Record<string, string>;
}

export interface RinRendererVersions {
  rinRenderer: string;
  finalOutput: string;
  latexml?: string;
  latexmlAdapter?: string;
  perl?: string;
  texlive?: string;
  dvisvgm?: string;
  diagramEngine?: string;
  mathjax?: string;
  mathjaxOutput?: string;
  mathjaxFont?: string;
  katex?: string;
}

export interface RinRendererLimits {
  projectArchiveMaxBytes: number;
  projectFileMaxCount: number;
  projectFileMaxBytes: number;
  diagramMaxBytes: number;
  finalOutputMaxBytes: number;
  renderTimeoutSeconds: number;
  diagramTimeoutSeconds: number;
  diagramParallelism: number;
  mathSourceMaxBytes: number;
  mathTexSvgFallbackMaxCount: number;
  mathTimeoutSeconds: number;
  mathParallelism: number;
}

export interface RinRendererCapabilities {
  version: string;
  engines: {
    document: string[];
    pdf: string[];
    diagram: string[];
    math: string[];
  };
  diagramTypes: string[];
  limits: RinRendererLimits;
  versions: RinRendererVersions;
  storage: {
    provider: 'cloudbase' | string;
    bucket: string;
    baseUrl?: string;
  };
  async: {
    enabled: boolean;
    queueScope: 'instance' | 'cluster';
    eventReplay: boolean;
    pollingFallback: boolean;
    cancellation: boolean;
    readiness: 'ready' | 'degraded';
    degraded: string[];
  };
  nodeWorkers: RinRendererNodeWorkers;
}

export interface RinRendererNodeWorkers {
  enabled: boolean;
  transport: 'ndjson-stdio' | 'ndjson-unix';
  contractVersion: 'rin-node-worker/v1';
  mathJaxMode: 'process-per-batch' | 'warm';
  count: number;
  maxRequestBytes: number;
  maxResponseBytes: number;
  maxTasks: number;
  maxRssBytes: number;
}

export interface RinRendererReadiness {
  status: 'ready' | 'degraded';
  ready: boolean;
  degraded: string[];
  components: Record<string, { ready: boolean; reason?: string }>;
  checkedAt: string;
}

export type RinRendererJobState =
  | 'queued'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'canceled'
  | 'expired';

export interface RinRendererQueueStatus {
  jobsAheadEstimate?: number;
  queuedProjects: number;
  activeProjects: number;
  estimate: RinRendererWaitEstimate | null;
  scope: 'instance' | 'cluster';
  calculatedAt: string;
}

export interface RinRendererWaitEstimate {
  estimatedStartAt: string;
  estimatedStartRange: {
    earliest: string;
    latest: string;
  };
  confidence: 'low' | 'medium' | 'high';
  sampleCount: number;
  estimatorVersion: 'rin-wait-estimator/v1' | string;
  scope: 'instance' | 'cluster';
  calculatedAt: string;
}

export interface RinRendererJobStatus {
  jobId: string;
  contentKind: 'latex' | 'markdown' | 'typst';
  outputKind?: 'document' | 'latex-pdf-preview' | 'typst-pdf-preview' | 'typst-pdf-export';
  documentEngine: string;
  resourceClass?: string;
  priorityClass?: 'publish' | 'preview' | 'rebuild' | 'migration';
  snapshotHash?: string;
  sessionId?: string;
  draftRevision?: number;
  enginePolicyId?: string;
  renderProfileId?: string;
  imageDigest?: string;
  entrypoint?: string;
  state: RinRendererJobState;
  stage: string;
  progress: {
    completedStages: number;
    totalStages: number;
  };
  elapsedSeconds?: number;
  createdAt: string;
  updatedAt: string;
  queuedAt?: string;
  startedAt?: string;
  finishedAt?: string;
  expiresAt: string;
  cancelRequested: boolean;
  error?: RinLatexPDFPreviewError;
  queue?: RinRendererQueueStatus;
}

export interface RinRendererJobSubmission {
  job: RinRendererJobStatus;
  queue: RinRendererQueueStatus;
  location: string;
  reused: boolean;
}

export interface RinMarkdownBookPage {
  path: string;
  id: string;
  title?: string;
}

export interface RinRendererJobSubmitFields {
  contentKind: 'latex' | 'markdown' | 'typst';
  outputKind?: 'document' | 'latex-pdf-preview' | 'typst-pdf-preview' | 'typst-pdf-export';
  documentEngine: 'auto' | 'latexml' | 'unified' | 'latexmk';
  priorityIntent: 'publish' | 'preview' | 'rebuild' | 'migration';
  resourceClass?: string;
  priorityClass?: 'publish' | 'preview' | 'rebuild' | 'migration';
  snapshotHash?: string;
  sessionId?: string;
  draftRevision?: number;
  enginePolicyId?: string;
  renderProfileId?: string;
  imageDigest?: string;
  previewContractVersion?: typeof RIN_LATEX_PDF_PREVIEW_SCHEMA_VERSION;
  entrypoint?: string;
  documentMode?: 'article' | 'book';
  bookPages?: RinMarkdownBookPage[];
  options?: string;
}

export interface RinRendererLatexPDFPreviewJobSubmitFields extends RinRendererJobSubmitFields {
  contentKind: 'latex';
  outputKind: 'latex-pdf-preview';
  documentEngine: 'latexmk';
  priorityIntent: 'preview';
  resourceClass: 'latex-pdf';
  priorityClass: 'preview';
  snapshotHash: string;
  sessionId: string;
  draftRevision: number;
  enginePolicyId: string;
  imageDigest: `sha256:${string}`;
  previewContractVersion: typeof RIN_LATEX_PDF_PREVIEW_SCHEMA_VERSION;
  entrypoint: string;
}

export interface RinRendererLatexPDFPreviewSubmission {
  idempotencyKey: string;
  fields: RinRendererLatexPDFPreviewJobSubmitFields;
}

export type RinLatexPDFPreviewArtifactKind =
  | 'pdf'
  | 'synctex'
  | 'log'
  | 'aux'
  | 'fls'
  | 'bbl'
  | 'blg'
  | 'toc'
  | 'out';

export interface RinLatexPDFPreviewArtifact {
  artifactId: string;
  kind: RinLatexPDFPreviewArtifactKind;
  path: string;
  sha256: string;
  bytes: number;
  mediaType: 'application/pdf' | 'application/gzip' | 'text/plain; charset=utf-8';
  visibility: 'private';
  expiresAt: string;
}

export interface RinLatexPDFPreviewError {
  code:
    | 'snapshot_invalid'
    | 'snapshot_hash_mismatch'
    | 'entrypoint_invalid'
    | 'source_invalid'
    | 'idempotency_conflict'
    | 'queue_full'
    | 'superseded'
    | 'compile_failed'
    | 'timeout'
    | 'resource_exhausted'
    | 'artifact_invalid'
    | 'canceled'
    | 'unavailable';
  category: 'input' | 'conflict' | 'admission' | 'compile' | 'limit' | 'cancel' | 'internal';
  retryable: boolean;
  message: string;
  exitCode?: number;
}

export interface RinLatexPDFPreviewResultV1 {
  schemaVersion: typeof RIN_LATEX_PDF_PREVIEW_SCHEMA_VERSION;
  jobId: string;
  requestId: string;
  contentKind: 'latex';
  outputKind: 'latex-pdf-preview';
  documentEngine: 'latexmk';
  resourceClass: 'latex-pdf';
  priorityClass: 'preview';
  snapshotHash: string;
  sessionId: string;
  draftRevision: number;
  enginePolicyId: string;
  imageDigest: `sha256:${string}`;
  idempotencyKey: string;
  entrypoint: string;
  workspaceRoot: '/workspace';
  artifacts: RinLatexPDFPreviewArtifact[];
  totalArtifactBytes: number;
  diagnostics: RinRendererDiagnostic[];
  cache: { hit: boolean; reusedJobId?: string };
  versions: {
    rinRenderer: string;
    latexmk: string;
    texlive: string;
    compilerImage: `sha256:${string}`;
  };
}

export interface RinRendererTypstPDFJobSubmitFields extends RinRendererJobSubmitFields {
  contentKind: 'typst';
  outputKind: 'typst-pdf-preview' | 'typst-pdf-export';
  documentEngine: 'typst';
  resourceClass: 'typst-pdf';
  priorityClass: 'preview' | 'publish' | 'rebuild';
  entrypoint: string;
  snapshotHash?: string;
  sessionId?: string;
  draftRevision?: number;
  enginePolicyId?: string;
  imageDigest?: `sha256:${string}`;
  previewContractVersion?: typeof RIN_TYPST_PDF_SCHEMA_VERSION;
  controlProjectId?: string;
  sourceCommit?: string;
  controlProjectHash?: string;
}

export interface RinRendererTypstPDFSubmission {
  idempotencyKey: string;
  fields: RinRendererTypstPDFJobSubmitFields;
}

export type RinTypstPDFArtifactKind = 'pdf' | 'log';

export interface RinTypstPDFArtifact {
  artifactId: string;
  kind: RinTypstPDFArtifactKind;
  path: string;
  sha256: string;
  bytes: number;
  mediaType: 'application/pdf' | 'text/plain; charset=utf-8';
  visibility: 'private';
  expiresAt: string;
}

export interface RinTypstPDFResultV1 {
  schemaVersion: typeof RIN_TYPST_PDF_SCHEMA_VERSION;
  jobId: string;
  requestId: string;
  contentKind: 'typst';
  outputKind: 'typst-pdf-preview' | 'typst-pdf-export';
  documentEngine: 'typst';
  resourceClass: 'typst-pdf';
  priorityClass: 'preview' | 'rebuild' | 'publish';
  idempotencyKey: string;
  renderProfileId: string;
  entrypoint: string;
  workspaceRoot: '/workspace';
  snapshotHash?: string;
  sessionId?: string;
  draftRevision?: number;
  enginePolicyId?: string;
  imageDigest?: `sha256:${string}`;
  controlProjectId?: string;
  sourceCommit?: string;
  controlProjectHash?: string;
  artifacts: RinTypstPDFArtifact[];
  totalArtifactBytes: number;
  diagnostics: RinRendererDiagnostic[];
  cache: { hit: boolean; reusedJobId?: string };
  versions: {
    rinRenderer: string;
    typst: string;
    compilerImage: `sha256:${string}`;
  };
}

export interface RinRendererProjectAccepted {
  requestId: string;
  jobId: string;
  state: RinRendererJobState;
  location: string;
}

export type RinRendererTerminalState = 'succeeded' | 'failed' | 'canceled';

export interface RinRendererCompletionV2 {
  schema: 'rin-control-event/v1';
  event_id: string;
  type: 'renderer.job.completed';
  occurred_at: string;
  producer: 'rin-renderer';
  correlation_id: string;
  subject: {
    rendererJobId: string;
  };
  data: {
    schemaVersion: 'rin-renderer-completion/v2';
    protocolVersion: 'v2';
    terminalVersion: number;
    rendererJobId: string;
    requestId: string;
    controlProjectId: string;
    sourceCommit: string;
    controlProjectHash: string;
    rendererProjectHash: string;
    terminalState: RinRendererTerminalState;
    resultHash?: string;
    failureCode?: string;
  };
}

export interface RinRendererJobEvent {
  id: number;
  type: string;
  stage: string;
  payload: Record<string, unknown>;
  createdAt: string;
}

export interface RinRendererJobSupport {
  job: {
    jobId: string;
    contentKind: 'latex' | 'markdown' | 'typst';
    outputKind?: 'document' | 'latex-pdf-preview' | 'typst-pdf-preview' | 'typst-pdf-export';
    documentEngine: string;
    resourceClass: string;
    priorityClass: 'publish' | 'preview' | 'rebuild' | 'migration';
    state: RinRendererJobState;
    rendererVersion: string;
    maxAttempts: number;
    attemptCount: number;
    queuedAt?: string;
    startedAt?: string;
    finishedAt?: string;
    expiresAt: string;
    cancelRequested: boolean;
    createdAt: string;
    updatedAt: string;
  };
  attempts: Array<{
    attemptId: string;
    attemptNo: number;
    state: string;
    errorCode?: string;
    durationMs?: number;
    startedAt?: string;
    finishedAt?: string;
  }>;
  estimator: (Omit<RinRendererWaitEstimate, 'estimatedStartRange'> & {
    earliestStartAt: string;
    latestStartAt: string;
    actualStartedAt?: string;
    centralErrorMs?: number;
    intervalCovered?: boolean;
  }) | null;
  artifacts: Array<{
    artifactId: string;
    kind: 'source' | 'result' | 'debug' | RinLatexPDFPreviewArtifactKind;
    visibility: 'private';
    sha256: string;
    byteSize: number;
    mediaType: string;
    schemaVersion?: string;
    expiresAt?: string;
    createdAt: string;
  }>;
  scope: 'instance' | 'cluster';
  inspectedAt: string;
}

export interface RinRendererDiagramRequest {
  type?: string;
  options?: string;
  body?: string;
  source?: string;
}

export interface RinRendererDiagramResponse {
  requestId: string;
  id: string;
  type: string;
  svgHash: string;
  svgBytes?: number;
  objectId: string;
  cloudbaseUrl: string;
  url: string;
  uploaded: boolean;
  engine: string;
  versions: RinRendererVersions;
  diagnostics: RinRendererDiagnostic[];
  cached: boolean;
  renderUrl?: string;
}

export interface RinDiagramBatchV1 {
  contractVersion: typeof RIN_DIAGRAM_BATCH_CONTRACT_VERSION;
  items: RinDiagramBatchItem[];
  outputStrategy: 'svg';
}

export interface RinDiagramBatchItem {
  unit: RinDiagramUnit;
  sourceMode: 'complete' | 'body';
  body?: string;
  wrapperId?: string;
}

export interface RinDiagramSourceMetadata {
  diagramType: string;
  source: string;
  options?: string;
  layout?: RinDiagramLayout;
  sourceMode: 'complete' | 'body';
  sourceLocation?: RinSourceLocation;
}

export interface RinDiagramCacheResult {
  status: 'hit' | 'miss' | 'bypass';
  keyVersion?: string;
  digest?: string;
}

export interface RinResolvedDiagramUnit {
  id: string;
  state: 'succeeded' | 'failed';
  diagramId?: string;
  engine: string;
  engineVersion: string;
  url?: string;
  svgHash?: string;
  objectId?: string;
  svgBytes?: number;
  uploaded?: boolean;
  engineCached?: boolean;
  renderUrl?: string;
  artifact?: RinArtifactReference;
  source: RinDiagramSourceMetadata;
  cache: RinDiagramCacheResult;
  diagnostics: RinDiagnostic[];
}

export interface RinDiagramBatchResultV1 {
  contractVersion: typeof RIN_DIAGRAM_RESULT_CONTRACT_VERSION;
  units: RinResolvedDiagramUnit[];
  versions: Record<string, string>;
}

export interface RinRendererProjectFile {
  path: string;
  kind: 'tex' | 'bib' | 'style' | 'asset' | 'text' | string;
  encoding?: 'base64' | string;
  mime?: string;
  body?: string;
  bytes: number;
}

export interface RinRendererProjectManifest {
  version: string;
  title: string;
  mainFile: string;
  files: RinRendererProjectFile[];
  fileCount: number;
  source?: string;
  analysisSource?: string;
  resolvedSource?: string;
  includes?: RinRendererProjectReference[];
  diagramPlaceholders: RinRendererDiagramPlaceholder[];
  assetInventory: RinRendererAssetInventory;
  bibliography: RinRendererBibliographyInventory;
  generatedLists: RinRendererGeneratedListInventory;
  diagnostics?: RinRendererDiagnostic[];
}

export interface RinRendererDiagramPlaceholder {
  id: string;
  type: string;
  options?: string;
  body: string;
  source: string;
  sourceFile: string;
  sourceLine: number;
  sourceColumn: number;
  placeholder: string;
  layout: {
    alignment?: string;
  };
}

export interface RinRendererProjectReference {
  kind: string;
  command: string;
  rawRef: string;
  path?: string;
  sourceFile: string;
  line: number;
  resolved: boolean;
}

export interface RinRendererAsset {
  path: string;
  mime?: string;
  encoding?: string;
  referenced: boolean;
  references?: RinRendererProjectReference[];
}

export interface RinRendererAssetInventory {
  references: RinRendererProjectReference[];
  assets: RinRendererAsset[];
  missing: RinRendererProjectReference[];
  unused: RinRendererAsset[];
  graphicsPaths: string[];
  graphicsExtensions: string[];
}

export interface RinRendererAssetManifest extends RinRendererAssetInventory {
  version: string;
  files: RinRendererAssetFile[];
}

export interface RinRendererBibliographyInventory {
  references: RinRendererProjectReference[];
  missing: RinRendererProjectReference[];
  bibPaths: string[];
  styles: RinRendererProjectReference[];
  missingStyles: RinRendererProjectReference[];
}

export interface RinRendererGeneratedListInventory {
  references: RinRendererGeneratedListReference[];
  missing: RinRendererGeneratedListReference[];
}

export interface RinRendererGeneratedListReference {
  kind: string;
  listType: string;
  command: string;
  label: string;
  path?: string;
  resolved: boolean;
  expectedExtensions: string[];
  fallbackExtensions?: string[];
  sourceFile: string;
  line: number;
}

export interface RinRendererProjectPayload {
  title: string;
  status: 'draft' | 'published-preview' | string;
  mode: 'article' | 'book' | string;
  mainFile: string;
  activePath: string;
  files: RinRendererProjectFile[];
  manifest: RinRendererProjectManifest;
}

export interface RinRendererAssetFile {
  path: string;
  filename?: string;
  mime?: string;
  encoding?: string;
  body?: string;
  bytes?: number;
  referenced?: boolean;
}

export interface RinRendererMathSummary {
  count: number;
  mathJaxCount: number;
  katexCount: number;
  svgFallbackCount: number;
  failedCount: number;
}

export interface RinRendererProjectResponse {
  requestId: string;
  title: string;
  html: string;
  engine: 'auto' | 'latexml' | string;
  fallback: boolean;
  primaryEngine?: 'latexml' | string;
  fallbackEngine?: string;
  mainFile: string;
  diagrams: Array<{
    type: string;
    objectId: string;
    cloudbaseUrl: string;
  }>;
  generatedArtifacts?: RinArtifactReference[];
  assets: Array<Record<string, unknown>>;
  assetFiles?: RinRendererAssetFile[];
  assetManifest?: RinRendererAssetManifest;
  reader: Record<string, unknown>;
  math: RinRendererMathSummary;
  versions: RinRendererVersions;
  diagnostics: RinRendererDiagnostic[];
  texSource?: string;
  source?: string;
  analysisSource?: string;
  resolvedSource?: string;
  assetInventory?: RinRendererAssetInventory;
  project?: RinRendererProjectPayload | Record<string, unknown>;
}
