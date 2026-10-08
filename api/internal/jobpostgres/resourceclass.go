package jobpostgres

// IsPreviewResourceClass reports whether a resource class runs as a
// context-isolated PDF build pool. Each such class has its own worker pool,
// capacity, one-running-per-context invariant, preview/export supersede
// semantics and priority defaults. LaTeX and Typst previews never share a pool
// or supersede each other.
func IsPreviewResourceClass(resourceClass string) bool {
	return previewResourceClass(resourceClass)
}

func previewResourceClass(resourceClass string) bool {
	return resourceClass == "latex-pdf" || resourceClass == "typst-pdf"
}

// TypstHTMLResourceClass is the single class that renders Typst sources into
// the semantic HTML reader bundle. It runs outside the ordinary document pool
// because its executor is unrelated to the Markdown/LaTeX document executor.
const TypstHTMLResourceClass = "document-typst"

// IsDedicatedWorkerResourceClass reports whether a class must be claimed by a
// worker pool dedicated to that exact class instead of the ordinary document
// worker. Preview PDF classes are dedicated per language because they supersede
// each other inside one author context, and the Typst HTML class is dedicated
// so the ordinary worker can never run a Typst job through the Markdown/LaTeX
// executor.
func IsDedicatedWorkerResourceClass(resourceClass string) bool {
	return resourceClass == TypstHTMLResourceClass || previewResourceClass(resourceClass)
}

// previewResourceClassListSQL is the literal list of dedicated preview classes.
const previewResourceClassListSQL = "('latex-pdf', 'typst-pdf')"

// ResourceClasses lists every schedulable resource class in a stable order.
// Callers that publish per-class metrics or iterate capacity pools use this
// instead of repeating the class names, so adding a class cannot silently skip
// a metric.
func ResourceClasses() []string {
	return []string{
		"document-light", "document-latexml", "math-node", "texsvg",
		"batch-migration", "latex-pdf", "document-typst", "typst-pdf",
	}
}

// HeavyResourceClasses lists the classes that share the global heavy workload
// quota. The returned slice is the Go-side mirror of heavyResourceClassListSQL
// and both are asserted equal by resourceclass_test.go.
func HeavyResourceClasses() []string {
	return []string{
		"document-latexml", "texsvg", "batch-migration", "latex-pdf",
		"document-typst", "typst-pdf",
	}
}

// IsHeavyResourceClass reports whether a class shares the global heavy
// workload quota. It mirrors heavyResourceClassListSQL.
func IsHeavyResourceClass(resourceClass string) bool {
	for _, candidate := range HeavyResourceClasses() {
		if candidate == resourceClass {
			return true
		}
	}
	return false
}

// documentWorkerResourceClassListSQL is the set of classes the general
// document worker may claim when it holds leadership without an explicit class
// filter. Typst classes are deliberately excluded: a Typst job must only ever be
// claimed by the worker pool that carries the pinned Typst compiler, so neither
// an older document worker nor a mixed-version rollout can run a Typst job
// through the Markdown/LaTeX executor. The Typst HTML channel claims
// document-typst through IsDedicatedWorkerResourceClass instead.
const documentWorkerResourceClassListSQL = "('document-light', 'document-latexml', 'math-node', 'texsvg', 'batch-migration')"

// heavyResourceClassListSQL is the literal list of classes that share the
// global heavy workload quota. Typst is not exempt merely because it is often
// faster; both Typst publishing and Typst PDF work count against it.
const heavyResourceClassListSQL = "('document-latexml', 'texsvg', 'batch-migration', 'latex-pdf', 'document-typst', 'typst-pdf')"
