package typstadapter

import "strings"

// UnsupportedHTML recognizes the fixed compiler's capability diagnostics,
// including successful exports that explicitly discarded semantic content.
// Generic compile errors, missing packages, timeouts and sanitizer failures
// are not HTML capability errors and must remain failures.
func UnsupportedHTML(log string) string {
	reason := ""
	for _, line := range strings.Split(log, "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "warning:") && !strings.Contains(lower, "error:") {
			continue
		}
		capability := strings.Contains(lower, "was ignored during html export") || strings.Contains(lower, "was ignored during mathml export") ||
			strings.Contains(lower, "blocks with layout routines should not occur in html export") ||
			((strings.Contains(lower, "html export") || strings.Contains(lower, "mathml export")) &&
				(strings.Contains(lower, "not supported") || strings.Contains(lower, "not yet supported") || strings.Contains(lower, "unsupported")))
		if capability && reason == "" {
			reason = strings.TrimSpace(line)
		} else if strings.Contains(lower, "error:") && !capability {
			return "" // a real source/dependency error must not be hidden by an earlier warning
		}
	}
	return reason
}
