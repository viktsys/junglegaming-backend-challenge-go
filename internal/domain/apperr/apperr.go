// Package apperr defines the domain error taxonomy shared by all layers.
// Errors carry a machine-readable code and a kind that maps to transport
// concerns (HTTP status, SQS retry classification) without leaking transport
// types into the domain.
package apperr

import (
	"errors"
	"fmt"
)

// Kind classifies an error for transport mapping and retry decisions.
type Kind string

const (
	KindInvalid      Kind = "invalid"
	KindNotFound     Kind = "not_found"
	KindConflict     Kind = "conflict"
	KindRejected     Kind = "rejected"
	KindUnauthorized Kind = "unauthorized"
	KindForbidden    Kind = "forbidden"
	KindUnavailable  Kind = "unavailable"
	KindInternal     Kind = "internal"
)

// Error is a classified domain error. It supports errors.Is through Code and
// errors.As through the concrete type.
type Error struct {
	Kind    Kind
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Is matches another *Error by code, which makes sentinel comparisons stable.
func (e *Error) Is(target error) bool {
	var other *Error
	if !errors.As(target, &other) {
		return false
	}
	return e.Code == other.Code
}

// New builds a classified error without an underlying cause.
func New(kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message}
}

// Wrap builds a classified error around an underlying cause.
func Wrap(kind Kind, code, message string, err error) *Error {
	return &Error{Kind: kind, Code: code, Message: message, Err: err}
}

// KindOf extracts the kind of a classified error, defaulting to internal.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// CodeOf extracts the stable code of a classified error, or "" when unclassified.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsKind reports whether err is classified with the given kind.
func IsKind(err error, kind Kind) bool {
	return KindOf(err) == kind
}
