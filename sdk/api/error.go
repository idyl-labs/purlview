// Copyright 2026 Idyl Labs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"errors"
	"net/http"
)

// Code is a stable failure code. Unknown remote codes remain errors.
type Code string

// Failure codes shared by clients and handlers.
const (
	NotImplemented       Code = "not_implemented"
	InvalidRequest       Code = "invalid_request"
	Unauthorised         Code = "unauthorised"
	Denied               Code = "denied"
	Expired              Code = "expired"
	NotFound             Code = "not_found"
	Conflict             Code = "conflict"
	Unavailable          Code = "unavailable"
	ProtocolError        Code = "protocol_error"
	InternalError        Code = "internal_error"
	UnsupportedVersion   Code = "unsupported_version"
	MethodNotAllowed     Code = "method_not_allowed"
	Misdirected          Code = "misdirected_request"
	RequestTooLarge      Code = "request_too_large"
	UnsupportedMediaType Code = "unsupported_media_type"
	// UpdateRequired: the server no longer serves this client version; the
	// user must update Purlview. Clients released before the code show a
	// generic failure.
	UpdateRequired Code = "update_required"
)

// Outcome describes whether an unsuccessful write could have taken effect.
type Outcome string

// Failure outcomes never imply confirmed success.
const (
	NotApplied Outcome = "not_applied"
	Unknown    Outcome = "unknown"
)

// Error is the canonical wire error. Message is diagnostic text, never trusted
// for display: Error() uses a fixed local vocabulary and omits all remote text.
//
// AttemptsLeft accompanies denied from login verify: how many more codes the
// challenge accepts, LoginAttempts-1 down to 1. Zero means not reported; the
// last miss and a lapsed challenge are expired instead. Clients read it only
// from denied and refuse any other value.
//
// Limit accompanies denied from share create when an account limit refused
// it. Clients read it only from denied.
type Error struct {
	Code         Code    `json:"error"`
	Message      string  `json:"message"`
	RequestID    string  `json:"request_id,omitempty"`
	Outcome      Outcome `json:"outcome"`
	Retryable    bool    `json:"retryable"`
	AttemptsLeft int     `json:"attempts_left,omitempty"`
	Limit        Limit   `json:"limit,omitempty"`
	Status       int     `json:"-"`
	Cause        error   `json:"-"`
}

// Limit names the account limit that refused a request. It never carries a
// number: limits differ by deployment and can be raised for one account. A
// client that does not know a value treats it as an unnamed limit.
type Limit string

// Account limits a share create can reach.
const (
	// LimitRunningShares: the account already runs as many shares as it may.
	LimitRunningShares Limit = "running_shares"
	// LimitSharesPerHour: the account has started as many shares as it may in
	// the last hour.
	LimitSharesPerHour Limit = "shares_per_hour"
)

func (e *Error) Error() string { return Message(e.Code) }

// Unwrap retains cancellation/deadline identity without reflecting transport text.
func (e *Error) Unwrap() error { return e.Cause }

// Message returns trusted diagnostic text for a code (including unknown codes).
func Message(c Code) string {
	switch c {
	case NotImplemented:
		return "not implemented in this scaffold build"
	case InvalidRequest:
		return "the platform request is invalid"
	case Unauthorised:
		return "the installation credential is not authorised"
	case Denied:
		return "sign-in was denied"
	case Expired:
		return "the authorisation expired"
	case NotFound:
		return "the requested resource was not found"
	case Conflict:
		return "the attempt key was already used for different inputs"
	case Unavailable:
		return "Purlview is unavailable"
	case ProtocolError:
		return "Purlview returned an invalid protocol response"
	case UnsupportedVersion:
		return "the product API version is not supported"
	case MethodNotAllowed:
		return "the HTTP method is not supported"
	case Misdirected:
		return "this host does not serve the account API"
	case RequestTooLarge:
		return "the request is too large"
	case UnsupportedMediaType:
		return "the request must use application/json"
	case UpdateRequired:
		return "this version of Purlview is no longer supported"
	default:
		return "Purlview rejected the request"
	}
}

// KnownCode reports whether this SDK version defines the failure code.
func KnownCode(c Code) bool { _, ok := codeStatus[c]; return ok }

var codeStatus = map[Code]int{
	NotImplemented:       http.StatusNotImplemented,
	InvalidRequest:       http.StatusBadRequest,
	Unauthorised:         http.StatusUnauthorized,
	Denied:               http.StatusForbidden,
	Expired:              http.StatusGone,
	NotFound:             http.StatusNotFound,
	Conflict:             http.StatusConflict,
	Unavailable:          http.StatusServiceUnavailable,
	UnsupportedVersion:   http.StatusBadRequest,
	MethodNotAllowed:     http.StatusMethodNotAllowed,
	Misdirected:          http.StatusMisdirectedRequest,
	RequestTooLarge:      http.StatusRequestEntityTooLarge,
	UnsupportedMediaType: http.StatusUnsupportedMediaType,
	UpdateRequired:       http.StatusUpgradeRequired,
	InternalError:        http.StatusInternalServerError,
	ProtocolError:        http.StatusInternalServerError,
}

// HTTPStatus maps known API failures to HTTP; unknown service codes become 500.
func HTTPStatus(c Code) int {
	if status, ok := codeStatus[c]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// ErrNotImplemented is the shared sentinel for an operation that this build
// or service does not implement.
var ErrNotImplemented = errors.New("not implemented in this scaffold build")

// Is supports consumer compatibility without losing the full typed API error.
func (e *Error) Is(target error) bool {
	return target == ErrNotImplemented && e.Code == NotImplemented && e.Outcome == NotApplied
}
