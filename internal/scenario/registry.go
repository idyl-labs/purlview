package scenario

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/idyl-labs/purlview/internal/share"
)

// Scenario is one runnable command story.
type Scenario struct {
	// Name is area/case, stable, and the heading of its transcript.
	Name string
	// Title is one line for readers.
	Title string
	// Setup describes the fixtures in words.
	Setup string
	// Run drives the world and records expectations.
	Run func(c *Check, w *World)
}

var registry []Scenario

func register(scs ...Scenario) {
	registry = append(registry, scs...)
}

// areaOrder is the order of the areas in the transcript document, the
// order in which a person meets them.
var areaOrder = []string{"first-use", "login", "foreground", "background", "options", "connectivity", "startup", "list", "unshare", "logout", "whoami", "updates", "output", "access"}

func areaRank(a string) int {
	for i, x := range areaOrder {
		if x == a {
			return i
		}
	}
	return len(areaOrder)
}

// All returns every scenario, grouped by area in areaOrder and otherwise in
// registration order.
func All() []Scenario {
	out := append([]Scenario(nil), registry...)
	sort.SliceStable(out, func(i, j int) bool { return areaRank(out[i].Area()) < areaRank(out[j].Area()) })
	return out
}

// Areas returns the distinct areas in the order All uses.
func Areas() []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range All() {
		a := s.Area()
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// Area is the part of the name before the slash.
func (s Scenario) Area() string {
	if i := strings.Index(s.Name, "/"); i > 0 {
		return s.Name[:i]
	}
	return s.Name
}

// Find returns the scenario with the given name.
func Find(name string) (Scenario, bool) {
	for _, s := range registry {
		if s.Name == name {
			return s, true
		}
	}
	return Scenario{}, false
}

// Outcome is one executed scenario.
type Outcome struct {
	Scenario   Scenario
	Failures   []string
	Transcript []entry
	Took       time.Duration
}

// Passed reports whether every expectation held.
func (o Outcome) Passed() bool { return len(o.Failures) == 0 }

// Execute runs one scenario in a fresh world.
func Execute(sc Scenario) Outcome {
	w := NewWorld()
	c := &Check{}
	start := time.Now()
	func() {
		defer func() {
			if r := recover(); r != nil {
				c.Failf("panic: %v", r)
			}
		}()
		sc.Run(c, w)
	}()
	w.Daemon.Stop()
	return Outcome{Scenario: sc, Failures: c.Failures, Transcript: w.rec.snapshot(), Took: time.Since(start)}
}

// ExecuteAll runs every scenario.
func ExecuteAll() []Outcome {
	var out []Outcome
	for _, sc := range All() {
		out = append(out, Execute(sc))
	}
	return out
}

// RenderTranscript renders one transcript as plain text: the command line,
// each stdout and stderr line tagged with its stream, fixture actions as
// "--" notes and the exit status.
//
// When a second command starts while another is still running, the lines
// the first one prints from then on are held back and rendered after the
// second command's exit under a "continued" heading. Each command's own
// lines keep their order, so the rendering is deterministic even though
// the two commands run concurrently.
func RenderTranscript(entries []entry) string {
	var b strings.Builder
	var current *Invocation // the command whose lines render inline
	var running []*Invocation
	deferred := map[*Invocation][]entry{}
	var lastInv *Invocation // owner of the last rendered output line
	open := map[openLine]bool{}
	writeLine := func(e entry) {
		if e.inv != nil && e.inv != lastInv && e.kind != kindCommand {
			fmt.Fprintf(&b, "        (purlview %s, continued)\n", strings.Join(e.inv.args, " "))
		}
		switch e.kind {
		case kindCommand:
			fmt.Fprintf(&b, "$ %s\n", e.text)
		case kindExit:
			fmt.Fprintf(&b, "= %s\n", e.text)
		case kindStdout, kindStderr:
			tag := "stdout|"
			if e.kind == kindStderr {
				tag = "stderr|"
			}
			// A prompt leaves its line open; the newline that closes it
			// later belongs to that line, not to a new empty one.
			key := openLine{e.inv, e.kind}
			text := e.text
			if open[key] {
				text = strings.TrimPrefix(text, "\n")
			}
			open[key] = text != "" && !strings.HasSuffix(text, "\n")
			if text == "" {
				break
			}
			for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
				if line = strings.TrimRight(line, " "); line == "" {
					fmt.Fprintf(&b, "%s\n", tag)
				} else {
					fmt.Fprintf(&b, "%s %s\n", tag, line)
				}
			}
		}
		if e.inv != nil {
			lastInv = e.inv
		}
	}
	remove := func(inv *Invocation) {
		for i, r := range running {
			if r == inv {
				running = append(running[:i], running[i+1:]...)
				return
			}
		}
	}
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		switch e.kind {
		case kindCommand:
			running = append(running, e.inv)
			current = e.inv
			writeLine(e)
		case kindClock:
			total := e.d
			for i+1 < len(entries) && entries[i+1].kind == kindClock {
				i++
				total += entries[i].d
			}
			fmt.Fprintf(&b, "        -- clock +%s\n", formatAdvance(total))
		case kindNote:
			fmt.Fprintf(&b, "        -- %s\n", e.text)
		case kindStdout, kindStderr, kindExit:
			if e.inv != current {
				deferred[e.inv] = append(deferred[e.inv], e)
				continue
			}
			writeLine(e)
			if e.kind == kindExit {
				remove(e.inv)
				current = nil
				if n := len(running); n > 0 {
					current = running[n-1]
					for _, d := range deferred[current] {
						writeLine(d)
						if d.kind == kindExit {
							remove(current)
							current = nil
						}
					}
					delete(deferred, current)
				}
			}
		}
	}
	// A command still running at the end (killed, or abandoned) has no
	// exit line; anything it printed after being deferred is dropped with
	// it, which matches what a killed process would show.
	return b.String()
}

// openLine names one stream of one command.
type openLine struct {
	inv  *Invocation
	kind entryKind
}

// RenderDocument renders every outcome as the reviewable Markdown document
// committed at docs/cli-scenarios.md.
func RenderDocument(outcomes []Outcome) string {
	var b strings.Builder
	b.WriteString(documentHeader)
	byArea := map[string][]Outcome{}
	var areas []string
	for _, o := range outcomes {
		a := o.Scenario.Area()
		if _, ok := byArea[a]; !ok {
			areas = append(areas, a)
		}
		byArea[a] = append(byArea[a], o)
	}
	b.WriteString("## Index\n\n")
	for _, a := range areas {
		fmt.Fprintf(&b, "- **%s**:", a)
		names := make([]string, 0, len(byArea[a]))
		for _, o := range byArea[a] {
			names = append(names, fmt.Sprintf(" [%s](#%s)", o.Scenario.Name, anchor(o.Scenario.Name)))
		}
		b.WriteString(strings.Join(names, ","))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	for _, a := range areas {
		fmt.Fprintf(&b, "## %s\n\n", areaTitle(a))
		for _, o := range byArea[a] {
			fmt.Fprintf(&b, "### %s\n\n", o.Scenario.Name)
			fmt.Fprintf(&b, "%s\n\n", o.Scenario.Title)
			if o.Scenario.Setup != "" {
				fmt.Fprintf(&b, "Fixtures: %s\n\n", o.Scenario.Setup)
			}
			b.WriteString("```text\n")
			b.WriteString(RenderTranscript(o.Transcript))
			b.WriteString("```\n\n")
			if !o.Passed() {
				b.WriteString("**FAILED expectations:**\n\n")
				for _, f := range o.Failures {
					fmt.Fprintf(&b, "- %s\n", strings.ReplaceAll(f, "\n", "\n  "))
				}
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

func formatAdvance(d time.Duration) string {
	if d%time.Second == 0 {
		return share.FormatDuration(d)
	}
	return d.String()
}

func anchor(name string) string {
	return strings.NewReplacer("/", "", ".", "").Replace(name)
}

func areaTitle(a string) string {
	titles := map[string]string{
		"first-use": "First use", "login": "Login", "foreground": "Foreground share", "background": "Background share",
		"options": "Share options", "connectivity": "Connectivity", "startup": "Startup failures", "list": "List",
		"unshare": "Unshare", "logout": "Logout", "whoami": "Whoami", "updates": "Updates", "output": "Output and exit status", "access": "Recipient access",
	}
	if t, ok := titles[a]; ok {
		return t
	}
	return a
}

const documentHeader = `# CLI scenario transcripts

Generated by ` + "`go run ./internal/tools/scenario doc -write docs/cli-scenarios.md`" + `;
` + "`go test ./internal/scenario`" + ` fails when this file is stale. Do not edit by hand.

**Scenario mode.** Every transcript below comes from running the real
` + "`purlview`" + ` command handlers in-process against controlled fixtures: a fake
platform, fake sign-in, a fake target probe, fake release metadata and an
in-process daemon built on the real share engine, all on a fake clock.
Recipient scenarios also model host-only cookie jars, verification mail,
upstream requests and active streams against that same platform authority.
All tokens are visibly synthetic and deterministic, not cryptographic proof.
Access follows separate entry and content domains with single-use handoffs,
and a recipient may reach the whole origin of a share; ` + "`purlview logout`" + `
signs this device out locally. The recipient model is not a browser: real
browser isolation and the service's own enforcement are outside these
scenarios, and link scanners are not modelled.
Identities and domains are synthetic (` + "`example.invalid`, `purlview.invalid`" + `).
Nothing contacts a real service, opens a real browser or touches your own
daemon. A transcript demonstrates the intended command behaviour and the
contract it needs; it does not prove process or socket guarantees, which
the daemon lifecycle tests cover.

**Reading a transcript.** ` + "`$`" + ` is the command line; ` + "`stdout|`" + ` and ` + "`stderr|`" + `
tag each output line with its stream; ` + "`--`" + ` lines are fixture actions (a
lost connection, a clock advance, Ctrl-C); ` + "`= exit N`" + ` is the exit status.
When a second command runs while a foreground share is still attached, the
share's later lines appear after that command under a "continued" heading.
The fake clock starts at 2026-09-16 12:00:00 UTC and only moves when a
scenario advances it, so an hour of expiry costs no test time.

`
