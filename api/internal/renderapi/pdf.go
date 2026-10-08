package renderapi

import (
	"errors"
	"io"
	"net/http"

	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfinspect"
)

const maxPDFInspectionBytes int64 = 80 << 20

func (s *Server) handlePDFInspection(response http.ResponseWriter, request *http.Request) {
	requestID := newRequestID()
	if !s.requireServiceToken(response, request, requestID) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxPDFInspectionBytes+1))
	if err != nil || int64(len(body)) > maxPDFInspectionBytes {
		writeError(response, http.StatusRequestEntityTooLarge, requestID, "PDF exceeds the inspection limit", nil)
		return
	}
	result, err := pdfinspect.Inspect(body)
	if errors.Is(err, pdfinspect.ErrQuarantined) {
		writeError(response, http.StatusUnprocessableEntity, requestID, "PDF was quarantined by the safety inspection", nil)
		return
	}
	if err != nil {
		writeError(response, http.StatusBadRequest, requestID, "PDF is invalid or damaged", nil)
		return
	}
	response.Header().Set("Cache-Control", "private, no-store")
	writeJSON(response, http.StatusOK, result)
}
