// Command purlview is the Purlview command-line interface.
//
// This executable is a thin entry point: it wires the process streams and
// signals into the command package and exits with the code it returns.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/command"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := command.Run(ctx, os.Args[1:], command.Streams{
		In:  os.Stdin,
		Out: os.Stdout,
		Err: os.Stderr,
	}, buildinfo.Current())
	stop()
	os.Exit(code)
}
