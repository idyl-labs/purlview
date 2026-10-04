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

import (
	"strings"
	"time"
)

// Symbol opens a status line.
type Symbol int

// The five symbols. None is an orderly ending: no symbol, no colour.
const (
	None      Symbol = iota
	Done             // ✓
	Failure          // ✗
	Attend           // !
	Live             // ●
	hintArrow        // →
)

func (p *Printer) symbol(s Symbol) string {
	switch s {
	case Done:
		return p.paint(styleGreen, "✓") + " "
	case Failure:
		return p.paint(styleRed, "✗") + " "
	case Attend:
		return p.paint(styleAmber, "!") + " "
	case Live:
		return p.paint(styleGreen, "●") + " "
	}
	return ""
}

// Message is one status line as data: "symbol text — remedy". Handlers build
// messages; only the printer turns them into bytes.
type Message struct {
	Symbol Symbol
	text   []Span
	remedy []Span
}

// Msg builds a message. Bare strings are text at default brightness.
func Msg(sym Symbol, parts ...any) Message {
	return Message{Symbol: sym, text: spans(stylePlain, parts)}
}

// Remedy adds what to do about it, after an em dash. Bare strings are dim;
// wrap the command to run in Text so it stays bright.
func (m Message) Remedy(parts ...any) Message {
	m.remedy = spans(styleDim, parts)
	return m
}

// String is the message without colour.
func (m Message) String() string { return New(nil, nil, false).line(m) }

func (p *Printer) line(m Message) string {
	var b strings.Builder
	b.WriteString(p.symbol(m.Symbol))
	b.WriteString(p.join(m.text))
	if len(m.remedy) > 0 {
		// The dash is dim like the words after it; a command between them
		// carries its own brightness, so no colour ever nests.
		sep, rest := "— ", m.remedy
		if rest[0].style == styleDim {
			sep, rest = sep+rest[0].text, rest[1:]
		}
		b.WriteString(" " + p.paint(styleDim, sep))
		b.WriteString(p.join(rest))
	}
	return b.String()
}

// Status prints one status line on stderr.
func (p *Printer) Status(m Message) { write(p.err, p.line(m)+"\n") }

// Event prints one session event: the local time, a two-space gutter and the
// message. Events are appended below the share block, one line each.
func (p *Printer) Event(at time.Time, m Message) {
	write(p.err, Clock(at)+"  "+p.line(m)+"\n")
}

// Hint prints "→ command", all dim: the one thing to run next.
func (p *Printer) Hint(command string) { write(p.err, p.paint(styleDim, "→ "+command)+"\n") }

// Cause prints the underlying cause on its own dim line (PURLVIEW_DEBUG=1).
func (p *Printer) Cause(text string) { write(p.err, p.paint(styleDim, "  "+text)+"\n") }

// Usage points a usage error at the command's help.
func (p *Printer) Usage(commandPath string) {
	write(p.err, p.paint(styleDim, "Run '"+commandPath+" --help' for usage.")+"\n")
}

// Empty prints an empty state and the command that fills it.
func (p *Printer) Empty(text, command string) {
	write(p.err, text+"\n")
	p.Hint(command)
}

// Clock renders a time of day the way the share block and the events do.
func Clock(t time.Time) string { return t.Format("3:04 PM") }
