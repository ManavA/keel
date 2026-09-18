// Package config loads a service's configuration from the environment.
//
// It is envconfig underneath, so the struct tags are envconfig's. This package
// adds four things: an optional dotenv step, a Validate call so a bad
// configuration fails at startup rather than at first use, whitespace trimming
// on secrets and URLs, and redaction helpers for logging what was loaded.
//
// The trimming is not cosmetic. A secret manager returns exactly the bytes it
// was given, and a value stored with `echo` carries a trailing newline; the
// authentication failure that follows reads as a wrong credential rather than a
// malformed one.
package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
)

// Validator is implemented by a configuration struct that can check itself.
// Load calls Validate after the environment has been read and secrets trimmed,
// and returns its error unchanged.
type Validator interface {
	Validate() error
}

// Options controls loading. The zero value reads the process environment,
// loads ".env" if it happens to exist, and takes no prefix.
type Options struct {
	// Prefix is envconfig's prefix: with "APP", a field tagged
	// envconfig:"PORT" reads APP_PORT.
	Prefix string

	// Files are dotenv files to load before reading the environment, in order.
	// A file that does not exist is not an error — in a deployed environment
	// there is no .env at all and the platform supplies real variables. Nil
	// means ".env"; an empty non-nil slice means load nothing.
	//
	// Values already present in the environment win. A dotenv file is a
	// convenience for a laptop, and it must never quietly override what a
	// deployment actually set.
	Files []string

	// SkipValidate stops Load calling Validate. For a partial load — a tool
	// that wants three fields out of a service's config struct and cannot
	// satisfy the rest.
	SkipValidate bool
}

// Load reads configuration into dest, which must be a non-nil pointer to a
// struct.
//
// An error from reading the environment is returned as text, not wrapped: the
// underlying envconfig error quotes the offending value, and a secret-shaped
// one is scrubbed out of the message before it is returned. Keeping %w would
// keep the unscrubbed text reachable through Unwrap. The cost is that
// envconfig's error type cannot be inspected with errors.As.
func Load(dest any) error { return LoadWith(dest, Options{}) }

// LoadWith is Load with explicit options.
func LoadWith(dest any, opts Options) error {
	if dest == nil {
		return errors.New("config: dest is nil")
	}
	v := reflect.ValueOf(dest)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("config: dest must be a non-nil pointer to a struct, got %T", dest)
	}

	files := opts.Files
	if files == nil {
		files = []string{".env"}
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		// godotenv.Load does not overwrite variables that are already set,
		// which is the behaviour we want and the reason this is Load rather
		// than Overload.
		if err := godotenv.Load(f); err != nil {
			return fmt.Errorf("config: load %s: %w", f, err)
		}
	}

	if err := envconfig.Process(opts.Prefix, dest); err != nil {
		// envconfig quotes the offending value in its message, which for a
		// non-string secret field ("converting 's3cr3t' to type int") puts the
		// credential in whatever the caller does with the error. The wrapping
		// is dropped along with it: the text is what leaks, so the text is what
		// must not be carried forward.
		return fmt.Errorf("config: read environment: %s", scrubSecrets(err.Error(), v.Elem(), opts.Prefix))
	}

	trimSecrets(v.Elem())

	if !opts.SkipValidate {
		if val, ok := dest.(Validator); ok {
			if err := val.Validate(); err != nil {
				return fmt.Errorf("config: invalid: %w", err)
			}
		}
	}
	return nil
}

// scrubSecrets replaces any secret-shaped environment value that appears in msg
// with the redaction placeholder.
//
// It works from the environment rather than from the struct because the struct
// field is unset when the parse failed: the value only exists in the variable
// the message quoted.
func scrubSecrets(msg string, v reflect.Value, prefix string) string {
	var values []string
	for _, key := range secretEnvKeys(v, prefix) {
		if value := os.Getenv(key); value != "" {
			values = append(values, value)
		}
	}

	// Longest first. A short secret that is a substring of a longer one would
	// otherwise consume it, and either replacement could land inside the
	// placeholder the other had already written.
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })

	for _, value := range values {
		// envconfig quotes the offending value, and the strconv error it carries
		// in "details:" quotes it again with double quotes. Both forms are exact,
		// so both are replaced whatever the value's length.
		msg = strings.ReplaceAll(msg, "'"+value+"'", "'"+Placeholder+"'")
		msg = strings.ReplaceAll(msg, `"`+value+`"`, `"`+Placeholder+`"`)

		// The bare form is replaced only when the value is long enough not to
		// appear in the message by accident. WEBHOOK_SECRET=t would otherwise
		// replace every "t" in the sentence, including those inside the
		// placeholder just inserted, destroying the error this function exists
		// to keep readable.
		//
		// The trade: a one- to five-character secret appearing somewhere other
		// than inside envconfig's quotes is not scrubbed. That is the right way
		// round — a secret that short is not protecting anything, and shredding
		// every message to chase it costs more than it saves.
		if len(value) >= minScrubbedLength {
			msg = strings.ReplaceAll(msg, value, Placeholder)
		}
	}
	return msg
}

// minScrubbedLength is the shortest value scrubbed outside envconfig's quotes.
const minScrubbedLength = 6

// secretEnvKeys lists the environment variables backing secret-shaped fields.
func secretEnvKeys(v reflect.Value, prefix string) []string {
	var keys []string
	t := v.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		fv := v.Field(i)
		if fv.Kind() == reflect.Pointer && !fv.IsNil() {
			fv = fv.Elem()
		}
		if fv.Kind() == reflect.Struct && fv.Type().PkgPath() != "time" {
			keys = append(keys, secretEnvKeys(fv, prefix)...)
			continue
		}
		if !isSecretField(field) {
			continue
		}
		name := field.Tag.Get("envconfig")
		if name == "" {
			name = strings.ToUpper(field.Name)
		}
		if prefix != "" {
			name = strings.ToUpper(prefix) + "_" + name
		}
		keys = append(keys, name, strings.ToUpper(name))
	}
	return keys
}

// trimSecrets strips surrounding whitespace from every string field that holds
// a secret or a URL, walking into nested structs and pointers to them.
//
// It does not trim every string: a field can hold a template, a separator or a
// banner whose leading space is intended.
func trimSecrets(v reflect.Value) {
	t := v.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		fv := v.Field(i)

		switch {
		case fv.Kind() == reflect.Pointer && !fv.IsNil() && fv.Elem().Kind() == reflect.Struct:
			trimSecrets(fv.Elem())
		case fv.Kind() == reflect.Struct:
			trimSecrets(fv)
		// Strings only: there is no whitespace to trim off an int or a
		// duration. Redacted covers the other kinds.
		case fv.Kind() == reflect.String && (isSecretField(field) || isURLField(field)):
			fv.SetString(strings.TrimSpace(fv.String()))
		}
	}
}

// isURLField reports whether a field holds a URL. URLs are trimmed alongside
// secrets: a trailing newline on DATABASE_URL produces a DNS lookup for a host
// name ending in "\n", which the driver reports as "no such host".
func isURLField(f reflect.StructField) bool {
	name := strings.ToUpper(f.Name)
	key := strings.ToUpper(f.Tag.Get("envconfig"))
	return strings.HasSuffix(name, "URL") || strings.HasSuffix(name, "URI") ||
		strings.HasSuffix(key, "URL") || strings.HasSuffix(key, "URI")
}

// isSecretField decides whether a struct field holds a credential.
//
// An explicit `secret:"true"` or `secret:"false"` tag wins. Without one, the
// field is judged by its name and its envconfig key, so a field does not have
// to be tagged to be protected.
func isSecretField(f reflect.StructField) bool {
	switch strings.ToLower(f.Tag.Get("secret")) {
	case "true":
		return true
	case "false":
		return false
	}
	return IsSecretName(f.Name) || IsSecretName(f.Tag.Get("envconfig"))
}

// IsSecretName reports whether a name — a field name, an environment variable —
// looks like it holds a credential. Case and separators are ignored, so
// SecretKey, SECRET_KEY and secret-key all match, and so does the camelCase
// spelling.
//
// The rule matches whole segments of the name rather than substrings, so
// api_key and client_secret are credentials while sort_key, cache_key and
// tokens_used are not. log.IsSecretKey applies the same rule to log attribute
// keys, over the same word lists; the two are separate so that neither
// bottom-layer package imports the other, and identical so a field and the log
// key named after it are treated alike. TestSecretNameAgreement in each package
// pins them to the same table.
func IsSecretName(name string) bool {
	return isSecretSegments(segments(name))
}
