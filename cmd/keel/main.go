// Command keel scaffolds projects built on keel.
package main

import (
	"fmt"
	"os"
	"strings"
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
	var b strings.Builder
	fmt.Fprint(&b, `keel scaffolds projects built on keel.

Usage:
	keel new <name> [-module <module-path>] [-profile <profile>]

	new creates <name>/ from one of the maintained examples.
	<module-path> sets the go.mod module; it defaults to <name>.
	-profile picks the template; the default is minimal.

Profiles:
`)
	for _, p := range profiles {
		fmt.Fprintf(&b, "\n\t%s\n\t\t%s.\n", p.name, p.blurb)
		fmt.Fprintf(&b, "\t\tpackages: %s\n", strings.Join(p.packages, ", "))
		fmt.Fprintf(&b, "\t\tmigrations: %s\n", strings.Join(p.migrations, "; "))
	}
	fmt.Fprint(os.Stderr, b.String())
}
