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

// Package ipc is the private local control protocol between the purlview
// CLI and the per-user daemon.
//
// Transport: a Unix-domain socket on macOS and Linux, a named pipe with an
// explicit owner-only security descriptor on Windows. Nothing listens on a
// network port. Both ends verify that the peer belongs to the same OS user.
//
// Framing: newline-delimited JSON. Every line is one message of at most
// MaxMessageSize bytes. A client sends requests and receives responses that
// carry the same id, plus unsolicited events once it has subscribed.
//
// The protocol version (ProtocolVersion) is independent of the executable's
// build version: the build changes on every release, the protocol only when a
// message shape changes incompatibly. The handshake carries both so a CLI can
// tell an older daemon of the same protocol from an incompatible one.
package ipc

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the version of this message set. Bump it only for
// incompatible changes; a daemon always answers "hello", "status" and
// "shutdown" regardless of the client's protocol version so an installed CLI
// can retire an older daemon.
//
// Version 2 replaced the share messages' scalar "recipient" with
// "recipients". Parameters are decoded leniently, so a version 1 peer would
// drop the list and treat a restricted share as a secret-link one. The share
// event kinds added with it are additive on their own.
//
// Version 3 added "targets" and "no_rewrite" to the share start and record,
// and dropped the never-accepted "rewrite_urls" from the start. A version 2
// daemon would drop the list and share the first target alone.
//
// The record's "end_app" came later without a bump: it is optional, and a
// peer that does not know it loses only the name of the failing app, which
// the ended event also carries.
const ProtocolVersion = 3

// MaxMessageSize bounds one framed message (request, response or event).
const MaxMessageSize = 64 * 1024

// Operation names.
const (
	OpHello       = "hello"
	OpStatus      = "status"
	OpShutdown    = "shutdown"
	OpSubscribe   = "subscribe"
	OpUpdateCheck = "update.check"

	// Share operations: the local half of a share's lifecycle. A daemon
	// without a platform configured answers them with not_implemented; it
	// never pretends to have created, stopped or listed a share.
	OpShareStart   = "share.start"
	OpShareStop    = "share.stop"
	OpShareStopAll = "share.stop_all"
	OpShareList    = "share.list"

	// Test-only operations, served only when the daemon runs with test
	// resources enabled. They exercise ownership and cancellation with a
	// resource that does nothing; they are not shares.
	OpTestStart  = "test.op.start"
	OpTestStop   = "test.op.stop"
	OpTestList   = "test.op.list"
	OpTestRevoke = "test.op.revoke"
)

// Event names.
const (
	EventOpStarted      = "op.started"
	EventOpEnded        = "op.ended"
	EventDaemonShutdown = "daemon.shutdown"
	// EventShare carries ShareEventData for subscribers.
	EventShare = "share.event"
	// EventOverflow is sent once before the daemon disconnects a subscriber
	// that did not keep up with the event stream.
	EventOverflow = "subscriber.overflow"
)

// Error codes carried in Response.Error.Code.
const (
	CodeBadRequest       = "bad_request"
	CodeUnknownOp        = "unknown_operation"
	CodeProtocolMismatch = "protocol_mismatch"
	CodeNotImplemented   = "not_implemented"
	CodeNotFound         = "not_found"
	CodeShuttingDown     = "shutting_down"
	CodeInternal         = "internal"
)

// Request is one client message.
type Request struct {
	ID     string          `json:"id"`
	Op     string          `json:"op"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response answers a Request with the same ID.
type Response struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Event is an unsolicited daemon message for subscribed clients.
type Event struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Error is a protocol-level failure.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (%s)", e.Message, e.Code) }

// message is the wire shape of every line; exactly one of the request,
// response or event fields is meaningful, distinguished by "op" (request),
// "event" (event) or otherwise a response.
type message struct {
	ID     string          `json:"id,omitempty"`
	Op     string          `json:"op,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	OK     *bool           `json:"ok,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// BuildIdentity identifies an executable build in the handshake.
type BuildIdentity struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Modified bool   `json:"modified,omitempty"`
	BuiltBy  string `json:"built_by,omitempty"`
}

// HelloParams opens a connection.
type HelloParams struct {
	Protocol int           `json:"protocol"`
	Client   string        `json:"client"`
	Build    BuildIdentity `json:"build"`
}

// HelloResult describes the daemon that answered.
type HelloResult struct {
	Protocol   int           `json:"protocol"`
	Instance   string        `json:"instance"`
	PID        int           `json:"pid"`
	StartedAt  string        `json:"started_at"`
	Build      BuildIdentity `json:"build"`
	Executable string        `json:"executable"`
	// ExeSize and ExeModTime identify the executable file the daemon was
	// started from, so a CLI can tell that the installed file was replaced
	// even when the version string did not change (development builds).
	ExeSize    int64  `json:"exe_size"`
	ExeModTime string `json:"exe_mod_time"`
	Ready      bool   `json:"ready"`
	// TestResources reports that the test-only operations are enabled.
	TestResources bool `json:"test_resources"`
	// Console reports how the daemon process is attached to a terminal:
	// "none" when it has no controlling terminal or console.
	Console string `json:"console"`
}

// StatusResult is the daemon's current state.
type StatusResult struct {
	Instance      string      `json:"instance"`
	PID           int         `json:"pid"`
	StartedAt     string      `json:"started_at"`
	UptimeSeconds float64     `json:"uptime_seconds"`
	Clients       int         `json:"clients"`
	Operations    []Operation `json:"operations"`
	IdleExitAt    string      `json:"idle_exit_at,omitempty"`
	ShuttingDown  bool        `json:"shutting_down"`
}

// Operation describes owned work.
type Operation struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Owner     string `json:"owner"` // "client" (attached) or "daemon" (detached)
	State     string `json:"state"` // "active" or "ended"
	StartedAt string `json:"started_at"`
	Deadline  string `json:"deadline,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// ShutdownParams asks the daemon to exit.
type ShutdownParams struct {
	Reason string `json:"reason,omitempty"`
}

// ShutdownResult acknowledges the request; the daemon exits afterwards.
type ShutdownResult struct {
	Operations int `json:"operations_ended"`
}

// TestStartParams starts a test-only operation.
type TestStartParams struct {
	Name     string `json:"name"`
	Detached bool   `json:"detached"`
	TTLMs    int64  `json:"ttl_ms,omitempty"`
}

// TestStopParams stops or revokes a test-only operation.
type TestStopParams struct {
	ID string `json:"id"`
}

// TestStopResult reports the outcome; stopping twice is not an error.
type TestStopResult struct {
	Operation    Operation `json:"operation"`
	AlreadyEnded bool      `json:"already_ended"`
}

// TestListResult lists operations.
type TestListResult struct {
	Operations []Operation `json:"operations"`
}

// UpdateCheckParams asks the daemon to run or join a metadata check.
type UpdateCheckParams struct {
	// Channel is "stable" or "prerelease".
	Channel string `json:"channel"`
	// WaitMs bounds how long the request waits for an in-flight check.
	WaitMs int64 `json:"wait_ms"`
}

// UpdateCheckResult carries the cached or fresh release metadata.
type UpdateCheckResult struct {
	// Status is "fresh" (the check completed during this request), "cached"
	// (a previous result; a check may still be running) or "none".
	Status    string       `json:"status"`
	Latest    *ReleaseInfo `json:"latest,omitempty"`
	CheckedAt string       `json:"checked_at,omitempty"`
	// Outcome names what the last check observed ("ok", "not_modified",
	// "no_release", "rate_limited", "network", "malformed", "disabled").
	Outcome string `json:"outcome,omitempty"`
}

// ReleaseInfo is one published release from the metadata source.
type ReleaseInfo struct {
	Version    string `json:"version"`
	Tag        string `json:"tag"`
	Prerelease bool   `json:"prerelease"`
	URL        string `json:"url"`
}

// OpEndedData accompanies EventOpEnded.
type OpEndedData struct {
	Operation Operation `json:"operation"`
}

// ShareCredential is the installation credential the CLI hands to the
// daemon at share start. The daemon keeps it in memory for reconnects and
// never writes it anywhere.
type ShareCredential struct {
	Account     string `json:"account"`
	AccountID   string `json:"account_id"`
	Device      string `json:"device"`
	DeviceLabel string `json:"device_label"`
	Token       string `json:"token"`
}

// ShareStartParams asks the daemon to create and serve a share. Target is the
// first target; Targets is the whole list when there are several, the first
// included. NoRewrite leaves the apps' addresses in response bodies alone.
type ShareStartParams struct {
	// Attempt is the CLI's idempotency key; a repeated start with the same
	// attempt returns the same share.
	Attempt string `json:"attempt"`
	// Owner is "attached" (ends with this connection) or "detached".
	Owner      string          `json:"owner"`
	Target     string          `json:"target"`
	Targets    []string        `json:"targets,omitempty"`
	TTLMs      int64           `json:"ttl_ms"`
	Recipients []string        `json:"recipients,omitempty"`
	NoRewrite  bool            `json:"no_rewrite,omitempty"`
	Credential ShareCredential `json:"credential"`
}

// ShareInvite reports one recipient's invite email: status "sent" or "not_sent".
type ShareInvite struct {
	Email  string `json:"email"`
	Status string `json:"status"`
}

// ShareRecord is the daemon's view of one share. Invites is what share
// creation reported, one entry per recipient; absent when none were attempted.
type ShareRecord struct {
	Origin      string        `json:"origin,omitempty"`
	ID          string        `json:"id,omitempty"`
	URL         string        `json:"url,omitempty"`
	Attempt     string        `json:"attempt"`
	Target      string        `json:"target"`
	Targets     []string      `json:"targets,omitempty"`
	Device      string        `json:"device"`
	DeviceLabel string        `json:"device_label,omitempty"`
	Recipients  []string      `json:"recipients,omitempty"`
	Invites     []ShareInvite `json:"invites,omitempty"`
	RewriteURLs bool          `json:"rewrite_urls,omitempty"`
	NoRewrite   bool          `json:"no_rewrite,omitempty"`
	Owner       string        `json:"owner"`
	// State is "starting", "ready", "reconnecting" or "ended".
	State     string `json:"state"`
	CreatedAt string `json:"created_at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	EndReason string `json:"end_reason,omitempty"`
	// EndApp names the app whose failure ended the share; a peer that does
	// not know it ignores it, so it needed no version.
	EndApp string `json:"end_app,omitempty"`
	// EndLimit names the account limit that refused the share; a peer that
	// does not know it ignores it, so it needed no version.
	EndLimit string `json:"end_limit,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// ShareStopParams ends one share by id or attempt.
type ShareStopParams struct {
	ID      string `json:"id,omitempty"`
	Attempt string `json:"attempt,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// ShareRemote reports whether the platform confirmed an end: "confirmed",
// "unconfirmed" or "none".
type ShareRemote struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// ShareStopResult reports what a stop did.
type ShareStopResult struct {
	Share        ShareRecord `json:"share"`
	AlreadyEnded bool        `json:"already_ended"`
	Remote       ShareRemote `json:"remote"`
}

// ShareStopAllParams ends every share the daemon owns.
type ShareStopAllParams struct {
	Reason string `json:"reason"`
}

// ShareStopAllResult reports how many shares were active.
type ShareStopAllResult struct {
	Ended int `json:"ended"`
}

// ShareListResult lists the daemon's shares, active first.
type ShareListResult struct {
	Shares []ShareRecord `json:"shares"`
}

// ShareEventData accompanies EventShare. Kind is "ready",
// "connection_lost", "restored", "ended", or one of the additive kinds
// "first_visitor", "app_unresponsive" and "app_responding", whose Detail names
// the app (localhost:3000). A client ignores kinds it does not know.
type ShareEventData struct {
	Kind   string      `json:"kind"`
	Share  ShareRecord `json:"share"`
	Remote ShareRemote `json:"remote"`
	Detail string      `json:"detail,omitempty"`
}
