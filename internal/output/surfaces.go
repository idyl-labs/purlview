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
	"fmt"
	"strings"
)

// ShareBlock is everything share says once the link works.
type ShareBlock struct {
	// App is what is shared, as the developer names it: localhost:3000.
	// More names the further apps of a share with several, in order.
	App  string
	More []string
	Link string
	// Recipients restricts access; empty means anyone with the link.
	Recipients []string
	// InvitesSent and InvitesNotSent are the platform's report; both are
	// empty when it attempted none.
	InvitesSent, InvitesNotSent []string
	// Opens is the start page when the app was shared with a path or query.
	Opens string
	// ExpiresIn is the time left (1h); ExpiresAt the local time of day.
	ExpiresIn, ExpiresAt string
	// StopID is set for a background share: the id its stop command takes.
	// A foreground share stops with Ctrl-C.
	StopID string
}

// Share prints the share block. The link is the only line on stdout.
func (p *Printer) Share(b ShareBlock) {
	p.Status(Msg(Done, "Sharing ", Name(b.App)))
	for _, app := range b.More {
		// One line per further app, under the first: "  + localhost:8000".
		write(p.err, "  + "+p.join([]Span{Name(app)})+"\n")
	}
	if len(b.InvitesSent) > 0 {
		word := "Invite"
		if len(b.InvitesSent) > 1 {
			word = "Invites"
		}
		p.Status(Msg(Done, word+" sent to ", List(b.InvitesSent)))
	}
	for _, email := range b.InvitesNotSent {
		p.Status(Msg(Attend, "Invite not sent to ", email).Remedy("send them the link yourself"))
	}
	p.Blank()
	p.Link(b.Link)
	p.Blank()
	access := KV("Access", "Anyone with this link")
	if len(b.Recipients) > 0 {
		access = KV("Access", "Only ", List(b.Recipients))
	}
	pairs := []Pair{access}
	if b.Opens != "" {
		pairs = append(pairs, KV("Opens", b.Opens, Dim(" · the whole app is reachable")))
	}
	stop := KV("Stop", "Ctrl-C")
	if b.StopID != "" {
		stop = KV("Stop", "purlview stop ", Name(b.StopID))
	}
	pairs = append(pairs, KV("Expires", "in "+b.ExpiresIn, Dim(" · "+b.ExpiresAt)), stop)
	p.pairs(p.err, pairs)
}

// List joins names the way a sentence does: a, b and c.
func List(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// Count is "1 share", "2 shares".
func Count(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// SignIn opens the sign-in block.
func (p *Printer) SignIn() {
	write(p.err, "\n"+p.paint(styleDim, "Sign in to")+" Purlview\n\n")
}

// Prompt prints an indented dim label and leaves the cursor after it.
func (p *Printer) Prompt(label string) { write(p.err, "  "+p.paint(styleDim, label)+" ") }

// PromptEnd finishes a prompt line when the terminal did not: after hidden
// input, after input that is not a terminal, and after Ctrl-C.
func (p *Printer) PromptEnd() { write(p.err, "\n") }

// Note prints an indented dim line inside a prompt block.
func (p *Printer) Note(text string) { write(p.err, "  "+p.paint(styleDim, text)+"\n") }

// Confirm asks a destructive question inline. It names what stops and
// defaults to No.
func (p *Printer) Confirm(question string) {
	write(p.err, question+" "+p.paint(styleDim, "[y/N]")+" ")
}

// Identity is the whoami block.
type Identity struct {
	Account, Device string
	Status          Span
}

// Whoami prints the identity block on stdout.
func (p *Printer) Whoami(id Identity) {
	p.pairs(p.out, []Pair{KV("Account", id.Account), KV("Device", id.Device), KV("Status", id.Status)})
}

// Update is a newer release and how this copy is updated.
type Update struct {
	Latest, Current string
	// Command updates this copy the way it was installed; Page is where to
	// get the release when no single command does.
	Command, Page string
}

// UpdateNotice prints the two dim lines of the update notice on stderr.
func (p *Printer) UpdateNotice(u Update) {
	how := Text(u.Command)
	if u.Command == "" {
		how = URL(u.Page)
	}
	write(p.err, p.paint(styleDim, fmt.Sprintf("A newer Purlview is available: %s (you have %s)", u.Latest, u.Current))+"\n"+
		p.join([]Span{Dim("Update:"), Text(" "), how, Dim(" — updating stops running shares")})+"\n")
}
