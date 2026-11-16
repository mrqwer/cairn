package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mrqwer/cairn/internal/cli"
)

func main() {
	os.Exit(run())
}

// run executes cairn with a context cancelled by SIGINT or SIGTERM, so
// commands stop between steps and undo partial work (adopt's rollback,
// apply's cleanup) instead of dying in the middle. After the first signal
// the default handling is restored: a second Ctrl-C quits at once (also
// while a prompt waits for input).
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	finished := make(chan struct{})
	go func() {
		<-ctx.Done()
		stop()
		select {
		case <-finished:
		default:
			// Only a command that can be in the middle of applying has a
			// step to finish; a read-only command or a prompt just ends.
			if !cli.MayBeMidApply() {
				return
			}
			fmt.Fprintln(os.Stderr, "cairn: interrupted; finishing the current step and undoing partial changes (press Ctrl-C again to quit at once)")
		}
	}()
	cmd := cli.NewRootCmd()
	cmd.SetOut(os.Stdout)
	err := cmd.ExecuteContext(ctx)
	close(finished)
	stop()
	if err != nil {
		var ee *cli.ExitError
		if errors.As(err, &ee) {
			return ee.Code
		}
		fmt.Fprintln(os.Stderr, "cairn:", err)
		return 1
	}
	return 0
}
