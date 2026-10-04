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

package command

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/idyl-labs/purlview/internal/account"
	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/clock"
	"github.com/idyl-labs/purlview/internal/daemon"
	"github.com/idyl-labs/purlview/internal/output"
	"github.com/idyl-labs/purlview/internal/share"
	"github.com/idyl-labs/purlview/internal/updatecheck"

	"github.com/idyl-labs/purlview/sdk/api"
)

// Terminal describes the terminal the command runs in.
type Terminal struct {
	// Interactive is true when standard input and standard error are
	// terminals: prompts can be read and answered. Only then may a share
	// sign the user in on its own.
	Interactive bool
	// Attended is true when standard output and standard error are
	// terminals: nothing is redirected, so a person reads the update notice.
	Attended bool
}

// Browser opens a URL for the user.
type Browser interface {
	Open(url string) error
}

// BrowserFunc adapts a function to Browser.
type BrowserFunc func(url string) error

// Open calls the function.
func (f BrowserFunc) Open(url string) error { return f(url) }

// Deps are the boundaries the product commands act through. Production
// wiring (DefaultDeps) talks to the real daemon and platform; the scenario
// runner injects controlled fixtures.
type Deps struct {
	// ReadLine reads an echoed answer to a prompt; ReadSecret reads one
	// without echo (the sign-in code). ReadSecret defaults to ReadLine.
	ReadLine   func(context.Context) (string, error)
	ReadSecret func(context.Context) (string, error)
	Clock      clock.Clock
	// Local is the zone clock times are shown in.
	Local    *time.Location
	Terminal Terminal
	// Color is the colour decision, made once (output.Color).
	Color   bool
	Browser Browser
	// Store is local credential custody; Auth is the platform's sign-in.
	Store account.Store
	Auth  account.Authorizer
	// Directory is the platform's account-scoped share view; Runner is the
	// local daemon.
	Directory share.Directory
	Runner    share.Runner
	// NewID produces attempt ids (idempotency keys for share starts).
	NewID func() string
	// Hostname names this installation at sign-in (deviceName).
	Hostname func() string
	// UpdateAdvice works out the upgrade instruction for a notice.
	UpdateAdvice func(info buildinfo.Info) updatecheck.Advice
	// NoticeDue reports whether the update notice for a version may be shown
	// now, and records that it was: at most once per version per day.
	NoticeDue func(version string, now time.Time) bool
	// Getenv reads the environment (daemon paths, update repository).
	Getenv func(string) string
}

// DefaultDeps is the production wiring for the process streams s.
func DefaultDeps(s Streams, info buildinfo.Info, getenv func(string) string) Deps {
	if getenv == nil {
		getenv = osGetenv
	}
	client := platformClient(getenv)
	input := output.NewInput(s.In)
	// Colour is decided here, once: the environment first, then whether
	// stdout is a terminal that accepts escapes.
	color := output.Color(getenv, output.IsTerminal(s.Out))
	if color && !output.EnableColor(s.Out, s.Err) {
		color = false
	}
	return Deps{
		ReadLine:   input.Line,
		ReadSecret: input.Secret,
		Clock:      clock.Real{},
		Local:      time.Local,
		Terminal:   Terminal{Interactive: output.IsTerminal(s.In) && output.IsTerminal(s.Err), Attended: output.IsTerminal(s.Out) && output.IsTerminal(s.Err)},
		Color:      color,
		Browser:    BrowserFunc(openBrowser),
		// The credential is kept in a file only this user can read
		// (account.FileStore), scoped to the configured platform.
		Store:     &envFileStore{getenv: getenv},
		Auth:      account.SDK{Client: client},
		Directory: share.SDKDirectory{Client: client},
		Runner: &daemonRunner{getenv: getenv, info: info, replaced: func() {
			output.New(s.Out, s.Err, color).Status(output.Msg(output.Attend, "Purlview was updated").Remedy("shares started before the update have stopped"))
		}},
		NewID:    randomID,
		Hostname: deviceName,
		UpdateAdvice: func(info buildinfo.Info) updatecheck.Advice {
			repo := updatecheck.DefaultReleaseRepository()
			if v := getenv(updatecheck.EnvRepository); v != "" {
				repo = v
			}
			exe, err := os.Executable()
			if err != nil {
				exe = ""
			}
			return updatecheck.Detect(exe, info, getenv, repo)
		},
		NoticeDue: func(version string, now time.Time) bool {
			p, err := daemon.Resolve(getenv)
			return err == nil && updatecheck.NoticeLog{Path: p.UpdateNoticePath()}.Due(version, now)
		},
		Getenv: getenv,
	}
}

func (d Deps) withDefaults() Deps {
	if d.ReadLine == nil {
		d.ReadLine = func(context.Context) (string, error) {
			return "", io.EOF
		}
	}
	if d.ReadSecret == nil {
		d.ReadSecret = d.ReadLine
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Local == nil {
		d.Local = time.UTC
	}
	if d.NoticeDue == nil {
		d.NoticeDue = func(string, time.Time) bool { return true }
	}
	if d.Browser == nil {
		d.Browser = BrowserFunc(func(string) error { return errors.New("no browser available") })
	}
	if d.Store == nil {
		d.Store = &account.MemoryStore{}
	}
	if d.Auth == nil {
		d.Auth = account.Unimplemented{}
	}
	if d.Directory == nil {
		d.Directory = share.UnimplementedDirectory{}
	}
	if d.Runner == nil {
		d.Runner = unavailableRunner{}
	}
	if d.NewID == nil {
		d.NewID = randomID
	}
	if d.Hostname == nil {
		d.Hostname = deviceName
	}
	if d.UpdateAdvice == nil {
		d.UpdateAdvice = func(buildinfo.Info) updatecheck.Advice { return updatecheck.Advice{Method: "unknown"} }
	}
	if d.Getenv == nil {
		d.Getenv = osGetenv
	}
	return d
}

// envFileStore resolves the credential path from the environment on first
// use, so building the command tree (help, completion, documentation
// generation) touches nothing.
type envFileStore struct {
	getenv func(string) string
}

func (s *envFileStore) store() (*account.FileStore, error) {
	p, err := daemon.Resolve(s.getenv)
	if err != nil {
		return nil, err
	}
	return account.ScopedFileStore(p.CredentialsPath(), s.getenv("PURLVIEW_ENVIRONMENT"), s.getenv("PURLVIEW_PLATFORM_ENDPOINT"), s.getenv("PURLVIEW_PLATFORM_HOST")), nil
}

func (s *envFileStore) Load() (*api.InstallationCredential, error) {
	fs, err := s.store()
	if err != nil {
		return nil, err
	}
	return fs.Load()
}

func (s *envFileStore) Save(c *api.InstallationCredential) error {
	fs, err := s.store()
	if err != nil {
		return err
	}
	return fs.Save(c)
}

func (s *envFileStore) Clear() error {
	fs, err := s.store()
	if err != nil {
		return err
	}
	return fs.Clear()
}

// unavailableRunner is the fallback when no runner is injected.
type unavailableRunner struct{}

func (unavailableRunner) Open(context.Context, bool) (share.Session, error) {
	return nil, errors.New("no local daemon is configured")
}

func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("command: random id: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// openBrowser opens an http(s) URL with the platform's default handler and
// returns once the handler has been started.
func openBrowser(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("refusing to open %q: not an http(s) URL", raw)
	}
	// The executable is fixed per platform and the only variable argument
	// is the validated http(s) URL above.
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", raw) //nolint:gosec // validated URL, fixed executable
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", raw) //nolint:gosec // validated URL, fixed executable
	default:
		cmd = exec.Command("xdg-open", raw) //nolint:gosec // validated URL, fixed executable
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// Time bounds of the command side.
const (
	// loginWait caps how long login waits for approval when the platform
	// does not say.
	loginWait = 10 * time.Minute
	// remoteCallWait bounds one account-scoped platform call from the CLI.
	remoteCallWait = 10 * time.Second
	// validateWait bounds whoami's online check.
	validateWait = 3 * time.Second
	// stopWait bounds a stop and its remote confirmation after Ctrl-C.
	stopWait = 15 * time.Second
	// updateNoticeWait bounds how long the daemon may hold a fresh update
	// answer for a share that is still running; the daemon finishes the
	// check into its cache regardless. The command never waits for it.
	updateNoticeWait = 1500 * time.Millisecond
	// localCallWait bounds one local daemon call that answers from memory.
	localCallWait = time.Second
)

// osGetenv is the default environment for the executable.
func osGetenv(key string) string { return os.Getenv(key) }
