package output

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

// IsTerminal reports whether a stream is a terminal (a Windows console
// included). Anything that is not an *os.File is not.
func IsTerminal(stream any) bool {
	f, ok := stream.(*os.File)
	return ok && f != nil && term.IsTerminal(int(f.Fd()))
}

// Input reads what a person types at a prompt. One buffered reader serves
// every prompt of the process, so piped answers meant for a later prompt are
// not swallowed by an earlier one, and a read that Ctrl-C abandoned hands
// its line to the next prompt instead of losing it.
type Input struct {
	in io.Reader

	mu       sync.Mutex
	buf      *bufio.Reader
	inflight chan inputLine
}

type inputLine struct {
	text string
	err  error
}

// NewInput reads prompts from in.
func NewInput(in io.Reader) *Input { return &Input{in: in, buf: bufio.NewReader(in)} }

// Line reads one echoed line, without its line ending and surrounding space.
// A cancelled context returns its error at once.
func (i *Input) Line(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	i.mu.Lock()
	if i.inflight == nil {
		ch := make(chan inputLine, 1)
		go func() {
			text, err := i.buf.ReadString('\n')
			if err == io.EOF && text != "" {
				err = nil
			}
			ch <- inputLine{strings.TrimSpace(text), err}
		}()
		i.inflight = ch
	}
	ch := i.inflight
	i.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case v := <-ch:
		i.mu.Lock()
		if i.inflight == ch {
			i.inflight = nil
		}
		i.mu.Unlock()
		return v.text, v.err
	}
}

// Secret reads one line without echo when the input is a terminal, so a
// sign-in code reaches neither the screen nor the scrollback; it is typed at
// a prompt, never passed as an argument, so it is not in shell history
// either. Input that is not a terminal has no echo to switch off and is read
// as a line. Ctrl-C restores the terminal before returning.
func (i *Input) Secret(ctx context.Context) (string, error) {
	f, ok := i.in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return i.Line(ctx)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fd := int(f.Fd())
	saved, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	ch := make(chan inputLine, 1)
	go func() {
		secret, err := term.ReadPassword(fd)
		ch <- inputLine{strings.TrimSpace(string(secret)), err}
	}()
	select {
	case <-ctx.Done():
		// ReadPassword restores echo only when its read returns; this
		// process is about to exit, so restore it here.
		_ = term.Restore(fd, saved)
		return "", ctx.Err()
	case v := <-ch:
		return v.text, v.err
	}
}
