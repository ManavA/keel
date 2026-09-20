package site_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// blockNames lists every block template the gallery must show.
func blockNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "examples", "minimal", "templates", "blocks"))
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".html") {
			names = append(names, strings.TrimSuffix(e.Name(), ".html"))
		}
	}
	sort.Strings(names)
	require.NotEmpty(t, names, "expected block templates beside the gallery")
	return names
}

func TestGalleryCoversEveryBlock(t *testing.T) {
	gallery, err := os.ReadFile("gallery.html")
	require.NoError(t, err, "run go generate ./site before testing")
	for _, block := range blockNames(t) {
		for _, theme := range []string{"landing", "dashboard"} {
			frag := filepath.Join("gallery", block+"-"+theme+".html")
			rendered, err := os.ReadFile(frag)
			require.NoError(t, err, "gallery is missing %s", frag)
			require.Contains(t, string(rendered), `data-theme="`+theme+`"`)
			require.Contains(t, string(gallery), frag, "gallery.html does not embed %s", frag)
		}
		require.Contains(t, string(gallery), `id="`+block+`"`)
	}
}

func TestGalleryCSSMatchesSource(t *testing.T) {
	for _, name := range []string{"tokens.css", "layout.css"} {
		got, err := os.ReadFile(filepath.Join("assets", name))
		require.NoError(t, err)
		want, err := os.ReadFile(filepath.Join("..", "examples", "minimal", "static", "css", name))
		require.NoError(t, err)
		require.Equal(t, string(want), string(got), "%s drifted from the stylesheet the service ships", name)
	}
}

// TestTourQuotesKeptInSync guards the tour page against quoting an API the
// samples no longer use. The executable copies live in tour_test.go.
func TestTourQuotesKeptInSync(t *testing.T) {
	tour, err := os.ReadFile("tour.html")
	require.NoError(t, err)
	for _, marker := range []string{
		"retry.Do",
		"textpolicy.New",
		"flags.Evaluate",
		"jobs.Outcome",
		"httpx.JSON",
		"config.RedactURL",
	} {
		require.Contains(t, string(tour), marker, "tour.html no longer quotes %s", marker)
	}
}

var pageLink = regexp.MustCompile(`href="([a-z]+\.html)"`)

// TestSiteLinksResolve checks the internal links between the hand-written
// pages and the generated gallery.
func TestSiteLinksResolve(t *testing.T) {
	pages := []string{"index.html", "tour.html", "tutorial.html", "gallery.html"}
	for _, page := range pages {
		body, err := os.ReadFile(page)
		require.NoError(t, err)
		for _, m := range pageLink.FindAllSubmatch(body, -1) {
			target := string(m[1])
			require.FileExists(t, target, "%s links to missing %s", page, target)
		}
	}
}
