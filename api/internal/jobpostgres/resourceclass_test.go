package jobpostgres

import (
	"strings"
	"testing"
)

// sqlStringList parses the literal class lists that are embedded in SQL so the
// Go helpers and the SQL predicates cannot drift apart.
func sqlStringList(t *testing.T, literal string) []string {
	t.Helper()
	trimmed := strings.TrimSpace(literal)
	if !strings.HasPrefix(trimmed, "(") || !strings.HasSuffix(trimmed, ")") {
		t.Fatalf("sql class list %q is not a parenthesised list", literal)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(trimmed, "("), ")")
	if strings.TrimSpace(inner) == "" {
		return nil
	}
	items := make([]string, 0, 8)
	for _, part := range strings.Split(inner, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "'") || !strings.HasSuffix(part, "'") {
			t.Fatalf("sql class list item %q is not a quoted literal", part)
		}
		items = append(items, strings.Trim(part, "'"))
	}
	return items
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestResourceClassHelpersMatchSQLLiterals(t *testing.T) {
	heavy := sqlStringList(t, heavyResourceClassListSQL)
	if !equalStringSlices(heavy, HeavyResourceClasses()) {
		t.Fatalf("heavy resource class SQL = %v, Go helper = %v", heavy, HeavyResourceClasses())
	}
	for _, resourceClass := range ResourceClasses() {
		expected := false
		for _, candidate := range heavy {
			if candidate == resourceClass {
				expected = true
			}
		}
		if IsHeavyResourceClass(resourceClass) != expected {
			t.Fatalf("IsHeavyResourceClass(%q) = %v, want %v", resourceClass, !expected, expected)
		}
	}

	preview := sqlStringList(t, previewResourceClassListSQL)
	for _, resourceClass := range ResourceClasses() {
		inSQL := false
		for _, candidate := range preview {
			if candidate == resourceClass {
				inSQL = true
			}
		}
		if IsPreviewResourceClass(resourceClass) != inSQL {
			t.Fatalf("IsPreviewResourceClass(%q) = %v, want %v", resourceClass, !inSQL, inSQL)
		}
	}
}

// The general document worker holds leadership without an explicit class
// filter. It must never claim a Typst class, otherwise an older worker would run
// a Typst job through the Markdown/LaTeX executor.
func TestDocumentWorkerClaimListExcludesTypstClasses(t *testing.T) {
	claimable := sqlStringList(t, documentWorkerResourceClassListSQL)
	allowed := map[string]bool{
		"document-light": true, "document-latexml": true, "math-node": true,
		"texsvg": true, "batch-migration": true,
	}
	for _, resourceClass := range claimable {
		if !allowed[resourceClass] {
			t.Fatalf("document worker claim list includes %q", resourceClass)
		}
		if IsPreviewResourceClass(resourceClass) || strings.Contains(resourceClass, "typst") {
			t.Fatalf("document worker claim list includes unowned class %q", resourceClass)
		}
	}
	for _, resourceClass := range []string{"document-light", "document-latexml", "math-node", "texsvg", "batch-migration"} {
		found := false
		for _, candidate := range claimable {
			if candidate == resourceClass {
				found = true
			}
		}
		if !found {
			t.Fatalf("document worker claim list is missing %q", resourceClass)
		}
	}
	if !equalStringSlices(claimable, sqlStringList(t, documentWorkerResourceClassListSQL)) {
		t.Fatal("document worker claim list is not deterministic")
	}
}

// Typst HTML rendering has its own executor and its own pinned compiler, so it
// must be claimed by a dedicated worker pool and never by the ordinary document
// worker. Preview PDF classes are dedicated for the same reason per language.
func TestDedicatedWorkerResourceClasses(t *testing.T) {
	if TypstHTMLResourceClass != "document-typst" {
		t.Fatalf("TypstHTMLResourceClass = %q, want document-typst", TypstHTMLResourceClass)
	}
	for _, resourceClass := range []string{"document-typst", "latex-pdf", "typst-pdf"} {
		if !IsDedicatedWorkerResourceClass(resourceClass) {
			t.Fatalf("IsDedicatedWorkerResourceClass(%q) = false, want true", resourceClass)
		}
	}
	for _, resourceClass := range []string{"", "document-light", "document-latexml", "math-node", "texsvg", "batch-migration"} {
		if IsDedicatedWorkerResourceClass(resourceClass) {
			t.Fatalf("IsDedicatedWorkerResourceClass(%q) = true, want false", resourceClass)
		}
	}
	for _, candidate := range sqlStringList(t, documentWorkerResourceClassListSQL) {
		if candidate == TypstHTMLResourceClass {
			t.Fatalf("ordinary document worker claim list must not include %q", TypstHTMLResourceClass)
		}
	}
}

func TestEveryResourceClassHasCapacityAndHeavyAccounting(t *testing.T) {
	policy := SchedulingPolicy{Resources: ResourceCapacities{
		DocumentLight: 1, DocumentLaTeXML: 1, MathNode: 1, TeXSVG: 1,
		BatchMigration: 1, LatexPDF: 1, DocumentTypst: 1, TypstPDF: 1, Heavy: 1,
	}, Weights: PriorityWeights{Publish: 8, Preview: 5, Rebuild: 3, Migration: 1},
		AgingInterval: 1, MaxAgingSteps: 1, PrincipalRunning: 1}
	if err := policy.Validate(); err != nil {
		t.Fatalf("full policy Validate() error = %v", err)
	}
	for _, resourceClass := range ResourceClasses() {
		if capacity := resourceCapacity(policy.Resources, resourceClass); capacity != 1 {
			t.Fatalf("resourceCapacity(%q) = %d, want 1", resourceClass, capacity)
		}
	}
	if capacity := resourceCapacity(policy.Resources, "unknown-class"); capacity != 0 {
		t.Fatalf("resourceCapacity(unknown) = %d, want 0", capacity)
	}
	if err := (SchedulingPolicy{Resources: ResourceCapacities{
		DocumentLight: 1, DocumentLaTeXML: 1, MathNode: 1, TeXSVG: 1,
		BatchMigration: 1, LatexPDF: 1, DocumentTypst: 1, TypstPDF: 0, Heavy: 1,
	}, Weights: PriorityWeights{Publish: 8, Preview: 5, Rebuild: 3, Migration: 1},
		AgingInterval: 1, MaxAgingSteps: 1, PrincipalRunning: 1}).Validate(); err == nil {
		t.Fatal("policy without Typst PDF capacity was accepted")
	}
}
