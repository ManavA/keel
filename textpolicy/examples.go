package textpolicy

import "regexp"

// ExamplePolicy is a placeholder for documentation and tests. Its rules are
// deliberately fake — nonsense tokens, not a real moderation list — so that
// nobody mistakes it for production-ready content and copies it into a
// deployment unexamined. A real caller builds its own Policy with its own
// domain's rules; this one exists to show the shape.
var ExamplePolicy = New(
	Rule{
		Name:    "example-placeholder-slur",
		Pattern: regexp.MustCompile(`(?i)\bbadword-placeholder\b`),
	},
	Rule{
		Name:    "example-placeholder-spam",
		Pattern: regexp.MustCompile(`(?i)\bfree-money-now\b`),
	},
	Rule{
		Name:    "example-placeholder-slogan",
		Pattern: regexp.MustCompile(`(?i)\bacme[\s-]+guaranteed[\s-]+winner\b`),
	},
)
