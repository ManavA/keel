package log

import (
	"log/slog"
	"net/url"
	"strings"
	"unicode"
)

// Placeholder replaces a redacted value.
const Placeholder = "[redacted]"

// The four lists below decide whether a key names a credential. They are
// matched against whole segments of the key, not as substrings: substring
// matching redacts ordinary metrics, because "token" is inside tokens_used,
// "session" inside session_count and "cookie" inside cookie_consent. Those
// arrive as numbers and booleans, and replacing them with a string breaks
// whatever reads them.
//
// config.IsSecretName applies the same rule, over the same words, to struct
// fields and environment variables. The two are separate so that neither
// bottom-layer package imports the other, and identical so that a field and the
// log key named after it are treated the same way. Change one, change both:
// TestSecretNameAgreement in each package pins them to the same table.
var (
	// secretSegments make a key a credential wherever they appear.
	secretSegments = []string{
		"secret", "password", "passwd", "credential", "credentials",
		"authorization", "apikey", "privatekey",
	}

	// secretFinal make a key a credential when they end it: api_token and
	// client_secret are credentials, tokens_used is a count.
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

	// secretExact are keys that name a credential on their own but are ordinary
	// words in a compound: "cookie" is one, "cookie_consent" is not.
	secretExact = []string{
		"cookie", "session", "sessionid", "sid", "auth", "authorization",
		"jwt", "bearer", "token", "tokens", "credentials", "secrets",
	}
)

// Redact is a slog ReplaceAttr function that replaces the value of any
// attribute whose key names a credential.
//
// Pass it to your own slog.HandlerOptions if you are not using New:
//
//	slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: log.Redact})
//
// It matches on the key, so it catches slog.String("api_token", tok) wherever
// that was written. It cannot see inside a value that formats itself: a struct
// with a Password field logged with %+v arrives as one opaque string.
//
// A redacted value is always a string, whatever it was. Anything genuinely
// secret is not a number worth reading, and the segment matching above is what
// keeps numeric metrics out of this path.
func Redact(groups []string, a slog.Attr) slog.Attr {
	return redactAttr(a, nil)
}

// Redactor returns a Redact that also hides the keys you name. An extra key
// matches the whole normalized key, so "internal-ref" hides internal_ref and
// internalRef and nothing else.
func Redactor(extra ...string) func([]string, slog.Attr) slog.Attr {
	lowered := make([]string, len(extra))
	for i, e := range extra {
		lowered[i] = strings.Join(Segments(e), "")
	}
	return func(groups []string, a slog.Attr) slog.Attr {
		return redactAttr(a, lowered)
	}
}

func redactAttr(a slog.Attr, extra []string) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		return a
	}
	if IsSecretKey(a.Key) || containsString(extra, strings.Join(Segments(a.Key), "")) {
		return slog.String(a.Key, Placeholder)
	}
	// A URL attribute keeps its host and loses its userinfo, since connection
	// strings are logged often and the host is the useful part.
	if isURLKey(a.Key) && a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, redactURL(a.Value.String()))
	}
	return a
}

// IsSecretKey reports whether a log attribute key names a credential. Case and
// separators are ignored, so apiToken, api_token and API-TOKEN all match.
func IsSecretKey(key string) bool { return IsSecretSegments(Segments(key)) }

// IsSecretSegments applies the rule to an already-segmented name. config calls
// it through its own wrapper.
func IsSecretSegments(segs []string) bool {
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

// Segments splits a name into lowercase words, on separators and on camelCase
// boundaries. "APIToken" and "api_token" both give ["api", "token"].
func Segments(name string) []string {
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

func isURLKey(key string) bool {
	segs := Segments(key)
	if len(segs) == 0 {
		return false
	}
	switch segs[len(segs)-1] {
	case "url", "uri", "dsn":
		return true
	}
	return false
}

func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Not something we can take apart. An "@" might mean credentials, so
		// the value goes; a relative path or a bare host stays readable.
		if strings.Contains(raw, "@") {
			return Placeholder
		}
		return raw
	}
	if u.User == nil {
		return raw
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		u.User = url.UserPassword(u.User.Username(), Placeholder)
	}
	// url.String percent-encodes the userinfo, so the placeholder would appear
	// as %5Bredacted%5D and a search for the documented word would miss it.
	return strings.ReplaceAll(u.String(), url.PathEscape(Placeholder), Placeholder)
}
