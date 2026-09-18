package meili

// Shared string literals reused across this package's test files, pulled
// out because a package-wide count (not a per-file one) is what a goconst
// check applies.
const (
	testUID      = "things"
	testStatus   = "status"
	testPrice    = "price"
	testPriceAsc = "price:asc"
	testCity     = "city"
	testSqft     = "sqft"
	testTypo     = "typo"
	testWords    = "words"
	testSF       = "san francisco"
	testKeep1    = "keep-1"
)
