// Command repoctl builds signed RPM repositories from a package set.
package main

import (
	"os"

	"github.com/dennisklein/repoctl/internal/cli"
)

func main() { os.Exit(cli.Main()) }
