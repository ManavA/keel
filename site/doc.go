// Package site holds the static showcase site served from GitHub Pages.
//
// The hand-written pages are index.html (overview), tour.html (package tour)
// and tutorial.html (getting started). gallery.html and gallery/ are rendered
// from the real minimal-example templates by gengallery, so the gallery can
// never show a block that no longer exists:
//
//go:generate go run ./gengallery
package site
