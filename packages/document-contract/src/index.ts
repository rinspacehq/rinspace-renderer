export const RIN_PROJECT_GRAPH_SCHEMA_VERSION = 'rin-project-graph/v1' as const;
export const RIN_DOCUMENT_BUNDLE_SCHEMA_VERSION_V1 = 'rin-document-bundle/v1' as const;
export const RIN_DOCUMENT_BUNDLE_SCHEMA_VERSION_V2 = 'rin-document-bundle/v2' as const;
// Producers remain on v1 until both Markdown and LaTeX block emitters are ready.
export const RIN_DOCUMENT_BUNDLE_SCHEMA_VERSION = RIN_DOCUMENT_BUNDLE_SCHEMA_VERSION_V1;

export type RinContentKind = 'latex' | 'markdown' | 'typst';
export type RinProjectContentKind = RinContentKind | 'pdf';
export type RinProjectType = 'article' | 'book' | 'tag-wiki' | 'pdf';
export type RinProjectEntrypointRole = 'document' | 'book-page';
export type RinProjectFileRole = 'source' | 'asset' | 'bibliography' | 'generated';

export interface RinProjectGraphV1 {
  schemaVersion: typeof RIN_PROJECT_GRAPH_SCHEMA_VERSION;
  projectHash: string;
  contentKind: RinProjectContentKind;
  entrypoints: RinProjectEntrypoint[];
  files: RinProjectGraphFile[];
  references: RinProjectReference[];
  options: Record<string, unknown>;
}

export interface RinProjectEntrypoint {
  path: string;
  role: RinProjectEntrypointRole;
}

export interface RinProjectGraphFile {
  path: string;
  sha256: string;
  bytes: number;
  mediaType?: string;
  role: RinProjectFileRole;
}

export interface RinProjectReference {
  from: string;
  to: string;
  kind: string;
  resolved: boolean;
}

export type RinDiagnosticSeverity = 'info' | 'warning' | 'error';
export type RinWorkUnit = RinMathUnit | RinDiagramUnit | RinCodeUnit;

export interface RinDocumentBundleV1 {
  schemaVersion: typeof RIN_DOCUMENT_BUNDLE_SCHEMA_VERSION_V1;
  projectHash: string;
  bundleHash?: string;
  state: 'draft' | 'final';
  contentKind: RinContentKind;
  documentEngine: string;
  title: string;
  pages: RinPage[];
  workUnits: RinWorkUnit[];
  assets: RinAssetReference[];
  diagnostics: RinDiagnostic[];
  provenance: RinProvenance;
}

export interface RinDocumentBundleV2 {
  schemaVersion: typeof RIN_DOCUMENT_BUNDLE_SCHEMA_VERSION_V2;
  projectHash: string;
  bundleHash?: string;
  state: 'draft' | 'final';
  contentKind: RinContentKind;
  documentEngine: string;
  title: string;
  pages: RinPageV2[];
  workUnits: RinWorkUnit[];
  assets: RinAssetReference[];
  diagnostics: RinDiagnostic[];
  provenance: RinProvenance;
}

export type RinDocumentBundle = RinDocumentBundleV1 | RinDocumentBundleV2;

export interface RinPage {
  id: string;
  sourcePath: string;
  title?: string;
  fragment: string;
  fragmentFormat: 'html-with-rin-placeholders' | 'html';
  toc: RinTocEntry[];
  dependencyHashes: string[];
}

export interface RinPageV2 extends RinPage {
  blocks: RinDocumentBlock[];
}

export type RinDocumentBlockKind =
  | 'heading'
  | 'paragraph'
  | 'list-item'
  | 'theorem'
  | 'math'
  | 'code'
  | 'figure'
  | 'table'
  | 'quote';

export interface RinDocumentBlock {
  id: string;
  kind: RinDocumentBlockKind;
  text: string;
  textHash: string;
  headingPath: string[];
  sourceLocation?: RinSourceLocation;
}

export interface RinTocEntry {
  id: string;
  depth: number;
  text: string;
}

export interface RinMathUnit {
  kind: 'math';
  id: string;
  source: string;
  display: boolean;
  macroContextHash: string;
  accessibilityContext?: RinMathAccessibilityContext;
  sourceLocation?: RinSourceLocation;
}

export interface RinMathAccessibilityContext {
  language?: string;
  speechStyle?: string;
  label?: string;
}

export interface RinDiagramUnit {
  kind: 'diagram';
  id: string;
  diagramType: string;
  source: string;
  options?: string;
  layout?: RinDiagramLayout;
  sourceLocation?: RinSourceLocation;
}

export interface RinDiagramLayout {
  alignment?: 'center' | 'flushleft' | 'flushright';
}

export interface RinCodeUnit {
  kind: 'code';
  id: string;
  source: string;
  language?: string;
  meta?: string;
  sourceLocation?: RinSourceLocation;
}

export interface RinSourceLocation {
  path: string;
  start: RinSourcePosition;
  end?: RinSourcePosition;
}

export interface RinSourcePosition {
  line: number;
  column: number;
}

export interface RinAssetReference {
  id: string;
  kind: 'project-file' | 'generated';
  sha256: string;
  bytes: number;
  mediaType: string;
  projectPath?: string;
  artifactKey?: string;
}

export interface RinDiagnostic {
  code: string;
  severity: RinDiagnosticSeverity;
  message: string;
  stage: string;
  sourceLocation?: RinSourceLocation;
}

export interface RinProvenance {
  adapter: string;
  adapterVersion: string;
  engineVersion: string;
  projectGraphSchemaVersion: typeof RIN_PROJECT_GRAPH_SCHEMA_VERSION;
}
