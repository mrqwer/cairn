package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/mrqwer/cairn/internal/cli"
)

func main() {
	cmd := cli.NewRootCmd()
	cmd.SetOut(os.Stdout)
	if err := cmd.Execute(); err != nil {
		var ee *cli.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.Code)
		}
		fmt.Fprintln(os.Stderr, "cairn:", err)
		os.Exit(1)
	}
}
