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

// Command scenario is the development runner for the CLI interaction
// scenarios. It is not part of the installed purlview executable.
//
//	go run ./internal/tools/scenario list
//	go run ./internal/tools/scenario run <name>...
//	go run ./internal/tools/scenario run --all
//	go run ./internal/tools/scenario doc [-write internal/scenario/testdata/transcripts.md]
//
// Every scenario runs the real command handlers in-process against
// controlled fixtures (SCENARIO MODE): synthetic identities, reserved
// example domains, an in-process daemon and a fake clock. Nothing reaches a
// real service, a real browser or your own daemon.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/idyl-labs/purlview/internal/scenario"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "scenario:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usage(stderr)
	}
	switch args[0] {
	case "list":
		for _, sc := range scenario.All() {
			fmt.Fprintf(stdout, "%-52s %s\n", sc.Name, sc.Title)
		}
		return nil
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fs.SetOutput(stderr)
		all := fs.Bool("all", false, "run every scenario")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var scs []scenario.Scenario
		if *all {
			scs = scenario.All()
		}
		for _, name := range fs.Args() {
			sc, ok := scenario.Find(name)
			if !ok {
				return fmt.Errorf("unknown scenario %q (see: list)", name)
			}
			scs = append(scs, sc)
		}
		if len(scs) == 0 {
			return fmt.Errorf("name at least one scenario, or use --all")
		}
		fmt.Fprintln(stderr, "SCENARIO MODE: controlled fixtures, synthetic identities (example.invalid), in-process daemon, fake clock. Nothing here contacts a real service or your daemon.")
		failed := 0
		for _, sc := range scs {
			o := scenario.Execute(sc)
			fmt.Fprintf(stdout, "\n## %s\n%s\nFixtures: %s\n\n", sc.Name, sc.Title, sc.Setup)
			fmt.Fprint(stdout, scenario.RenderTranscript(o.Transcript))
			if o.Passed() {
				fmt.Fprintf(stdout, "\nPASS %s (%s)\n", sc.Name, o.Took.Round(1e6))
			} else {
				failed++
				fmt.Fprintf(stdout, "\nFAIL %s\n", sc.Name)
				for _, f := range o.Failures {
					fmt.Fprintf(stdout, "  - %s\n", strings.ReplaceAll(f, "\n", "\n    "))
				}
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d scenarios failed", failed, len(scs))
		}
		fmt.Fprintf(stdout, "\n%d scenarios passed\n", len(scs))
		return nil
	case "doc":
		fs := flag.NewFlagSet("doc", flag.ContinueOnError)
		fs.SetOutput(stderr)
		write := fs.String("write", "", "write the document to this path instead of stdout")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		outcomes := scenario.ExecuteAll()
		doc := scenario.RenderDocument(outcomes)
		failed := 0
		for _, o := range outcomes {
			if !o.Passed() {
				failed++
				fmt.Fprintf(stderr, "FAIL %s\n", o.Scenario.Name)
			}
		}
		if *write != "" {
			if err := os.WriteFile(*write, []byte(doc), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(stderr, "wrote %s (%d scenarios)\n", *write, len(outcomes))
		} else {
			fmt.Fprint(stdout, doc)
		}
		if failed > 0 {
			return fmt.Errorf("%d scenarios failed; the document records their failures", failed)
		}
		return nil
	default:
		return usage(stderr)
	}
}

func usage(w io.Writer) error {
	fmt.Fprintln(w, "usage: scenario list | run <name>... | run --all | doc [-write <path>]")
	return fmt.Errorf("unknown command")
}
