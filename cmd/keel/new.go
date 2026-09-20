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

// profile is one keel new template: a maintained example, copied verbatim.
// cmd/new_test.go fails when a copy drifts from its example, so a change to
// an example either updates the template or says why not.
//
// packages and migrations are the contract the profile listing makes: which
// keel packages a project made from this profile wires, and which migrations
// it applies. usage prints them, so they are the first thing a team compares.
type profile struct {
	name       string
	example    string
	root       string
	blurb      string
	packages   []string
	migrations []string
	// bootEnvs are the extra environment variables the success message shows
	// beyond DATABASE_URL, because the profile refuses to boot without them.
	bootEnvs []string
}

var profiles = []profile{
	{
		name:    "minimal",
		example: "minimal",
		root:    "testdata/minimal",
		blurb:   "a Postgres-backed notes API with local auth, search and a background job",
		packages: []string{
			"app", "auth", "config", "events", "httpx", "jobs", "log", "mail", "pg", "search",
		},
		migrations: []string{
			"auth: 0001_auth_users through 0005_auth_login_attempts",
			"001_notes",
			"002_notes_owner",
		},
	},
	{
		name:    "standard",
		example: "fullstack",
		root:    "testdata/standard",
		blurb:   "the minimal API plus operator auth, an outbox relay and idempotent writes",
		packages: []string{
			"admin", "app", "auth", "config", "events", "httpx", "idempotency", "jobs", "log", "outbox", "pg",
		},
		migrations: []string{
			"auth: 0001_auth_users through 0005_auth_login_attempts",
			"admin: 0001_admin_users, 0002_admin_audit",
			"outbox: 001_outbox_events, 002_outbox_events_parked",
			"idempotency: 001_idempotency_keys",
			"001_fullstack_notes",
		},
		bootEnvs: []string{"ADMIN_SECRET"},
	},
}

// defaultProfile is the template keel new copies when no profile is named.
const defaultProfile = "minimal"

// templateFS holds every profile's example. Each subtree mirrors
// examples/<example> file for file; see profile and cmd/new_test.go.
//
//go:embed all:testdata/minimal all:testdata/standard
var templateFS embed.FS

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

func lookupProfile(name string) (profile, bool) {
	for _, p := range profiles {
		if p.name == name {
			return p, true
		}
	}
	return profile{}, false
}

func profileNames() string {
	names := make([]string, 0, len(profiles))
	for _, p := range profiles {
		names = append(names, p.name)
	}
	return strings.Join(names, ", ")
}

func runNew(argv []string) int {
	// The usage reads `keel new <name> [flags]`, but the flag package stops
	// at the first positional, so flags after the name would parse as names.
	// Hoisting them first keeps both orders working.
	argv = hoistFlags(argv)
	fset := flag.NewFlagSet("new", flag.ContinueOnError)
	module := fset.String("module", "", "module path for the new project (defaults to <name>)")
	profileName := fset.String("profile", defaultProfile, "template profile for the new project ("+profileNames()+")")
	fset.Usage = usage
	if err := fset.Parse(argv); err != nil {
		return 2
	}
	if fset.NArg() != 1 {
		fmt.Fprint(os.Stderr, "keel new takes exactly one name\n\n")
		usage()
		return 2
	}
	p, ok := lookupProfile(*profileName)
	if !ok {
		fmt.Fprintf(os.Stderr, "keel new: unknown profile %q (choose from %s)\n\n", *profileName, profileNames())
		usage()
		return 2
	}
	name := fset.Arg(0)
	if *module == "" {
		*module = filepath.Base(filepath.Clean(name))
	}
	if err := createProject(name, *module, p.name); err != nil {
		fmt.Fprintf(os.Stderr, "keel new: %v\n", err)
		return 1
	}
	env := "DATABASE_URL=postgres://keel:keel@127.0.0.1:5432/keel?sslmode=disable" //nolint:gosec // G101: documented example URL with placeholder credentials, printed as next-step guidance
	for _, name := range p.bootEnvs {
		env += " " + name + "=<secret>"
	}
	fmt.Printf(`created %s/ from keel's %s profile: %s.

next:
	cd %s
	go mod tidy
	%s go run .

until %s has a published release, point the require at a checkout instead:
	go mod edit -replace %s=<path-to-keel>
`, name, p.name, p.blurb, name, env, keelModule, keelModule)
	return 0
}

// createProject copies the named profile's template into dir and writes a
// go.mod naming module. dir must not already exist.
func createProject(dir, module, profileName string) error {
	p, ok := lookupProfile(profileName)
	if !ok {
		return fmt.Errorf("unknown profile %q (choose from %s)", profileName, profileNames())
	}
	if err := checkModulePath(module); err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s already exists", dir)
	} else if !os.IsNotExist(err) {
		return err
	}

	err := fs.WalkDir(templateFS, p.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Embed paths are slash-separated on every platform; CutPrefix keeps
		// this off filepath, whose separator differs on Windows.
		if path == p.root {
			return nil
		}
		rel, ok := strings.CutPrefix(path, p.root+"/")
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

// valueFlags are runNew's flags that take a separate argument, which
// hoistFlags needs to keep each flag with its value.
var valueFlags = map[string]bool{"module": true, "profile": true}

// hoistFlags moves flag arguments before positional ones, preserving order,
// so `keel new <name> -profile standard` parses like the usage reads. A "--"
// still ends flag parsing, with everything after it positional.
func hoistFlags(argv []string) []string {
	var flags, positional []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			positional = append(positional, argv[i+1:]...)
			break
		}
		trimmed := strings.TrimLeft(a, "-")
		if len(a) <= 1 || a[0] != '-' || trimmed == "" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if eq := strings.IndexByte(trimmed, '='); eq < 0 && valueFlags[trimmed] && i+1 < len(argv) {
			i++
			flags = append(flags, argv[i])
		}
	}
	return append(flags, positional...)
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
