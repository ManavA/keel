package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	keellog "github.com/ManavA/keel/log"
	"github.com/ManavA/keel/pg/testdb"
)

// A scaffold nobody boots stops working quietly. TestMain starts the shared
// database the boot test below needs; testdb fails the run if no test uses it,
// so a renamed skip cannot turn the acceptance check silent.
func TestMain(m *testing.M) {
	slog.SetDefault(keellog.New(keellog.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// templateMatchesExample walks one example and the embedded copy of it and
// compares them file by file, so the scaffold cannot silently fall behind the
// example it claims to copy. The example has no go.mod of its own — it lives
// in this module — which is why generation writes one rather than copying one.
func templateMatchesExample(t *testing.T, p profile) {
	t.Helper()

	example := filepath.Join("..", "..", "examples", p.example)

	var names []string
	err := filepath.WalkDir(example, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(example, path)
		if err != nil {
			return err
		}
		names = append(names, rel)

		want, err := os.ReadFile(path)
		require.NoError(t, err)
		got, err := templateFS.ReadFile(p.root + "/" + filepath.ToSlash(rel))
		require.NoError(t, err, "template is missing %s", rel)
		assert.Equal(t, string(want), string(got), "template copy of %s drifted", rel)
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, names, "example walk found nothing")

	// And nothing extra in the template that the example does not have.
	var extra []string
	err = fs.WalkDir(templateFS, p.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, ok := strings.CutPrefix(path, p.root+"/")
		require.True(t, ok, "unexpected template path %q", path)
		if _, err := os.Stat(filepath.Join(example, rel)); os.IsNotExist(err) {
			extra = append(extra, rel)
		}
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, extra, "template holds files the example does not")
}

func TestTemplateMatchesExample(t *testing.T) {
	for _, p := range profiles {
		t.Run(p.name, func(t *testing.T) {
			templateMatchesExample(t, p)
		})
	}
}

func TestGoDirectiveMatchesRepo(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	require.NoError(t, err)
	for line := range strings.Lines(string(raw)) {
		if v, ok := strings.CutPrefix(line, "go "); ok {
			assert.Equal(t, strings.TrimSpace(v), goDirective)
			return
		}
	}
	t.Fatal("no go directive in this repository's go.mod")
}

func TestLookupProfile(t *testing.T) {
	minimal, ok := lookupProfile("minimal")
	require.True(t, ok, "minimal profile must exist")
	assert.Equal(t, "minimal", minimal.example)

	standard, ok := lookupProfile("standard")
	require.True(t, ok, "standard profile must exist")
	assert.Equal(t, "fullstack", standard.example)

	assert.Equal(t, "minimal", defaultProfile, "minimal stays the default")

	for _, name := range []string{"", "bogus", "Minimal", "fullstack"} {
		_, ok := lookupProfile(name)
		assert.False(t, ok, "profile %q must not resolve", name)
	}
}

// The profile listing is the contract the issue asks for: each profile says
// which keel packages and which migrations a project made from it includes.
// The test pins the load-bearing entries, so a profile cannot silently stop
// mentioning the package it exists to teach.
func TestProfilesDescribePackagesAndMigrations(t *testing.T) {
	byName := map[string]profile{}
	for _, p := range profiles {
		require.NotEmpty(t, p.packages, "profile %q lists no packages", p.name)
		require.NotEmpty(t, p.migrations, "profile %q lists no migrations", p.name)
		byName[p.name] = p
	}

	minimal := byName["minimal"]
	for _, want := range []string{"app", "auth", "search", "mail", "jobs"} {
		assert.Contains(t, minimal.packages, want, "minimal must list %s", want)
	}
	assert.Contains(t, strings.Join(minimal.migrations, " "), "001_notes")

	standard := byName["standard"]
	for _, want := range []string{"app", "auth", "admin", "outbox", "idempotency"} {
		assert.Contains(t, standard.packages, want, "standard must list %s", want)
	}
	for _, want := range []string{"auth", "admin", "outbox", "idempotency", "001_fullstack_notes"} {
		assert.Contains(t, strings.Join(standard.migrations, " "), want, "standard must list %s", want)
	}
}

func TestNewGeneratesAMinimalProject(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "notes")
	require.NoError(t, createProject(dir, "example.com/notes", "minimal"))

	for _, name := range []string{
		"main.go", "config.go", "handlers.go", "notes.go", "main_test.go",
		"ui.go", "ui_notes.go", "ui_auth.go", "ui_test.go", "ui_auth_test.go",
		"README.md",
		filepath.Join("templates", "landing.html"),
		filepath.Join("templates", "notes.html"),
		filepath.Join("templates", "auth.html"),
		filepath.Join("templates", "app.html"),
		filepath.Join("static", "css", "tokens.css"),
		filepath.Join("static", "vendor", "VERSION"),
		filepath.Join("migrations", "001_notes.up.sql"),
		filepath.Join("migrations", "001_notes.down.sql"),
	} {
		assert.FileExists(t, filepath.Join(dir, name))
	}
	assert.NoFileExists(t, filepath.Join(dir, "admin.go"), "minimal must not carry the standard profile's admin service")

	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "module example.com/notes\n")
	assert.Contains(t, string(raw), "require "+keelModule+" "+keelVersion+"\n")
	assert.Contains(t, string(raw), "go "+goDirective+"\n")
}

func TestNewStandardProfileGeneratesFullstackShape(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "portal")
	require.NoError(t, createProject(dir, "example.com/portal", "standard"))

	for _, name := range []string{
		"main.go", "config.go", "handlers.go", "notes.go", "auth.go", "admin.go", "main_test.go",
		"README.md",
		filepath.Join("migrations", "001_fullstack_notes.up.sql"),
		filepath.Join("migrations", "001_fullstack_notes.down.sql"),
	} {
		assert.FileExists(t, filepath.Join(dir, name))
	}

	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "module example.com/portal\n")
	assert.Contains(t, string(raw), "require "+keelModule+" "+keelVersion+"\n")
}

func TestNewRejectsAnUnknownProfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "notes")
	require.ErrorContains(t, createProject(dir, "example.com/notes", "bogus"), `unknown profile "bogus"`)
	assert.NoDirExists(t, dir, "a rejected profile must not leave a directory behind")
}

func TestNewDefaultsTheModuleToTheName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "notes")
	require.NoError(t, createProject(dir, "notes", "minimal"))

	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "module notes\n")
}

func TestNewDefaultsTheProfileToMinimal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "notes")
	require.Equal(t, 0, run([]string{"new", dir}))
	assert.FileExists(t, filepath.Join(dir, "notes.go"))
	assert.NoFileExists(t, filepath.Join(dir, "admin.go"), "the default profile is minimal, not standard")
}

func TestHoistFlags(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   []string
		want []string
	}{
		{"flags after the name", []string{"portal", "-profile", "standard"}, []string{"-profile", "standard", "portal"}},
		{"flags before the name", []string{"-profile", "standard", "portal"}, []string{"-profile", "standard", "portal"}},
		{"no flags", []string{"portal"}, []string{"portal"}},
		{"empty", nil, nil},
		{"equals form", []string{"portal", "-profile=standard"}, []string{"-profile=standard", "portal"}},
		{"double dash ends flags", []string{"-profile", "--", "standard"}, []string{"-profile", "--", "standard"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hoistFlags(tt.in))
		})
	}
}

func TestRunNewWithStandardProfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "portal")
	require.Equal(t, 0, run([]string{"new", dir, "-profile", "standard"}))
	assert.FileExists(t, filepath.Join(dir, "admin.go"))
}

func TestRunNewRejectsAnUnknownProfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "portal")
	assert.Equal(t, 2, run([]string{"new", dir, "-profile", "bogus"}))
	assert.NoDirExists(t, dir, "a rejected profile must not leave a directory behind")
}

func TestNewRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	require.ErrorContains(t, createProject(dir, "example.com/x", "minimal"), "already exists")

	bad := filepath.Join(t.TempDir(), "x")
	require.Error(t, createProject(bad, "not a module", "minimal"))
	require.Error(t, createProject(bad, "", "minimal"))
	require.Error(t, createProject(bad, "/leading/slash", "minimal"))
	require.Error(t, createProject(bad, "trailing/slash/", "minimal"))
	require.Error(t, createProject(bad, "doubled//slash", "minimal"))
}

func TestCheckModulePath(t *testing.T) {
	for _, module := range []string{"notes", "example.com/notes", "a/b/c"} {
		assert.NoError(t, checkModulePath(module))
	}
}

func TestRunNewUsage(t *testing.T) {
	assert.Equal(t, 2, run([]string{}))
	assert.Equal(t, 2, run([]string{"bogus"}))
	assert.Equal(t, 2, run([]string{"new"}))
	assert.Equal(t, 0, run([]string{"help"}))
}

// TestStandardProfileBuildsAndBoots is the issue's acceptance check: a project
// made with the standard profile builds and boots against a real database.
// It points the generated module at this checkout with a replace directive,
// builds the binary, starts it against the shared test database, and walks
// the paths that prove the scaffold wired auth, admin and the notes API.
func TestStandardProfileBuildsAndBoots(t *testing.T) {
	db := testdb.Shared(t)

	dir := filepath.Join(t.TempDir(), "portal")
	require.NoError(t, createProject(dir, "example.com/portal", "standard"))

	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	goRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "go %s: %s", strings.Join(args, " "), out)
	}
	goRun("mod", "edit", "-replace", keelModule+"="+root)
	goRun("mod", "tidy")
	bin := filepath.Join(dir, "portal")
	goRun("build", "-o", bin, ".")

	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+db.URL,
		"ADMIN_SECRET=test-admin-secret-that-is-long-enough",
		"ADMIN_SEED_EMAIL=admin@example.com",
		"ADMIN_SEED_PASSWORD=adminpassword123",
		"PORT="+strconv.Itoa(port),
	)
	var serverLog bytes.Buffer
	cmd.Stdout = &serverLog
	cmd.Stderr = &serverLog
	require.NoError(t, cmd.Start())

	base := "http://127.0.0.1:" + strconv.Itoa(port)
	require.NoError(t, waitForStatus(t, base+"/readyz", http.StatusOK, time.Minute),
		"standard scaffold never became ready; server log:\n%s", &serverLog)

	body := getBody(t, base+"/readyz", "")
	assert.Contains(t, body, `"outbox":"ok"`)
	assert.Contains(t, body, `"idempotency":"ok"`)

	token := signupToken(t, base, "owner@example.com")
	status, created := postJSON(t, base+"/api/notes", token, `{"title":"Roof repair","body":"Slate tiles"}`)
	require.Equal(t, http.StatusCreated, status, "POST /api/notes: %s", created)
	assert.Contains(t, created, "Roof repair")

	status, adminSession := postJSON(t, base+"/admin/login", "",
		`{"email":"admin@example.com","password":"adminpassword123"}`)
	require.Equal(t, http.StatusOK, status, "POST /admin/login: %s", adminSession)
	assert.Contains(t, adminSession, "admin@example.com")
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// waitForStatus polls url until it answers want, so migrations and the first
// listen do not race the assertions.
func waitForStatus(t *testing.T, url string, want int, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp, err := http.Get(url) //nolint:gosec // G107: test polls its own server
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return nil
			}
			err = fmt.Errorf("GET %s: last status %d, wanted %d", url, resp.StatusCode, want)
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func getBody(t *testing.T, url, token string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s: %s", url, raw)
	return string(raw)
}

func signupToken(t *testing.T, base, email string) string {
	t.Helper()
	status, raw := postJSON(t, base+"/auth/signup", "",
		`{"email":"`+email+`","password":"password123"}`)
	require.Equal(t, http.StatusCreated, status, "POST /auth/signup: %s", raw)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &session))
	require.NotEmpty(t, session.Token)
	return session.Token
}

func postJSON(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return doRequest(t, req)
}

func doRequest(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}
