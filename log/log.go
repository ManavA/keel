// Package log builds the slog logger a service uses, with two behaviours that
// are hard to add afterwards.
//
// It refuses to print the value of an attribute whose key names a credential.
// Nobody logs a password on purpose; it arrives because a struct got dumped
// whole, or because a handler logged the request body of the one endpoint that
// takes a token. A rule in the handler catches those, and it catches them in
// code nobody thought to review.
//
// It carries the request id without being asked. slog hands every record its
// context, so the id put there by the request-id middleware ends up on every
// line a handler logs, including lines logged four packages deep by code that
// has never heard of HTTP.
//
// Options.Cloud maps slog's level onto the "severity" field Google Cloud
// Logging reads. Without it every entry lands at DEFAULT severity and alert
// policies that match on severity match nothing — quietly, because the logs
// themselves look fine.
package log

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// Options configures New. The zero value is a JSON logger on stdout at info
// level, with redaction on.
type Options struct {
	// Level defaults to slog.LevelInfo.
	Level slog.Leveler

	// Output defaults to os.Stdout. Logs belong on stdout: a container
	// platform collects both streams, and putting ordinary operation on stderr
	// makes every log line look like a problem to anything that separates them.
	Output io.Writer

	// Cloud maps the level onto a "severity" key for Google Cloud Logging.
	Cloud bool

	// AddSource records the file and line of the log call. Useful when a
	// message turns out to be ambiguous; not worth the cost by default.
	AddSource bool

	// SecretKeys are extra attribute keys to redact, beyond the ones that look
	// like credentials on their own. For the key your service happens to call
	// something else.
	SecretKeys []string

	// NoRedact turns redaction off. There is one honest reason to set it: a
	// test that asserts on the value of an attribute this package would
	// otherwise hide.
	NoRedact bool
}

// New returns a JSON logger.
func New(opts Options) *slog.Logger {
	out := opts.Output
	if out == nil {
		out = os.Stdout
	}
	level := opts.Level
	if level == nil {
		level = slog.LevelInfo
	}

	ho := &slog.HandlerOptions{
		Level:     level,
		AddSource: opts.AddSource,
	}
	ho.ReplaceAttr = replacer(opts)

	return slog.New(&contextHandler{Handler: slog.NewJSONHandler(out, ho)})
}

// replacer composes the severity mapping and the redaction rule into the single
// ReplaceAttr slot slog gives us.
func replacer(opts Options) func([]string, slog.Attr) slog.Attr {
	redact := Redact
	if len(opts.SecretKeys) > 0 {
		redact = Redactor(opts.SecretKeys...)
	}

	return func(groups []string, a slog.Attr) slog.Attr {
		if opts.Cloud {
			a = cloudSeverity(groups, a)
		}
		if !opts.NoRedact {
			a = redact(groups, a)
		}
		return a
	}
}

// cloudSeverity rewrites slog's "level" into the "severity" key Cloud Logging
// reads. WARNING, not slog's WARN: the spelling is the whole point.
func cloudSeverity(groups []string, a slog.Attr) slog.Attr {
	if a.Key != slog.LevelKey || len(groups) != 0 {
		return a
	}
	level, ok := a.Value.Any().(slog.Level)
	if !ok {
		return a
	}
	switch {
	case level >= slog.LevelError:
		return slog.String("severity", "ERROR")
	case level >= slog.LevelWarn:
		return slog.String("severity", "WARNING")
	case level >= slog.LevelInfo:
		return slog.String("severity", "INFO")
	default:
		return slog.String("severity", "DEBUG")
	}
}

// contextHandler puts the request id from the context onto every record.
//
// It wraps rather than replaces the JSON handler because the alternative — a
// middleware that calls logger.With(requestID) and stuffs the logger into the
// context — only works for code that remembers to pull the logger back out.
// Anything logging through slog.Default, which is most code, would lose the id.
type contextHandler struct{ slog.Handler }

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String(RequestIDKey, id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}
