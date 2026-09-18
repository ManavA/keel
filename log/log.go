// Package log builds the slog logger a service uses.
//
// It adds two behaviours to slog's JSON handler that are awkward to add later.
// It replaces the value of any attribute whose key names a credential, which
// catches the cases nobody wrote on purpose: a struct dumped whole, a request
// body logged by the one endpoint that takes a token. And it reads the request
// id from the record's context, so a line logged deep in a call stack carries
// the id without every layer having to pass a logger down.
//
// Options.Cloud maps slog's level onto the "severity" field Google Cloud
// Logging reads. Without it every entry is DEFAULT severity and alert policies
// that match on severity match nothing.
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

	// Output defaults to os.Stdout. Container platforms collect both streams,
	// and tools that separate them treat stderr as a problem, so ordinary
	// operation belongs on stdout.
	Output io.Writer

	// Cloud maps the level onto a "severity" key for Google Cloud Logging.
	Cloud bool

	// AddSource records the file and line of the log call. Useful when a
	// message turns out to be ambiguous; not worth the cost by default.
	AddSource bool

	// SecretKeys are extra attribute keys to redact, for a credential your
	// service names something this package would not recognise.
	SecretKeys []string

	// NoRedact turns redaction off. Intended for a test that asserts on the
	// value of an attribute this package would otherwise hide.
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
// reads. The spelling matters: Cloud Logging wants WARNING, not slog's WARN.
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
// The alternative is a middleware that calls logger.With(requestID) and puts
// the logger in the context, which only reaches code that pulls the logger back
// out. Anything logging through slog.Default would lose the id.
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
