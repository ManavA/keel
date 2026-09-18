package config

import (
	"fmt"
	"net/url"
	"reflect"
)

// Placeholder is what a redacted value is replaced with.
const Placeholder = "[redacted]"

// Unset replaces a redacted empty value. It differs from Placeholder so that an
// unset secret can be told apart from one that is set and hidden.
const Unset = "[unset]"

// Redact replaces a secret value with a placeholder.
func Redact(value string) string {
	if value == "" {
		return Unset
	}
	return Placeholder
}

// RedactURL removes the credentials from a connection string and keeps the
// scheme, host, port and path.
//
// Use it rather than string surgery. Cutting at the first "@" and keeping the
// left-hand side keeps the password and discards the host, which is the wrong
// way round. A string that does not parse as a URL is redacted whole.
func RedactURL(raw string) string {
	if raw == "" {
		return Unset
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return Placeholder
	}
	if u.User != nil {
		// The username stays: it is not the secret, and it often identifies
		// which database this is.
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

// Redacted renders a configuration struct as fields safe to log at startup,
// which is the quickest way to confirm a deployment is reading the variables it
// was given.
//
// Secret fields — the same ones Load trims, by the same rule — are replaced.
// URL fields go through RedactURL, so the host survives and the password does
// not.
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
