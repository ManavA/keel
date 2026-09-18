// Package config loads a service's configuration out of the environment and
// refuses to let two specific mistakes through.
//
// The first is a secret with a trailing newline. Secret managers hand back
// exactly the bytes they were given, and the bytes they were given usually came
// from `echo`, which appends one. A token with "\n" on the end authenticates
// against nothing, and the error the provider returns says "invalid
// credentials" — which sends you to look at the credential, not at the shell
// that stored it. Load trims any field it can tell holds a secret.
//
// The second is a configuration error found at first use rather than at
// startup. A struct may implement Validator; Load calls it, so a service that
// cannot possibly work exits immediately with a message naming the field,
// instead of serving traffic until the request that needs it arrives.
//
// Loading is envconfig underneath, so the struct tags are envconfig's. This
// package adds the dotenv step, the trimming, the validation call and the
// redaction helpers you need to log what you loaded.
package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
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
		return fmt.Errorf("config: read environment: %w", err)
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

// trimSecrets strips surrounding whitespace from every string field that holds
// a secret or a URL, walking into nested structs and pointers to them.
//
// It does not trim every string. A field can legitimately hold a template, a
// separator, or a banner whose leading space is deliberate, and silently
// reshaping those would be a worse bug than the one this prevents.
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
		case fv.Kind() == reflect.String && (isSecretField(field) || isURLField(field)):
			fv.SetString(strings.TrimSpace(fv.String()))
		}
	}
}

// isURLField reports whether a field holds a URL. They are trimmed alongside
// secrets because they arrive from the same places and fail the same way: a
// DATABASE_URL with a trailing newline produces a DNS lookup for a host name
// ending in "\n", and the driver reports that as "no such host" — which reads
// like a networking problem rather than a shell one. Whitespace around a URL is
// never deliberate.
func isURLField(f reflect.StructField) bool {
	name := strings.ToUpper(f.Name)
	key := strings.ToUpper(f.Tag.Get("envconfig"))
	return strings.HasSuffix(name, "URL") || strings.HasSuffix(name, "URI") ||
		strings.HasSuffix(key, "URL") || strings.HasSuffix(key, "URI")
}

// isSecretField decides whether a struct field holds a credential.
//
// An explicit `secret:"true"` or `secret:"false"` tag always wins. Without one,
// the field is judged by its name and its envconfig key, because the tag is the
// thing most likely to be forgotten on the field that most needs it — and a
// heuristic that is right about API_KEY without being told is worth more than a
// rule that is only ever right when somebody remembered.
func isSecretField(f reflect.StructField) bool {
	switch strings.ToLower(f.Tag.Get("secret")) {
	case "true":
		return true
	case "false":
		return false
	}
	return IsSecretName(f.Name) || IsSecretName(f.Tag.Get("envconfig"))
}

// secretWords are the substrings that make a name look like a credential.
var secretWords = []string{"secret", "token", "password", "passwd", "credential", "apikey", "api_key", "privatekey", "private_key"}

// IsSecretName reports whether a name — a field name, an environment variable,
// a log attribute key — looks like it holds a credential. Case and separators
// are ignored, so SecretKey, SECRET_KEY and secret-key all match.
//
// "key" alone is deliberately not on the list. Sort keys, cache keys, partition
// keys and idempotency keys are all things you want to read in a log line, and
// a rule that hides every one of them to catch API_KEY is a rule people turn
// off.
func IsSecretName(name string) bool {
	n := strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == '.' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToLower(name))
	for _, w := range secretWords {
		if strings.Contains(n, strings.ReplaceAll(w, "_", "")) {
			return true
		}
	}
	return false
}
