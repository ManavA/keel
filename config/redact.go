package config

import (
	"fmt"
	"net/url"
	"reflect"
)

// Placeholder is what a redacted value is replaced with.
const Placeholder = "[redacted]"

// Unset is what a redacted EMPTY value is replaced with. It is a different
// string from Placeholder on purpose: "the token is missing" and "the token is
// set and I am not showing it to you" are the two answers you actually want
// from a log line, and collapsing them into one leaves you unable to tell an
// unconfigured service from a misconfigured one.
const Unset = "[unset]"

// Redact replaces a secret value with a placeholder.
func Redact(value string) string {
	if value == "" {
		return Unset
	}
	return Placeholder
}

// RedactURL strips the credentials out of a connection string while keeping
// everything that makes it identifiable: scheme, host, port, path.
//
// Do not reach for string surgery instead. The shell version of this
// ("${URL%%@*}@***") cut at the first "@" and kept the part BEFORE it — which
// is the userinfo — so it published the password and hid the host, and it did
// so on every run of a migration job for weeks before anyone read the output
// closely enough to notice.
//
// A string that does not parse as a URL is redacted whole. Guessing at the
// shape of something unrecognised is how the shell version got it wrong.
func RedactURL(raw string) string {
	if raw == "" {
		return Unset
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return Placeholder
	}
	if u.User != nil {
		// The username stays. It is often the only clue to which database you
		// are actually pointed at, and it is not the secret.
		name := u.User.Username()
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(name, Placeholder)
		} else if name != "" {
			u.User = url.User(name)
		} else {
			u.User = nil
		}
	}
	u.RawQuery = redactQuery(u.RawQuery)
	return u.String()
}

// redactQuery masks the value of any query parameter whose name looks like a
// credential. Connection strings carry them: sslpassword, an access token on a
// signed URL.
func redactQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return Placeholder
	}
	for name, vs := range values {
		if !IsSecretName(name) {
			continue
		}
		for i := range vs {
			vs[i] = Placeholder
		}
	}
	return values.Encode()
}

// Field is one configuration value, ready to log.
type Field struct {
	Name   string
	Value  string
	Secret bool
}

// Redacted renders a configuration struct as fields safe to log at startup.
//
// Logging the configuration you actually loaded is the cheapest possible answer
// to "is this deployment even reading the variable I set", and services skip it
// because dumping a struct that contains a password is worse than dumping
// nothing. This makes the dump safe, so there is no reason left not to do it.
//
// Secret fields — the same ones Load trims, by the same rule — are replaced
// rather than printed. Anything whose name ends in URL is passed through
// RedactURL, because a connection string is both a secret and the single most
// useful line in a startup log.
func Redacted(cfg any) []Field {
	v := reflect.ValueOf(cfg)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	return redactedFields(v, "")
}

func redactedFields(v reflect.Value, prefix string) []Field {
	var out []Field
	t := v.Type()
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		fv := v.Field(i)
		name := prefix + sf.Name

		if fv.Kind() == reflect.Pointer && !fv.IsNil() {
			fv = fv.Elem()
		}
		if fv.Kind() == reflect.Struct && fv.Type().PkgPath() != "time" {
			out = append(out, redactedFields(fv, name+".")...)
			continue
		}

		secret := fv.Kind() == reflect.String && isSecretField(sf)
		switch {
		case secret:
			out = append(out, Field{Name: name, Value: Redact(fv.String()), Secret: true})
		case fv.Kind() == reflect.String && isURLField(sf):
			out = append(out, Field{Name: name, Value: RedactURL(fv.String()), Secret: true})
		default:
			out = append(out, Field{Name: name, Value: fmt.Sprint(fv.Interface())})
		}
	}
	return out
}
