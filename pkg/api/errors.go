package api

import (
	"errors"
	"fmt"
	"net/http"
)

// Status is the error body of a failed request, in the Kubernetes shape: a
// machine-readable Reason and Code beside a message written for a person.
//
// Clients branch on Reason. It is a closed set precisely so that a caller can
// tell "your object is wrong, fixing it is up to you" from "someone else wrote
// first, re-read and retry" without pattern-matching on prose.
type Status struct {
	TypeMeta `json:",inline"`
	Status   string `json:"status"`
	Message  string `json:"message"`
	Reason   Reason `json:"reason"`
	Code     int    `json:"code"`
}

// Reason is the machine-readable cause of a failure.
type Reason string

// Failure reasons.
const (
	// ReasonBadRequest: the request itself is malformed.
	ReasonBadRequest Reason = "BadRequest"
	// ReasonNotFound: the named object does not exist.
	ReasonNotFound Reason = "NotFound"
	// ReasonAlreadyExists: an object with that name already exists.
	ReasonAlreadyExists Reason = "AlreadyExists"
	// ReasonConflict: the object changed since the version the client sent.
	ReasonConflict Reason = "Conflict"
	// ReasonInvalid: the object is well-formed but violates a rule.
	ReasonInvalid Reason = "Invalid"
	// ReasonNotSupported: the backend cannot do what was asked.
	ReasonNotSupported Reason = "NotSupported"
	// ReasonBackendError: the transport failed; retrying may help.
	ReasonBackendError Reason = "BackendError"
	// ReasonInternalError: a bug on this side.
	ReasonInternalError Reason = "InternalError"
	// ReasonTimeout: the operation did not complete in the time allowed.
	ReasonTimeout Reason = "Timeout"
)

// StatusError is a Status carried as a Go error, so the same failure travels
// unchanged from the store, through the service, to either the HTTP layer or an
// MCP tool result without either of them re-deriving what went wrong.
type StatusError struct {
	Status Status
}

func (e *StatusError) Error() string { return e.Status.Message }

// Reason returns the failure's machine-readable cause, or ReasonInternalError
// for an error that is not a StatusError — an unclassified failure is a bug on
// this side by definition.
func ReasonOf(err error) Reason {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status.Reason
	}
	return ReasonInternalError
}

// CodeOf returns the HTTP status code an error should be served as.
func CodeOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status.Code
	}
	return http.StatusInternalServerError
}

// StatusOf renders any error as a Status object, so every failure leaves the
// daemon in the same shape.
func StatusOf(err error) Status {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return newStatus(ReasonInternalError, http.StatusInternalServerError, err.Error())
}

func newStatus(reason Reason, code int, msg string) Status {
	return Status{
		TypeMeta: TypeMeta{APIVersion: Version, Kind: "Status"},
		Status:   "Failure",
		Message:  msg,
		Reason:   reason,
		Code:     code,
	}
}

func newErr(reason Reason, code int, format string, args ...any) *StatusError {
	return &StatusError{Status: newStatus(reason, code, fmt.Sprintf(format, args...))}
}

// NewBadRequest reports a malformed request.
func NewBadRequest(format string, args ...any) error {
	return newErr(ReasonBadRequest, http.StatusBadRequest, format, args...)
}

// NewNotFound reports that a named object does not exist.
func NewNotFound(kind, name string) error {
	return newErr(ReasonNotFound, http.StatusNotFound, "%s %q not found", kind, name)
}

// NewAlreadyExists reports a name collision.
func NewAlreadyExists(kind, name string) error {
	return newErr(ReasonAlreadyExists, http.StatusConflict, "%s %q already exists", kind, name)
}

// NewConflict reports that the object changed under the client. The message
// names both versions, because the fix is always the same — re-read and retry —
// and the numbers are what make it obvious that is what happened.
func NewConflict(kind, name string, have, want int64) error {
	return newErr(ReasonConflict, http.StatusConflict,
		"%s %q has been modified: resourceVersion is %d, you sent %d — re-read the object and retry",
		kind, name, have, want)
}

// NewInvalid reports a rule violation in a well-formed object.
func NewInvalid(format string, args ...any) error {
	return newErr(ReasonInvalid, http.StatusUnprocessableEntity, format, args...)
}

// NewNotSupported reports that the backend cannot do what was asked.
func NewNotSupported(format string, args ...any) error {
	return newErr(ReasonNotSupported, http.StatusNotImplemented, format, args...)
}

// NewBackendError reports a transport failure. It is 502, not 500: the daemon
// is fine, something it depends on is not.
func NewBackendError(format string, args ...any) error {
	return newErr(ReasonBackendError, http.StatusBadGateway, format, args...)
}

// NewTimeout reports that an operation ran out of time.
func NewTimeout(format string, args ...any) error {
	return newErr(ReasonTimeout, http.StatusGatewayTimeout, format, args...)
}

// NewInternalError reports a bug on this side.
func NewInternalError(format string, args ...any) error {
	return newErr(ReasonInternalError, http.StatusInternalServerError, format, args...)
}

// IsNotFound reports whether err is a NotFound failure.
func IsNotFound(err error) bool { return ReasonOf(err) == ReasonNotFound }

// IsConflict reports whether err is a Conflict failure.
func IsConflict(err error) bool { return ReasonOf(err) == ReasonConflict }

// IsAlreadyExists reports whether err is an AlreadyExists failure.
func IsAlreadyExists(err error) bool { return ReasonOf(err) == ReasonAlreadyExists }
