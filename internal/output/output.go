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

// Package output is the CLI's design system and the only package that writes
// to the process streams. Command handlers hand it data; it renders.
//
// Four symbols, six colours, one pattern everywhere: labels dim, values at
// default brightness. Colours are basic ANSI only. Tables have no borders,
// dim uppercase headers and a two-space gutter. Errors read
// "✗ problem — remedy" with the remedy dim and any command in it bright.
// Nothing spins and the cursor never moves: output is append-only, so it
// survives tmux, logs and CI.
//
// Streams: data goes to stdout (the link of share and link, tables, the
// whoami block); every status line, prompt, hint and notice goes to stderr.
// Colour is decided once per process (see Color); without it the bytes are
// the same minus the escapes. The lint rule in .golangci.yml keeps printing
// out of every other package of the CLI.
package output

import (
	"io"
	"strings"
)

// Printer renders to one pair of streams with one colour decision. Every
// line is written with a single Write, so two commands sharing a terminal
// never interleave inside a line.
type Printer struct {
	out, err io.Writer
	color    bool
}

// New returns a printer for data stream out and status stream err.
func New(out, err io.Writer, color bool) *Printer {
	return &Printer{out: out, err: err, color: color}
}

// Link writes the one line of data that share and link produce: the link
// itself, never coloured, so a pipe or the clipboard receives exactly it.
func (p *Printer) Link(link string) { write(p.out, link+"\n") }

// Data writes one raw line to stdout (the hidden daemon commands' reports).
func (p *Printer) Data(line string) { write(p.out, line+"\n") }

// Diagnostic writes one raw line to stderr (the hidden daemon commands).
func (p *Printer) Diagnostic(line string) { write(p.err, line+"\n") }

// Blank separates blocks of status output.
func (p *Printer) Blank() { write(p.err, "\n") }

// Out is the data stream, for encoders that write a whole document
// themselves (daemon status --json).
func (p *Printer) Out() io.Writer { return p.out }

// Err is the status stream, for the daemon controller's own diagnostics.
func (p *Printer) Err() io.Writer { return p.err }

func write(w io.Writer, s string) { _, _ = io.WriteString(w, s) }

// join renders spans in order.
func (p *Printer) join(spans []Span) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(p.paint(s.style, s.text))
	}
	return b.String()
}
