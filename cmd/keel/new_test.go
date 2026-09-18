package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// templateMatchesExample walks the example and the embedded copy and compares
// them file by file, so the scaffold cannot silently fall behind the example
// it claims to copy. The example has no go.mod of its own — it lives in this
// module — which is why generation writes one rather than copying one.
func TestTemplateMatchesExample(t *testing.T) {
	example := filepath.Join("..", "..", "examples", "minimal")

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
		got, err := templateFS.ReadFile(templateRoot + "/" + filepath.ToSlash(rel))
		require.NoError(t, err, "template is missing %s", rel)
		assert.Equal(t, string(want), string(got), "template copy of %s drifted", rel)
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, names, "example walk found nothing")

	// And nothing extra in the template that the example does not have.
	var extra []string
	err = fs.WalkDir(templateFS, templateRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, ok := strings.CutPrefix(path, templateRoot+"/")
		require.True(t, ok, "unexpected template path %q", path)
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(example, rel)); os.IsNotExist(err) {
			extra = append(extra, rel)
		}
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, extra, "template holds files the example does not")
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

func TestNewGeneratesAProject(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "notes")
	require.NoError(t, createProject(dir, "example.com/notes"))

	for _, name := range []string{
		"main.go", "config.go", "handlers.go", "notes.go", "main_test.go",
		"README.md",
		filepath.Join("migrations", "001_notes.up.sql"),
		filepath.Join("migrations", "001_notes.down.sql"),
	} {
		assert.FileExists(t, filepath.Join(dir, name))
	}

	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "module example.com/notes\n")
	assert.Contains(t, string(raw), "require "+keelModule+" "+keelVersion+"\n")
	assert.Contains(t, string(raw), "go "+goDirective+"\n")
}

func TestNewDefaultsTheModuleToTheName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "notes")
	require.NoError(t, createProject(dir, "notes"))

	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "module notes\n")
}

func TestNewRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	require.ErrorContains(t, createProject(dir, "example.com/x"), "already exists")

	bad := filepath.Join(t.TempDir(), "x")
	require.Error(t, createProject(bad, "not a module"))
	require.Error(t, createProject(bad, ""))
	require.Error(t, createProject(bad, "/leading/slash"))
	require.Error(t, createProject(bad, "trailing/slash/"))
	require.Error(t, createProject(bad, "doubled//slash"))
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
