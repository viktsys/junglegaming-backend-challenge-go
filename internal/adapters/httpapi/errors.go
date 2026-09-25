package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
)

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Default().Error("failed to encode response", slog.String("error", err.Error()))
	}
}

func writeError(w http.ResponseWriter, err error) {
	kind := apperr.KindOf(err)
	code := apperr.CodeOf(err)
	if code == "" {
		code = "INTERNAL_ERROR"
	}
	message := err.Error()
	if kind == apperr.KindInternal {
		message = "internal error"
	}
	writeJSON(w, statusForKind(kind), errorResponse{
		Code:    code,
		Message: message,
		Kind:    string(kind),
	})
}

// writeRejection renders a persisted business rejection with its audit identity.
func writeRejection(w http.ResponseWriter, transactionID, failureCode, message string, idempotentReplay bool) {
	writeJSON(w, http.StatusUnprocessableEntity, errorResponse{
		Code:             failureCode,
		Message:          message,
		Kind:             string(apperr.KindRejected),
		FailureCode:      failureCode,
		TransactionID:    transactionID,
		IdempotentReplay: idempotentReplay,
	})
}

func statusForKind(kind apperr.Kind) int {
	switch kind {
	case apperr.KindInvalid:
		return http.StatusBadRequest
	case apperr.KindUnauthorized:
		return http.StatusUnauthorized
	case apperr.KindForbidden:
		return http.StatusForbidden
	case apperr.KindNotFound:
		return http.StatusNotFound
	case apperr.KindConflict:
		return http.StatusConflict
	case apperr.KindRejected:
		return http.StatusUnprocessableEntity
	case apperr.KindUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return apperr.Wrap(apperr.KindInvalid, "INVALID_JSON", "request body is not valid JSON", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return apperr.New(apperr.KindInvalid, "INVALID_JSON", "request body must contain a single JSON object")
	}
	return nil
}
