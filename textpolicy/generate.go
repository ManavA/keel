//go:build ignore

// Command generate reproduces defaultIgnorable in defaultignorable.go from
// Unicode's own data, pinned to a specific version rather than /UCD/latest/
// so a re-run years from now fetches the same input. Run it with:
//
//	go run generate.go
//
// It fetches every Default_Ignorable_Code_Point entry from
// DerivedCoreProperties.txt verbatim — no filtering by general category.
// An earlier version of this table kept only entries whose category was not
// Cf, on the theory that Cf is already covered by unicode.Cf at runtime; that
// silently dropped every entry categorized Cn (unassigned — reserved so a
// future character assigned there stays invisible by default), since an
// unassigned code point matches no category table at all, Cf included. The
// output here is deliberately redundant with unicode.Cf and unicode.IsControl
// where their ranges overlap: shouldStrip ORs this table together with both,
// so the only requirement on this table is that it be a correct superset of
// Default_Ignorable_Code_Point, not a minimal or disjoint one.
package main

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// unicodeVersion is the pinned DerivedCoreProperties.txt release this
// package's defaultIgnorable table was last generated from.
const unicodeVersion = "18.0.0"

var sourceURL = fmt.Sprintf("https://www.unicode.org/Public/%s/ucd/DerivedCoreProperties.txt", unicodeVersion)

var lineRE = regexp.MustCompile(`^([0-9A-Fa-f]{4,6})(?:\.\.([0-9A-Fa-f]{4,6}))?\s*;\s*Default_Ignorable_Code_Point\b`)

func main() {
	resp, err := http.Get(sourceURL) //nolint:gosec,noctx // a one-off developer tool, not shipped code
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	type codePoint struct{ lo, hi rune }
	var points []codePoint

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		m := lineRE.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		lo := parseHex(m[1])
		hi := lo
		if m[2] != "" {
			hi = parseHex(m[2])
		}
		points = append(points, codePoint{lo, hi})
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		os.Exit(1)
	}

	sort.Slice(points, func(i, j int) bool { return points[i].lo < points[j].lo })

	// Merge strictly adjacent ranges (previous.hi+1 == next.lo). This never
	// changes the set of code points covered, only how compactly it prints.
	merged := points[:0]
	for _, cp := range points {
		if n := len(merged); n > 0 && cp.lo == merged[n-1].hi+1 {
			merged[n-1].hi = cp.hi
			continue
		}
		merged = append(merged, cp)
	}

	for _, cp := range merged {
		if cp.hi > 0xFFFF {
			fmt.Printf("{Lo: 0x%05X, Hi: 0x%05X, Stride: 1}, // R32\n", cp.lo, cp.hi)
		} else {
			fmt.Printf("{Lo: 0x%04X, Hi: 0x%04X, Stride: 1}, // R16\n", cp.lo, cp.hi)
		}
	}
}

func parseHex(s string) rune {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 16, 32)
	if err != nil {
		panic(err)
	}
	return rune(v)
}
