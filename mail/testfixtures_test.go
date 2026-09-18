package mail

// Shared string literals reused across this package's test files, pulled
// out because a package-wide count (not a per-file one) is what a goconst
// check applies.
const (
	testServerToken        = "tok"
	testFromEmail          = "hello@example.com"
	testRecipient          = "a@b.com"
	testName               = "Jane"
	testNameKey            = "name"
	testBrand              = "Acme"
	testPasswordResetAlias = "password-reset"
)
