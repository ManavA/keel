package search

// Shared string literals reused across this package's test files, pulled
// out because a package-wide count (not a per-file one) is what a goconst
// check applies.
const (
	testTable  = "documents"
	testField  = "price"
	testCity   = "city"
	testTitle  = "title"
	testKeep1  = "keep-1"
	testStale1 = "stale-1"
	testStale2 = "stale-2"
)
