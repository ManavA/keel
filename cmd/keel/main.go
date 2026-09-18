// Command keel scaffolds projects built on keel.
package main

import (
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	if len(argv) == 0 {
		usage()
		return 2
	}
	switch argv[0] {
	case "new":
		return runNew(argv[1:])
	case "-h", "-help", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "keel: unknown command %q\n\n", argv[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `keel scaffolds projects built on keel.

Usage:
	keel new <name> [-module <module-path>]

	new creates <name>/ from the minimal example: a Postgres-backed service
	with a small JSON API, migrations, health checks and a background job.
	<module-path> sets the go.mod module; it defaults to <name>.
`)
}
