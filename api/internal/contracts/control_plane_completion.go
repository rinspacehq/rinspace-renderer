package contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	ControlPlaneEventSchemaV1       = "rin-control-event/v1"
	ControlPlaneCompletionSchemaV2  = "rin-renderer-completion/v2"
	ControlPlaneCompletionEventType = "renderer.job.completed"
)

var (
	completionRendererJobIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	completionControlJobIDPattern  = regexp.MustCompile(`^render-[0-9a-f]{32}$`)
	completionProjectIDPattern     = regexp.MustCompile(`^(article|book|tag-wiki|pdf):[1-9][0-9]*$`)
	completionCommitPattern        = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	completionFailureCodes         = map[string]bool{
		"document_invalid":             true,
		"document_render_failed":       true,
		"document_render_timeout":      true,
		"invalid_book_manifest":        true,
		"invalid_compatibility_result": true,
		"invalid_project_source":       true,
		"invalid_render_result":        true,
		"invalid_request_metadata":     true,
		"job_canceled":                 true,
		"result_storage_failed":        true,
		"source_artifact_unavailable":  true,
		"worker_lease_expired":         true,
		"worker_shutting_down":         true,
	}
)

type ControlPlaneCompletionEvent struct {
	Schema        string                        `json:"schema"`
	EventID       string                        `json:"event_id"`
	Type          string                        `json:"type"`
	OccurredAt    string                        `json:"occurred_at"`
	Producer      string                        `json:"producer"`
	CorrelationID string                        `json:"correlation_id"`
	Subject       ControlPlaneCompletionSubject `json:"subject"`
	Data          ControlPlaneCompletionData    `json:"data"`
}

type ControlPlaneCompletionSubject struct {
	RendererJobID string `json:"rendererJobId"`
}

type ControlPlaneCompletionData struct {
	SchemaVersion       string `json:"schemaVersion"`
	ProtocolVersion     string `json:"protocolVersion"`
	TerminalVersion     int    `json:"terminalVersion"`
	RendererJobID       string `json:"rendererJobId"`
	RequestID           string `json:"requestId"`
	ControlProjectID    string `json:"controlProjectId"`
	SourceCommit        string `json:"sourceCommit"`
	ControlProjectHash  string `json:"controlProjectHash"`
	RendererProjectHash string `json:"rendererProjectHash"`
	TerminalState       string `json:"terminalState"`
	ResultHash          string `json:"resultHash,omitempty"`
	FailureCode         string `json:"failureCode,omitempty"`
}

func ControlPlaneCompletionEventID(rendererJobID string, terminalVersion int) (string, error) {
	if !completionRendererJobIDPattern.MatchString(rendererJobID) || terminalVersion < 1 {
		return "", errors.New("Control Plane completion event identity is invalid")
	}
	return "renderer:" + rendererJobID + ":completed:" + strconv.Itoa(terminalVersion), nil
}

func ParseControlPlaneCompletionEvent(data []byte) (ControlPlaneCompletionEvent, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var value ControlPlaneCompletionEvent
	if err := decoder.Decode(&value); err != nil {
		return ControlPlaneCompletionEvent{}, fmt.Errorf("decode Control Plane completion event: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ControlPlaneCompletionEvent{}, errors.New("Control Plane completion event must contain exactly one JSON value")
	}
	if err := ValidateControlPlaneCompletionEvent(value); err != nil {
		return ControlPlaneCompletionEvent{}, err
	}
	return value, nil
}

func ValidateControlPlaneCompletionEvent(value ControlPlaneCompletionEvent) error {
	if value.Schema != ControlPlaneEventSchemaV1 || value.Type != ControlPlaneCompletionEventType || value.Producer != "rin-renderer" {
		return errors.New("Control Plane completion envelope identity is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, value.OccurredAt); err != nil {
		return errors.New("Control Plane completion occurred_at is invalid")
	}
	data := value.Data
	if data.SchemaVersion != ControlPlaneCompletionSchemaV2 || data.ProtocolVersion != "v2" || data.TerminalVersion < 1 {
		return errors.New("Control Plane completion protocol identity is invalid")
	}
	if !completionRendererJobIDPattern.MatchString(data.RendererJobID) || value.Subject.RendererJobID != data.RendererJobID ||
		!completionControlJobIDPattern.MatchString(data.RequestID) || value.CorrelationID != data.RequestID {
		return errors.New("Control Plane completion job identity is invalid")
	}
	if !completionProjectIDPattern.MatchString(data.ControlProjectID) || !completionCommitPattern.MatchString(data.SourceCommit) ||
		strings.Trim(data.SourceCommit, "0") == "" || !validControlPlaneCompletionHash(data.ControlProjectHash) || !validControlPlaneCompletionHash(data.RendererProjectHash) {
		return errors.New("Control Plane completion publication identity is invalid")
	}
	expectedID, err := ControlPlaneCompletionEventID(data.RendererJobID, data.TerminalVersion)
	if err != nil || value.EventID != expectedID {
		return errors.New("Control Plane completion event_id is invalid")
	}
	switch data.TerminalState {
	case "succeeded":
		if !validControlPlaneCompletionHash(data.ResultHash) || data.FailureCode != "" {
			return errors.New("succeeded Control Plane completion evidence is invalid")
		}
	case "failed", "canceled":
		if data.ResultHash != "" || !completionFailureCodes[data.FailureCode] {
			return errors.New("unsuccessful Control Plane completion evidence is invalid")
		}
	default:
		return fmt.Errorf("Control Plane completion terminal state %q is invalid", data.TerminalState)
	}
	return nil
}

func IsControlPlaneCompletionFailureCode(value string) bool {
	return completionFailureCodes[value]
}

func validControlPlaneCompletionHash(value string) bool {
	return isSHA256(value) && strings.Trim(value, "0") != ""
}
