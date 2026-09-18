package config

import (
	"strings"
	"unicode"
)

// The four lists below decide whether a name holds a credential. They are
// matched against whole segments of the name, not as substrings: substring
// matching would hide sort_key and cache_key to catch api_key, and would report
// tokens_used as a secret.
//
// This is a copy of the rule in log/redact.go, over the same words. config and
// log both sit at the bottom layer and neither imports the other, so the rule
// is duplicated rather than shared. The lists must stay identical: a struct
// field and the log attribute named after it have to be treated the same way,
// and TestSecretNameAgreement in each package checks both against the same
// table of names.
var (
	// secretSegments make a name a credential wherever they appear.
	secretSegments = []string{
		"secret", "password", "passwd", "credential", "credentials",
		"authorization", "apikey", "privatekey",
	}

	// secretFinal make a name a credential when they end it: APIToken and
	// ClientSecret are credentials, TokensUsed is a count.
	secretFinal = []string{
		"token", "tokens", "secret", "secrets", "password", "passwords",
		"passwd", "credential", "credentials", "authorization", "cookie",
		"jwt", "bearer", "sid",
	}

	// secretPairs are two adjacent segments that name a credential only
	// together: api_key does, cache_key does not.
	secretPairs = []string{
		"apikey", "privatekey", "secretkey", "sessionid", "sessionkey",
		"accesstoken", "refreshtoken", "idtoken", "authtoken", "clientsecret",
	}

	// secretExact are names that hold a credential on their own but are
	// ordinary words in a compound: "cookie" is one, "cookie_consent" is not.
	secretExact = []string{
		"cookie", "session", "sessionid", "sid", "auth", "authorization",
		"jwt", "bearer", "token", "tokens", "credentials", "secrets",
	}
)

func isSecretSegments(segs []string) bool {
	if len(segs) == 0 {
		return false
	}
	if containsString(secretExact, strings.Join(segs, "")) {
		return true
	}
	if containsString(secretFinal, segs[len(segs)-1]) {
		return true
	}
	for _, s := range segs {
		if containsString(secretSegments, s) {
			return true
		}
	}
	for i := 0; i+1 < len(segs); i++ {
		if containsString(secretPairs, segs[i]+segs[i+1]) {
			return true
		}
	}
	return false
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// segments splits a name into lowercase words, on separators and on camelCase
// boundaries. "APIToken" and "API_TOKEN" both give ["api", "token"].
func segments(name string) []string {
	var (
		out     []string
		current []rune
	)
	flush := func() {
		if len(current) > 0 {
			out = append(out, strings.ToLower(string(current)))
			current = nil
		}
	}

	runes := []rune(name)
	for i, r := range runes {
		switch {
		case r == '_' || r == '-' || r == '.' || r == ' ' || r == '/' || r == ':':
			flush()
		case unicode.IsUpper(r):
			// A run of capitals stays together until the last of them, which
			// begins the next word: APIToken splits as API and Token.
			switch {
			case i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])):
				flush()
			case i > 0 && i+1 < len(runes) && unicode.IsUpper(runes[i-1]) && unicode.IsLower(runes[i+1]):
				flush()
			}
			current = append(current, r)
		default:
			current = append(current, r)
		}
	}
	flush()
	return out
}
