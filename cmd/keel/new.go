package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// templateFS is the minimal example, copied at the commit that added this
// command. cmd/new_test.go fails when the copy drifts from examples/minimal,
// so a change to the example either updates the template or says why not.
//
//go:embed all:testdata/minimal
var templateFS embed.FS

const templateRoot = "testdata/minimal"

// keelModule and keelVersion are the require line a new project starts with.
// The version names the release the project is written against; until that
// release exists, point the require at a checkout with a replace directive
// (the success message says how).
const (
	keelModule  = "github.com/ManavA/keel"
	keelVersion = "v0.1.0"
)

// goDirective is the go line a new project starts with. It must match this
// repository's own go.mod; cmd/new_test.go pins the two together.
const goDirective = "1.26.0"

func runNew(argv []string) int {
	fset := flag.NewFlagSet("new", flag.ContinueOnError)
	module := fset.String("module", "", "module path for the new project (defaults to <name>)")
	fset.Usage = usage
	if err := fset.Parse(argv); err != nil {
		return 2
	}
	if fset.NArg() != 1 {
		fmt.Fprint(os.Stderr, "keel new takes exactly one name\n\n")
		usage()
		return 2
	}
	name := fset.Arg(0)
	if *module == "" {
		*module = filepath.Base(filepath.Clean(name))
	}
	if err := createProject(name, *module); err != nil {
		fmt.Fprintf(os.Stderr, "keel new: %v\n", err)
		return 1
	}
	fmt.Printf(`created %s/ from keel's minimal example.

next:
	cd %s
	go mod tidy
	DATABASE_URL=postgres://keel:keel@127.0.0.1:5432/keel?sslmode=disable go run .

until %s has a published release, point the require at a checkout instead:
	go mod edit -replace %s=<path-to-keel>
`, name, name, keelModule, keelModule)
	return 0
}

// createProject copies the template into dir and writes a go.mod naming
// module. dir must not already exist.
func createProject(dir, module string) error {
	if err := checkModulePath(module); err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s already exists", dir)
	} else if !os.IsNotExist(err) {
		return err
	}

	err := fs.WalkDir(templateFS, templateRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Embed paths are slash-separated on every platform; CutPrefix keeps
		// this off filepath, whose separator differs on Windows.
		if path == templateRoot {
			return nil
		}
		rel, ok := strings.CutPrefix(path, templateRoot+"/")
		if !ok {
			return fmt.Errorf("keel new: unexpected template path %q", path)
		}
		// A go.mod in the template would carry a stale module line and a
		// stale require set; the file below is the only one.
		if rel == "go.mod" || rel == "go.sum" {
			return nil
		}
		target := filepath.Join(dir, rel)
		if d.IsDir() {
			return makeDir(target)
		}
		raw, err := templateFS.ReadFile(path)
		if err != nil {
			return err
		}
		if err := makeDir(filepath.Dir(target)); err != nil {
			return err
		}
		return writeFile(target, raw)
	})
	if err != nil {
		return err
	}

	gomod := "module " + module + "\n\ngo " + goDirective + "\n\nrequire " + keelModule + " " + keelVersion + "\n"
	return writeFile(filepath.Join(dir, "go.mod"), []byte(gomod))
}

// checkModulePath rejects what `go mod tidy` would: an empty module, an
// empty element (a leading, trailing or doubled slash), or whitespace.
func checkModulePath(module string) error {
	if strings.TrimSpace(module) == "" {
		return fmt.Errorf("module path is empty")
	}
	if strings.ContainsAny(module, " \t\n\r") {
		return fmt.Errorf("module path %q contains whitespace", module)
	}
	for _, element := range strings.Split(module, "/") {
		switch element {
		case "":
			return fmt.Errorf("module path %q has an empty element", module)
		case ".", "..":
			return fmt.Errorf("module path %q must not contain %q", module, element)
		}
	}
	return nil
}

// makeDir and writeFile use the conventional source permissions: a generated
// project is plain source, not a secret.
func makeDir(dir string) error {
	return os.MkdirAll(dir, 0o755) //nolint:gosec // G301: generated source is public by design
}

func writeFile(path string, raw []byte) error {
	return os.WriteFile(path, raw, 0o644) //nolint:gosec // G306: generated source is public by design
}
