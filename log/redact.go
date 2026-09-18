package log

import (
	"log/slog"
	"net/url"
	"strings"
)

// Placeholder replaces a redacted value.
const Placeholder = "[redacted]"

// secretWords are the substrings that make an attribute key look like a
// credential. "key" on its own is not among them: cache keys, sort keys and
// idempotency keys are things you want to read, and a rule that hides all of
// them to catch api_key is a rule people turn off.
var secretWords = []string{
	"secret", "token", "password", "passwd", "credential",
	"apikey", "authorization", "privatekey", "cookie", "session",
}

// Redact is a slog ReplaceAttr function that replaces the value of any
// attribute whose key names a credential.
//
// Pass it to your own slog.HandlerOptions if you are not using New:
//
//	slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: log.Redact})
//
// It matches on the key, so it catches slog.String("api_token", tok) wherever
// it was written, but it cannot see inside a value that formats itself — a
// struct with a Password field logged with %+v arrives as one opaque string.
// Redaction is a safety net; it is not a reason to log the struct.
func Redact(groups []string, a slog.Attr) slog.Attr {
	return redactAttr(a, nil)
}

// Redactor returns a Redact that also hides the keys you name.
func Redactor(extra ...string) func([]string, slog.Attr) slog.Attr {
	lowered := make([]string, len(extra))
	for i, e := range extra {
		lowered[i] = normalizeKey(e)
	}
	return func(groups []string, a slog.Attr) slog.Attr {
		return redactAttr(a, lowered)
	}
}

func redactAttr(a slog.Attr, extra []string) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		return a
	}
	if IsSecretKey(a.Key) || matchesAny(normalizeKey(a.Key), extra) {
		return slog.String(a.Key, Placeholder)
	}
	// A URL attribute keeps its host and loses its userinfo. Connection strings
	// get logged constantly — "connecting to %s" is the most natural line in
	// the world to write — and the host is the part worth reading.
	//
	// config.RedactURL does the same job for a configuration dump. The two are
	// separate, rather than one calling the other, so that neither of these two
	// bottom-layer packages has to import the other.
	if isURLKey(a.Key) && a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, redactURL(a.Value.String()))
	}
	return a
}

// IsSecretKey reports whether a log attribute key names a credential. Case and
// separators are ignored: apiToken, api_token and API-TOKEN all match.
func IsSecretKey(key string) bool {
	return matchesAny(normalizeKey(key), secretWords)
}

func matchesAny(normalized string, words []string) bool {
	for _, w := range words {
		if w != "" && strings.Contains(normalized, w) {
			return true
		}
	}
	return false
}

func normalizeKey(key string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == '.' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToLower(key))
}

func isURLKey(key string) bool {
	k := normalizeKey(key)
	return strings.HasSuffix(k, "url") || strings.HasSuffix(k, "uri") || strings.HasSuffix(k, "dsn")
}

func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Not something we can take apart. Anything with an "@" in it might be
		// carrying credentials, so it goes; a relative path or a bare host does
		// not, and stays readable.
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
	return u.String()
}
