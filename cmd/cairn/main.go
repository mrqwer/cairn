package main

import (
	"fmt"
	"os"

	"github.com/mrqwer/cairn/internal/cli"
)

func main() {
	cmd := cli.NewRootCmd()
	cmd.SetOut(os.Stdout)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "cairn:", err)
		os.Exit(1)
	}
}
