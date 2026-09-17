// iugu — the Platform 2 Console CLI for developers and their AI agents.
package main

import (
	"os"

	"github.com/iugu/cli/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:]))
}
