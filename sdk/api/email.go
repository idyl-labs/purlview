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
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/idyl-labs/purlview/sdk/resource"
)

// LoginStartPath is the anonymous creator email challenge endpoint.
const LoginStartPath = "/api/v1/login/start"

// LoginVerifyPath is the atomic email-code verification endpoint.
const LoginVerifyPath = "/api/v1/login/verify"

// LoginResendPath is the bounded challenge resend endpoint.
const LoginResendPath = "/api/v1/login/resend"

// LoginStartRequest selects an email and installation label.
type LoginStartRequest struct {
	Email       string `json:"email"`
	DeviceLabel string `json:"device_label"`
}

// Validate checks wire structure; current authority remains a server decision.
func (r LoginStartRequest) Validate() error {
	if r.Email == "" || !resource.Recipient(r.Email) || !resource.Text(r.DeviceLabel, 256) {
		return errors.New("invalid login request")
	}
	return nil
}

// LoginChallenge contains only the challenge identity and its time bounds.
type LoginChallenge struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
	ResendAt  time.Time `json:"resend_at"`
}

// Validate checks wire structure; current authority remains a server decision.
func (r LoginChallenge) Validate() error {
	if !resource.Identifier(r.ID) || r.ExpiresAt.IsZero() || r.ResendAt.IsZero() {
		return errors.New("invalid challenge")
	}
	return nil
}

// LoginCodeDigits is the length of an emailed sign-in code. LoginAttempts is
// how many codes one challenge accepts: a miss reports 1 to LoginAttempts-1
// attempts left, and the last one ends the challenge.
const (
	LoginCodeDigits = 6
	LoginAttempts   = 5
)

var loginCode = regexp.MustCompile(fmt.Sprintf(`^[0-9]{%d}$`, LoginCodeDigits))

// NormaliseLoginCode removes the spaces and dashes a person types or pastes
// ("482 913", "482-913"), so every client accepts the same input. The result
// still needs LoginVerifyRequest.Validate.
func NormaliseLoginCode(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.Is(unicode.Pd, r) {
			return -1
		}
		return r
	}, s)
}

// LoginVerifyRequest supplies the short-lived email code for one challenge:
// exactly LoginCodeDigits digits.
type LoginVerifyRequest struct {
	ID   string `json:"id"`
	Code string `json:"code"`
}

// Validate checks wire structure; current authority remains a server decision.
func (r LoginVerifyRequest) Validate() error {
	if !resource.Identifier(r.ID) || !loginCode.MatchString(r.Code) {
		return errors.New("invalid code")
	}
	return nil
}

// LoginResendRequest identifies an existing challenge without extending its lifetime.
type LoginResendRequest struct {
	ID string `json:"id"`
}

// Validate checks wire structure; current authority remains a server decision.
func (r LoginResendRequest) Validate() error {
	if !resource.Identifier(r.ID) {
		return errors.New("invalid challenge")
	}
	return nil
}
