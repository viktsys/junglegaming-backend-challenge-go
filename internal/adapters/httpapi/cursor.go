package httpapi

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/ports"
)

// encodeCursor builds the opaque keyset cursor (createdAt|id) in base64url.
func encodeCursor(createdAt time.Time, id uuid.UUID) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(raw string) (*ports.LedgerCursor, error) {
	if raw == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, apperr.Wrap(apperr.KindInvalid, "INVALID_CURSOR", "cursor is not valid base64", err)
	}
	parts := strings.SplitN(string(decoded), "|", 2)
	if len(parts) != 2 {
		return nil, apperr.New(apperr.KindInvalid, "INVALID_CURSOR", "cursor is malformed")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, apperr.Wrap(apperr.KindInvalid, "INVALID_CURSOR", "cursor timestamp is malformed", err)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return nil, apperr.Wrap(apperr.KindInvalid, "INVALID_CURSOR", "cursor id is malformed", err)
	}
	return &ports.LedgerCursor{CreatedAt: createdAt, ID: id}, nil
}

func parseLimit(raw string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	var limit int
	if _, err := fmt.Sscanf(raw, "%d", &limit); err != nil {
		return 0, apperr.Wrap(apperr.KindInvalid, "INVALID_LIMIT", "limit must be an integer", err)
	}
	if limit < 1 || limit > 200 {
		return 0, apperr.New(apperr.KindInvalid, "INVALID_LIMIT", "limit must be between 1 and 200")
	}
	return limit, nil
}
