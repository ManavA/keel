// Command gengallery renders site/gallery from the real minimal-example
// templates, so the gallery shows every block exactly as the service serves
// it: the same template source, the same stylesheets, in both themes.
//
// A block the generator has no sample data for stops the run instead of
// being skipped, which keeps "every block" true when blocks are added.
// The stylesheets are copied byte for byte; site_test.go fails if a copy
// drifts from the source it was taken from.
package main

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

var themes = []string{"landing", "dashboard"}

// blockDoc describes one block for the gallery index. A block without an
// entry here fails the run: an undocumented block is a skipped block.
var blockDocs = map[string]struct {
	purpose string
	sample  string
	height  int
}{
	"empty_state": {
		purpose: "The list's empty branch. note_list renders this when there are no notes to show.",
		sample:  "No data: the block takes none.",
		height:  140,
	},
	"flash": {
		purpose: "The one inline message. A form re-rendered with a 422 carries the reason here.",
		sample:  `Rendered with .Flash set to "Title is required."`,
		height:  120,
	},
	"note_card": {
		purpose: "One note in the list. Deleting goes through hx-delete, retargeting the card itself.",
		sample:  "Rendered with one stored note, title and body set.",
		height:  220,
	},
	"note_form": {
		purpose: "The create form. It posts through hx-post and prepends the new card to the list.",
		sample:  "Rendered empty, as the notes page serves it on first load.",
		height:  440,
	},
	"note_list": {
		purpose: "The list. Cards while there are notes, the empty state otherwise — never a second copy of either.",
		sample:  "Rendered with two notes, one without a body to show the card's short form.",
		height:  560,
	},
}

type sampleNote struct {
	ID        string
	Title     string
	Body      string
	CreatedAt time.Time
}

type sampleForm struct {
	Title string
	Body  string
	Flash string
}

type sampleList struct {
	Notes []sampleNote
}

func sampleNotes() []sampleNote {
	first := time.Date(2026, 9, 18, 14, 30, 0, 0, time.UTC)
	return []sampleNote{
		{
			ID:        "3f6d1c2a-9b4e-4f8a-a2c1-7e5d9b0f3a6c",
			Title:     "Roof repair quote",
			Body:      "Slate tiles, south side",
			CreatedAt: first,
		},
		{
			ID:        "7a1e5b9c-2d4f-4a8e-b3c7-1f6d8a0e4b2c",
			Title:     "Call the surveyor",
			CreatedAt: first.Add(26 * time.Hour),
		},
	}
}

func sampleData(block string) (any, error) {
	switch block {
	case "empty_state":
		return nil, nil
	case "flash":
		return map[string]string{"Flash": "Title is required."}, nil
	case "note_card":
		return sampleNotes()[0], nil
	case "note_form":
		return sampleForm{}, nil
	case "note_list":
		return sampleList{Notes: sampleNotes()}, nil
	default:
		return nil, fmt.Errorf("no sample data for block %q: add it to sampleData and blockDocs", block)
	}
}

const fragmentShell = `<!DOCTYPE html>
<html lang="en" data-theme="%s">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s in the %s theme — keel template gallery</title>
<link rel="stylesheet" href="../assets/tokens.css">
<link rel="stylesheet" href="../assets/layout.css">
</head>
<body>
<main class="dashboard">
%s
</main>
</body>
</html>
`

func repoRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot locate gengallery source")
	}
	// This file is site/gengallery/main.go; the root is two levels up.
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile))), nil
}

// copyFile copies src to dst. Both paths derive from the repo layout, never
// from input; the 0644 matches the files a checkout already carries, because
// generated site files are public by design.
func copyFile(dst, src string) error {
	raw, err := os.ReadFile(src) //nolint:gosec // G304: src is the repo's own stylesheet, not input
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o644) //nolint:gosec // G306: generated site files are public by design
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gengallery:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	siteDir := filepath.Join(root, "site")
	blocksDir := filepath.Join(root, "examples", "minimal", "templates", "blocks")
	cssDir := filepath.Join(root, "examples", "minimal", "static", "css")

	entries, err := os.ReadDir(blocksDir)
	if err != nil {
		return fmt.Errorf("read blocks: %w", err)
	}
	var blocks []string
	var blockFiles []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		blocks = append(blocks, strings.TrimSuffix(e.Name(), ".html"))
		blockFiles = append(blockFiles, filepath.Join(blocksDir, e.Name()))
	}
	sort.Strings(blocks)
	if len(blocks) == 0 {
		return fmt.Errorf("no blocks found in %s", blocksDir)
	}
	for _, b := range blocks {
		if _, ok := blockDocs[b]; !ok {
			return fmt.Errorf("block %q has no gallery entry: add it to blockDocs", b)
		}
	}

	tmpl, err := template.New("gallery").ParseFiles(blockFiles...)
	if err != nil {
		return fmt.Errorf("parse block templates: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(siteDir, "gallery"), 0o755); err != nil { //nolint:gosec // G301: generated site files are public by design
		return err
	}
	if err := os.MkdirAll(filepath.Join(siteDir, "assets"), 0o755); err != nil { //nolint:gosec // G301: generated site files are public by design
		return err
	}
	for _, name := range []string{"tokens.css", "layout.css"} {
		if err := copyFile(filepath.Join(siteDir, "assets", name), filepath.Join(cssDir, name)); err != nil {
			return fmt.Errorf("copy %s: %w", name, err)
		}
	}

	var index strings.Builder
	index.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Template gallery — keel showcase</title>
<link rel="stylesheet" href="assets/site.css">
</head>
<body>
<nav class="top"><a href="index.html">keel showcase</a> · <a href="tour.html">Tour</a> · <strong>Gallery</strong> · <a href="tutorial.html">Tutorial</a></nav>
<main>
<h1>Template gallery</h1>
<p>Every block the notes UI is built from, rendered from the real templates with the real
stylesheets, in both themes. The landing theme is the public face; the dashboard theme is the
signed-in workspace. Buttons and forms are inert here: the markup is identical, the service behind
it is not running.</p>
<p>Sources live at <code>examples/minimal/templates/blocks/</code>; the themes at
<code>examples/minimal/static/css/tokens.css</code>, where a page picks its theme with nothing but
<code>&lt;html data-theme="..."&gt;</code>.</p>
`)

	for _, b := range blocks {
		doc := blockDocs[b]
		data, err := sampleData(b)
		if err != nil {
			return err
		}
		fmt.Fprintf(&index, "<section id=\"%s\">\n<h2><code>%s</code></h2>\n<p>%s %s</p>\n", b, b, doc.purpose, doc.sample)
		for _, theme := range themes {
			var rendered strings.Builder
			if err := tmpl.ExecuteTemplate(&rendered, b, data); err != nil {
				return fmt.Errorf("render %s in %s: %w", b, theme, err)
			}
			page := fmt.Sprintf(fragmentShell, theme, b, theme, rendered.String())
			fragName := fmt.Sprintf("%s-%s.html", b, theme)
			if err := os.WriteFile(filepath.Join(siteDir, "gallery", fragName), []byte(page), 0o644); err != nil { //nolint:gosec // G306: generated site files are public by design
				return err
			}
			fmt.Fprintf(&index, "<h3>%s theme</h3>\n<iframe title=\"%s block in the %s theme\" src=\"gallery/%s\" style=\"height:%dpx\" loading=\"lazy\"></iframe>\n<p class=\"open\"><a href=\"gallery/%s\">Open standalone</a></p>\n",
				theme, b, theme, fragName, doc.height, fragName)
		}
		index.WriteString("</section>\n")
	}

	index.WriteString(`<p><a href="tutorial.html">Build this UI yourself in the tutorial →</a></p>
</main>
<footer>Rendered from the real templates by <code>go generate ./site</code>.</footer>
</body>
</html>
`)
	if err := os.WriteFile(filepath.Join(siteDir, "gallery.html"), []byte(index.String()), 0o644); err != nil { //nolint:gosec // G306: generated site files are public by design
		return err
	}
	fmt.Printf("gallery: %d blocks x %d themes\n", len(blocks), len(themes))
	return nil
}
