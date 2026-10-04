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

// Package apiserver serves version 1 of the Purlview account API over HTTP.
//
// It is the server half of the contract the purlview package's Client speaks:
// routing, methods, authentication headers, request decoding and validation,
// response validation and the error envelope. Everything an account service
// decides lives behind Service: who a credential belongs to, what it may do,
// and what state changes. A handler from New never stores anything itself.
//
// New is enough to run a complete fake platform in tests. A production
// deployment wraps the handler with its own rate limits, client policy and
// routing; WriteError gives such wrappers the same error envelope.
package apiserver

import (
	"context"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/resource"
)

// Service is the account backend behind the HTTP binding.
//
// The authority string each method receives is the credential from the
// request's Authorization header, already checked for shape but not for
// validity: implementations must check its purpose and the account's and
// device's current authority before reading or changing state. Requests reach
// a Service only after they have been decoded and validated, and its results
// are validated again before they are encoded.
//
// Confirming a revocation means access is already denied and the affected
// streams are closed. Create keys are scoped to the installation; reusing a
// key with different inputs is a conflict and never extends the original
// expiry.
type Service interface {
	BeginAuthorization(context.Context, api.BeginAuthorizationRequest) (api.Authorization, error)
	ObserveAuthorization(context.Context, string, api.ObserveAuthorizationRequest) (api.AuthorizationStatus, error)
	Identity(context.Context, string) (resource.Identity, error)
	RevokeInstallation(context.Context, string) (api.Revocation, error)
	CreateShare(context.Context, string, api.CreateShareRequest) (api.ShareAccess, error)
	ListShares(context.Context, string) (api.ListSharesResult, error)
	RevokeShare(context.Context, string, api.RevokeShareRequest) (api.RevokeShareResult, error)
}

// InstallationService lists and revokes the caller's account's installations.
// A Service that also implements it serves the installation routes; otherwise
// they answer not_implemented.
//
// The list marks at most one entry current: the caller's own, when the caller
// is an installation. An id outside the caller's account must be not_found,
// like an unknown id. Revocation ends the installation's shares exactly as
// RevokeInstallation does.
type InstallationService interface {
	ListInstallations(context.Context, string) (api.ListInstallationsResult, error)
	RevokeInstallationByID(context.Context, string, api.RevokeInstallationRequest) (api.RevokeInstallationResult, error)
}

// EmailService runs sign-in by emailed code. A Service that also implements it
// serves the login routes; otherwise they answer not_implemented.
type EmailService interface {
	StartLogin(context.Context, api.LoginStartRequest) (api.LoginChallenge, error)
	VerifyLogin(context.Context, api.LoginVerifyRequest) (api.InstallationCredential, error)
	ResendLogin(context.Context, api.LoginResendRequest) (api.LoginChallenge, error)
}

// Unimplemented answers every operation with not_implemented: it issues no
// credentials, creates no state and confirms no revocation. New uses it when
// given no service. Embed it to implement only part of Service.
type Unimplemented struct{}

func absent() error { return &api.Error{Code: api.NotImplemented, Outcome: api.NotApplied} }

// BeginAuthorization answers not_implemented.
func (Unimplemented) BeginAuthorization(context.Context, api.BeginAuthorizationRequest) (api.Authorization, error) {
	return api.Authorization{}, absent()
}

// ObserveAuthorization answers not_implemented.
func (Unimplemented) ObserveAuthorization(context.Context, string, api.ObserveAuthorizationRequest) (api.AuthorizationStatus, error) {
	return api.AuthorizationStatus{}, absent()
}

// Identity answers not_implemented.
func (Unimplemented) Identity(context.Context, string) (resource.Identity, error) {
	return resource.Identity{}, absent()
}

// RevokeInstallation answers not_implemented.
func (Unimplemented) RevokeInstallation(context.Context, string) (api.Revocation, error) {
	return api.Revocation{}, absent()
}

// CreateShare answers not_implemented.
func (Unimplemented) CreateShare(context.Context, string, api.CreateShareRequest) (api.ShareAccess, error) {
	return api.ShareAccess{}, absent()
}

// ListShares answers not_implemented.
func (Unimplemented) ListShares(context.Context, string) (api.ListSharesResult, error) {
	return api.ListSharesResult{}, absent()
}

// RevokeShare answers not_implemented.
func (Unimplemented) RevokeShare(context.Context, string, api.RevokeShareRequest) (api.RevokeShareResult, error) {
	return api.RevokeShareResult{}, absent()
}
