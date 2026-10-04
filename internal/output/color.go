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

package output

import "strings"

// Basic ANSI colours only: they follow the terminal's own palette.
const (
	ansiReset = "\x1b[0m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiAmber = "\x1b[33m"
	ansiBlue  = "\x1b[34m"
	ansiCyan  = "\x1b[36m"
	ansiDim   = "\x1b[90m"
)

type style int

const (
	stylePlain style = iota
	styleDim         // labels, hints, remedies
	styleGreen       // working
	styleAmber       // in progress, needs attention
	styleRed         // failed
	styleCyan        // names and ids
	styleBlue        // links
)

var codes = map[style]string{styleDim: ansiDim, styleGreen: ansiGreen, styleAmber: ansiAmber, styleRed: ansiRed, styleCyan: ansiCyan, styleBlue: ansiBlue}

// Color makes the colour decision, once per process: NO_COLOR (any value)
// wins, then FORCE_COLOR (0, false and none switch colour off, anything else
// on), then whether stdout is a terminal.
func Color(getenv func(string) string, stdoutIsTerminal bool) bool {
	if getenv("NO_COLOR") != "" {
		return false
	}
	if force := getenv("FORCE_COLOR"); force != "" {
		switch strings.ToLower(force) {
		case "0", "false", "none":
			return false
		}
		return true
	}
	return stdoutIsTerminal
}

func (p *Printer) paint(st style, s string) string {
	code, ok := codes[st]
	if !p.color || !ok || s == "" {
		return s
	}
	return code + s + ansiReset
}

// Span is a run of text with one meaning. Colour is never the only carrier
// of that meaning: every state also has a symbol and a word.
type Span struct {
	text  string
	style style
}

// Text is a value at default brightness. Inside a remedy it marks the
// command to run.
func Text(s string) Span { return Span{s, stylePlain} }

// Dim is a label, a hint or a remedy.
func Dim(s string) Span { return Span{s, styleDim} }

// Name is a name or an id: an app, a share, a device, an account.
func Name(s string) Span { return Span{s, styleCyan} }

// URL is a link inside a status line.
func URL(s string) Span { return Span{s, styleBlue} }

// Working colours a status word for something that works.
func Working(s string) Span { return Span{s, styleGreen} }

// Attention colours a status word for something in progress or waiting on
// the reader.
func Attention(s string) Span { return Span{s, styleAmber} }

// Failed colours a status word for a failure. An orderly ending is not a
// failure and stays uncoloured (Text).
func Failed(s string) Span { return Span{s, styleRed} }

// spans turns strings and spans into spans; a bare string takes style st.
func spans(st style, parts []any) []Span {
	out := make([]Span, 0, len(parts))
	for _, part := range parts {
		switch v := part.(type) {
		case Span:
			out = append(out, v)
		case string:
			out = append(out, Span{v, st})
		}
	}
	return out
}
