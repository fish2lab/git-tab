// git-tab writes what the AI work behind a commit cost into the commit.
package main

import (
	"os"

	"github.com/fish2lab/git-tab/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args[1:])) }
