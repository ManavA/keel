package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lease_test.go is in package agent_test, because it uses agenttest, which
// imports this package. These three names are how it reaches what it tests.

// Keep is keep.
var Keep = keep

// ErrCancelRequested is errCancelRequested.
var ErrCancelRequested = errCancelRequested

// KeepOptions is keepOptions.
type KeepOptions = keepOptions

const (
	invokeRunID = "0d9c1f4e-7b2a-4e6d-8c35-1a2b3c4d5e6f"
	invokeInput = `{"id":7}`
	// invokeAmple is a timeout for a tool that returns at once. It is longer
	// than a test binary may run, so no such test turns on how busy the
	// machine is.
	invokeAmple = 24 * time.Hour
)

// invokeCall is the invocation every test here hands to invoke.
func invokeCall() Invocation {
	return Invocation{
		RunID: invokeRunID, Agent: "alpha", Seq: 4, Attempt: 2, Key: StepKey(invokeRunID, 4),
		Call: Call{ID: "call-1", Name: "lookup", Input: json.RawMessage(invokeInput)},
	}
}

func invokeTool(run ToolFunc) Tool {
	return Tool{Name: "lookup", Run: run}
}

// invokeLog collects what invoke logs to the logger it is given.
type invokeLog struct {
	mu      sync.Mutex
	entries []invokeEntry
}

type invokeEntry struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func (l *invokeLog) Enabled(context.Context, slog.Level) bool { return true }

func (l *invokeLog) Handle(_ context.Context, r slog.Record) error {
	entry := invokeEntry{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		entry.attrs[a.Key] = a.Value.String()
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
	return nil
}

func (l *invokeLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *invokeLog) WithGroup(string) slog.Handler      { return l }

func (l *invokeLog) logger() *slog.Logger { return slog.New(l) }

func (l *invokeLog) all() []invokeEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]invokeEntry(nil), l.entries...)
}

// invokeDefaultLog makes logs the default logger until the test ends.
func invokeDefaultLog(t *testing.T, logs *invokeLog) {
	t.Helper()
	// Setting the default logger also points the log package at it, and
	// setting it back does not undo that.
	previous, writer, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(logs.logger())
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
}

// invokeFlaky is an error of a type of its own that wraps ErrTransient.
type invokeFlaky struct{ op string }

func (e *invokeFlaky) Error() string { return e.op + ": try again" }
func (e *invokeFlaky) Unwrap() error { return ErrTransient }

func TestInvoke_ReturnsWhatTheToolReturned(t *testing.T) {
	errMissing := errors.New("no such record")
	cases := []struct {
		name string
		run  ToolFunc
		want outcome
	}{
		{
			name: "a result",
			run:  func(context.Context, Invocation) (string, error) { return "found", nil },
			want: outcome{result: "found"},
		},
		{
			name: "an empty result",
			run:  func(context.Context, Invocation) (string, error) { return "", nil },
			want: outcome{},
		},
		{
			name: "an error becomes an error result with its text",
			run:  func(context.Context, Invocation) (string, error) { return "", errMissing },
			want: outcome{result: "no such record", isError: true},
		},
		{
			name: "an error wins over a result returned with it",
			run:  func(context.Context, Invocation) (string, error) { return "half an answer", errMissing },
			want: outcome{result: "no such record", isError: true},
		},
		{
			name: "an error marked permanent is a result like any other",
			run: func(context.Context, Invocation) (string, error) {
				return "", fmt.Errorf("lookup: %w", ErrPermanent)
			},
			want: outcome{result: "lookup: agent: permanent failure", isError: true},
		},
		{
			name: "a context error of the tool's own making is a result like any other",
			run: func(context.Context, Invocation) (string, error) {
				return "", fmt.Errorf("lookup: upstream: %w", context.DeadlineExceeded)
			},
			want: outcome{result: "lookup: upstream: context deadline exceeded", isError: true},
		},
		{
			name: "an error with no text",
			run:  func(context.Context, Invocation) (string, error) { return "", errors.New("") },
			want: outcome{isError: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &invokeLog{}

			got := invoke(context.Background(), invokeTool(tc.run), invokeCall(), invokeAmple, logs.logger())

			assert.Equal(t, tc.want, got)
			assert.Empty(t, logs.all(), "only a panic is logged")
		})
	}
}

func TestInvoke_ATransientErrorAsksForAnotherTry(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"the sentinel itself", ErrTransient},
		{"wrapped once", fmt.Errorf("lookup: %w", ErrTransient)},
		{
			"wrapped several layers down",
			fmt.Errorf("lookup: %w", fmt.Errorf("fetch: %w", fmt.Errorf("dial: %w", fmt.Errorf("reset: %w", ErrTransient)))),
		},
		{"joined with another error", errors.Join(errors.New("closing the body"), fmt.Errorf("fetch: %w", ErrTransient))},
		{"behind a type of its own", &invokeFlaky{op: "fetch"}},
		{"behind a type of its own, wrapped", fmt.Errorf("lookup: %w", &invokeFlaky{op: "fetch"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := invokeTool(func(context.Context, Invocation) (string, error) {
				return "half an answer", tc.err
			})

			got := invoke(context.Background(), tool, invokeCall(), invokeAmple, nil)

			// The error comes back as the tool gave it, and nothing else:
			// there is no result to record.
			assert.Same(t, tc.err, got.retry)
			assert.Empty(t, got.result)
			assert.False(t, got.isError)
		})
	}
}

func TestInvoke_APanicIsRecoveredAndLogged(t *testing.T) {
	cases := []struct {
		name  string
		panic func(in Invocation)
		// logged is text the logged panic value must contain.
		logged string
	}{
		{"an error", func(Invocation) { panic(errors.New("index is corrupt")) }, "index is corrupt"},
		{"a string", func(Invocation) { panic("no rows") }, "no rows"},
		{"a number", func(Invocation) { panic(42) }, "42"},
		{"a struct", func(Invocation) { panic(struct{ code int }{code: 7}) }, "7"},
		{"nil", func(Invocation) {
			var nothing any
			panic(nothing)
		}, "nil"},
		{"a fault the runtime raises", func(in Invocation) {
			_ = in.Call.Input[len(in.Call.Input)+in.Seq]
		}, "index out of range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &invokeLog{}
			tool := invokeTool(func(_ context.Context, in Invocation) (string, error) {
				tc.panic(in)
				return "not reached", nil
			})

			got := invoke(context.Background(), tool, invokeCall(), invokeAmple, logs.logger())

			assert.Equal(t, outcome{result: "tool panicked", isError: true}, got)

			logged := logs.all()
			require.Len(t, logged, 1)
			entry := logged[0]
			assert.Equal(t, slog.LevelError, entry.level)
			assert.Equal(t, "lookup", entry.attrs["tool"])
			assert.Equal(t, invokeRunID, entry.attrs["run"])
			assert.Equal(t, "alpha", entry.attrs["agent"])
			assert.Equal(t, "4", entry.attrs["seq"])
			assert.Equal(t, "2", entry.attrs["attempt"])
			assert.Contains(t, entry.attrs["panic"], tc.logged)
			assert.Contains(t, entry.attrs["stack"], "invoke_test.go", "the stack reaches the tool's own frame")
			for key, value := range entry.attrs {
				assert.NotContains(t, value, invokeInput, "the call's arguments are not logged, in %q", key)
			}
		})
	}
}

// invokeBroken is an error whose own method panics, as a method on a nil
// pointer that reads a field does.
type invokeBroken struct{ text string }

func (e *invokeBroken) Error() string { return e.text }

func TestInvoke_APanicOutsideTheToolsOwnBodyIsStillTheTools(t *testing.T) {
	cases := []struct {
		name string
		tool Tool
	}{
		{
			name: "in the method that gives its error's text",
			tool: invokeTool(func(context.Context, Invocation) (string, error) {
				var broken *invokeBroken
				return "", broken
			}),
		},
		{
			name: "in the method that unwraps its error",
			tool: invokeTool(func(context.Context, Invocation) (string, error) {
				return "", invokeUnwrapPanics{}
			}),
		},
		{
			// Only a tool with Run is ever invoked. One without is a fault in
			// the caller, and is reported as one in the tool rather than
			// ending the process.
			name: "in calling a tool that has no Run",
			tool: Tool{Name: "lookup", Delegate: "researcher"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &invokeLog{}

			got := invoke(context.Background(), tc.tool, invokeCall(), invokeAmple, logs.logger())

			assert.Equal(t, outcome{result: "tool panicked", isError: true}, got)
			assert.Len(t, logs.all(), 1)
		})
	}
}

// invokeUnwrapPanics is an error that panics when asked what it wraps.
type invokeUnwrapPanics struct{}

func (invokeUnwrapPanics) Error() string { return "unwrap panics" }
func (invokeUnwrapPanics) Unwrap() error { panic("unwrap panics") }

func TestInvoke_AToolThatExitsItsGoroutineIsTreatedAsAPanic(t *testing.T) {
	logs := &invokeLog{}
	tool := invokeTool(func(context.Context, Invocation) (string, error) {
		runtime.Goexit()
		return "not reached", nil
	})

	got := invoke(context.Background(), tool, invokeCall(), invokeAmple, logs.logger())

	assert.Equal(t, outcome{result: "tool panicked", isError: true}, got)
	logged := logs.all()
	require.Len(t, logged, 1)
	assert.Equal(t, slog.LevelError, logged[0].level)
	assert.Equal(t, "lookup", logged[0].attrs["tool"])
	assert.Contains(t, logged[0].attrs["panic"], "without returning", "the log says it was not a panic")
}

func TestInvoke_AToolPastItsTimeoutIsTimedOut(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		// run builds the tool. release is closed when the test is over, for a
		// tool that waits on nothing else.
		run  func(release <-chan struct{}) ToolFunc
		want string
	}{
		{
			name:    "a tool that ends with its context and returns the context's error",
			timeout: 2 * time.Minute,
			run: func(<-chan struct{}) ToolFunc {
				return func(ctx context.Context, _ Invocation) (string, error) {
					<-ctx.Done()
					return "", ctx.Err()
				}
			},
			want: "timed out after 2m0s",
		},
		{
			name:    "a tool that ends with its context and returns a result",
			timeout: 2 * time.Minute,
			run: func(<-chan struct{}) ToolFunc {
				return func(ctx context.Context, _ Invocation) (string, error) {
					<-ctx.Done()
					return "too late", nil
				}
			},
			want: "timed out after 2m0s",
		},
		{
			name:    "a tool that ends with its context and asks for another try",
			timeout: 2 * time.Minute,
			run: func(<-chan struct{}) ToolFunc {
				return func(ctx context.Context, _ Invocation) (string, error) {
					<-ctx.Done()
					return "", fmt.Errorf("fetch: %w: %w", ErrTransient, ctx.Err())
				}
			},
			want: "timed out after 2m0s",
		},
		{
			name:    "a tool that ignores its context and does not return",
			timeout: 2 * time.Minute,
			run: func(release <-chan struct{}) ToolFunc {
				return func(context.Context, Invocation) (string, error) {
					<-release
					return "much too late", nil
				}
			},
			want: "timed out after 2m0s",
		},
		{
			name:    "a timeout with a fraction",
			timeout: 1500 * time.Millisecond,
			run: func(release <-chan struct{}) ToolFunc {
				return func(context.Context, Invocation) (string, error) {
					<-release
					return "", nil
				}
			},
			want: "timed out after 1.5s",
		},
		{
			name:    "a timeout of hours",
			timeout: 90 * time.Minute,
			run: func(release <-chan struct{}) ToolFunc {
				return func(context.Context, Invocation) (string, error) {
					<-release
					return "", nil
				}
			},
			want: "timed out after 1h30m0s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &invokeLog{}
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				defer close(release)
				// The tool's own Timeout is not what bounds it: the caller
				// works the bound out and hands it over.
				tool := Tool{Name: "lookup", Timeout: time.Nanosecond, Run: tc.run(release)}
				start := time.Now()

				got := invoke(context.Background(), tool, invokeCall(), tc.timeout, logs.logger())

				assert.Equal(t, outcome{result: tc.want, isError: true}, got)
				assert.Equal(t, tc.timeout, time.Since(start), "invoke returns when the time is up, and not before")
			})
			assert.Empty(t, logs.all())
		})
	}
}

func TestInvoke_AToolThatFinishesJustInsideItsTimeoutIsNotTimedOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tool := invokeTool(func(context.Context, Invocation) (string, error) {
			time.Sleep(2*time.Minute - time.Nanosecond)
			return "just in time", nil
		})

		got := invoke(context.Background(), tool, invokeCall(), 2*time.Minute, nil)

		assert.Equal(t, outcome{result: "just in time"}, got)
	})
}

func TestInvoke_ATimeoutOfZeroOrLessDoesNotRunTheTool(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{"zero", 0, "timed out after 0s"},
		{"negative", -time.Second, "timed out after -1s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ran := false
				// The tool's own Timeout does not stand in for the one given.
				tool := Tool{Name: "lookup", Timeout: time.Hour, Run: func(context.Context, Invocation) (string, error) {
					ran = true
					return "ran", nil
				}}

				got := invoke(context.Background(), tool, invokeCall(), tc.timeout, nil)

				assert.Equal(t, outcome{result: tc.want, isError: true}, got)
				// Nothing is left running that could still call the tool.
				synctest.Wait()
				assert.False(t, ran)
			})
		})
	}
}

func TestInvoke_AToolLeftBehindEndsCleanly(t *testing.T) {
	cases := []struct {
		name string
		// after is what the tool does once the test lets it go on, long
		// after invoke has returned.
		after  func() (string, error)
		logged int
	}{
		{"it returns a result nobody reads", func() (string, error) { return "much too late", nil }, 0},
		{"it returns an error nobody reads", func() (string, error) { return "", errors.New("much too late") }, 0},
		{"it panics", func() (string, error) { panic("much too late") }, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &invokeLog{}
			// The bubble fails if the tool's goroutine is still blocked when
			// the test ends, as it would be were it waiting to hand over a
			// result nobody will take.
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				tool := invokeTool(func(context.Context, Invocation) (string, error) {
					<-release
					return tc.after()
				})

				got := invoke(context.Background(), tool, invokeCall(), time.Minute, logs.logger())
				require.Equal(t, outcome{result: "timed out after 1m0s", isError: true}, got)
				require.Empty(t, logs.all())

				close(release)
				synctest.Wait()

				// A panic in a goroutine nothing waits for would end the
				// process. It is recovered and logged like any other.
				logged := logs.all()
				require.Len(t, logged, tc.logged)
				if tc.logged > 0 {
					assert.Equal(t, slog.LevelError, logged[0].level)
					assert.Contains(t, logged[0].attrs["panic"], "much too late")
				}
			})
		})
	}
}

func TestInvoke_ResultsAreBoundedAtOneMebibyte(t *testing.T) {
	const mebibyte = 1 << 20
	exactly := strings.Repeat("x", mebibyte)
	// Two bytes to the character: the bound is on bytes.
	wide := strings.Repeat("é", mebibyte/2)

	cases := []struct {
		name   string
		result string
		err    error
		// want is the result expected, when it is not the tool's own.
		want    string
		isError bool
	}{
		{name: "exactly a mebibyte is kept", result: exactly},
		{name: "one byte over is refused with its size", result: exactly + "x", want: "result too large: 1048577 bytes", isError: true},
		{name: "exactly a mebibyte of wider characters is kept", result: wide},
		{name: "one wider character over is refused with its size in bytes", result: wide + "é", want: "result too large: 1048578 bytes", isError: true},
		{name: "exactly a mebibyte ending in a three-byte character is kept", result: strings.Repeat("x", mebibyte-3) + "€"},
		{name: "one byte over, ending in a three-byte character, is refused", result: strings.Repeat("x", mebibyte-2) + "€", want: "result too large: 1048577 bytes", isError: true},
		{name: "exactly a mebibyte ending in a four-byte character is kept", result: strings.Repeat("x", mebibyte-4) + "🙂"},
		{name: "an error whose text is exactly a mebibyte is kept", err: errors.New(exactly), want: exactly, isError: true},
		{name: "an error whose text is one byte over is refused the same way", err: errors.New(exactly + "x"), want: "result too large: 1048577 bytes", isError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := invokeTool(func(context.Context, Invocation) (string, error) { return tc.result, tc.err })
			want := tc.want
			if want == "" {
				want = tc.result
			}

			got := invoke(context.Background(), tool, invokeCall(), invokeAmple, nil)

			// Compared without assert.Equal, which would print a mebibyte.
			assert.Len(t, got.result, len(want))
			assert.True(t, got.result == want, "the result is not the one expected")
			assert.Equal(t, tc.isError, got.isError)
			assert.NoError(t, got.retry)
		})
	}
}

func TestInvoke_AResultTheJournalCannotHoldIsRefused(t *testing.T) {
	const mebibyte = 1 << 20
	nul := outcome{result: "result contains a NUL byte", isError: true}
	invalid := outcome{result: "result is not valid UTF-8", isError: true}

	cases := []struct {
		name   string
		result string
		want   outcome
	}{
		{"a NUL byte in the middle", "before\x00after", nul},
		{"a NUL byte at the end", "found\x00", nul},
		{"a NUL byte and nothing else", "\x00", nul},
		{"bytes that are not UTF-8 at the end", "found\xff\xfe", invalid},
		{"a character cut short at the end", "caf" + "é"[:1], invalid},
		{"a stray continuation byte in the middle", "be\x80fore", invalid},
		{"both, which is reported as not valid UTF-8", "be\x00fore\xff", invalid},
		{
			"over a mebibyte and not valid UTF-8, which is reported as too large",
			strings.Repeat("x", mebibyte) + "\xff",
			outcome{result: "result too large: 1048577 bytes", isError: true},
		},
		{
			"over a mebibyte with a NUL byte, which is reported as too large",
			strings.Repeat("x", mebibyte) + "\x00",
			outcome{result: "result too large: 1048577 bytes", isError: true},
		},
		{"characters of several bytes are text the journal holds", "naïve 日本語 🙂", outcome{result: "naïve 日本語 🙂"}},
		{"the replacement character is text the journal holds", "found \uFFFD", outcome{result: "found \uFFFD"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := invokeTool(func(context.Context, Invocation) (string, error) { return tc.result, nil })

			got := invoke(context.Background(), tool, invokeCall(), invokeAmple, nil)

			assert.Equal(t, tc.want, got)
		})
	}
}

func TestInvoke_AnErrorsTextIsMadeFitForTheJournal(t *testing.T) {
	const mebibyte = 1 << 20

	cases := []struct {
		name string
		text string
		want string
	}{
		{"a NUL byte is removed", "no such\x00 record", "no such record"},
		{"bytes that are not UTF-8 are replaced", "no such record\xff\xfe", "no such record\uFFFD"},
		{"both at once", "bad\x00 byte \xff in \x00\xc3 it", "bad byte \uFFFD in \uFFFD it"},
		{
			// Taking the NUL out first would join the two halves into a
			// character the tool never wrote.
			"a NUL byte between the halves of a character leaves no character behind",
			"caf\xc3\x00\xa9",
			"caf\uFFFD\uFFFD",
		},
		{"nothing but bytes the journal cannot hold", "\x00\xff\x00", "\uFFFD"},
		{"text the journal can hold is left alone", "naïve 日本語 🙂 \uFFFD", "naïve 日本語 🙂 \uFFFD"},
		{
			"the bound is on the text as it would be stored: over it once replaced",
			strings.Repeat("\xffa", mebibyte/2),
			"result too large: 2097152 bytes",
		},
		{
			"the bound is on the text as it would be stored: within it once a NUL is gone",
			strings.Repeat("x", mebibyte) + "\x00",
			strings.Repeat("x", mebibyte),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := invokeTool(func(context.Context, Invocation) (string, error) { return "", errors.New(tc.text) })

			got := invoke(context.Background(), tool, invokeCall(), invokeAmple, nil)

			// Compared without assert.Equal, which could print a mebibyte.
			assert.Len(t, got.result, len(tc.want))
			assert.True(t, got.result == tc.want, "the result is not the one expected")
			assert.True(t, got.isError)
			assert.NoError(t, got.retry)
		})
	}
}

func TestInvoke_APanicValueTheJournalCannotHoldStaysOutOfIt(t *testing.T) {
	logs := &invokeLog{}
	tool := invokeTool(func(context.Context, Invocation) (string, error) {
		panic("index \x00is corrupt\xff")
	})

	got := invoke(context.Background(), tool, invokeCall(), invokeAmple, logs.logger())

	// The result of a panic is the fixed text. The value goes to the log
	// only, as the tool raised it.
	assert.Equal(t, outcome{result: "tool panicked", isError: true}, got)
	logged := logs.all()
	require.Len(t, logged, 1)
	assert.Equal(t, "index \x00is corrupt\xff", logged[0].attrs["panic"])
}

func TestInvoke_LogsToTheLoggerItIsGiven(t *testing.T) {
	cases := []struct {
		name string
		// given is whether invoke is handed a logger of its own.
		given bool
	}{
		{"the one it is given, and not the default", true},
		{"the default when it is given none", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fallback, own := &invokeLog{}, &invokeLog{}
			invokeDefaultLog(t, fallback)
			var logger *slog.Logger
			if tc.given {
				logger = own.logger()
			}
			tool := invokeTool(func(context.Context, Invocation) (string, error) { panic("no rows") })

			got := invoke(context.Background(), tool, invokeCall(), invokeAmple, logger)

			require.Equal(t, outcome{result: "tool panicked", isError: true}, got)
			if tc.given {
				assert.Len(t, own.all(), 1)
				assert.Empty(t, fallback.all())
			} else {
				assert.Len(t, fallback.all(), 1)
			}
		})
	}
}

func TestInvoke_TheToolReceivesTheInvocationAndABoundedContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type key struct{}
		ctx := context.WithValue(context.Background(), key{}, "carried")

		var (
			received Invocation
			seen     context.Context
			deadline time.Time
			bounded  bool
			live     error
		)
		tool := Tool{Name: "lookup", Timeout: time.Hour, Run: func(ctx context.Context, in Invocation) (string, error) {
			received, seen = in, ctx
			deadline, bounded = ctx.Deadline()
			live = ctx.Err()
			return "found", nil
		}}
		start := time.Now()

		got := invoke(ctx, tool, invokeCall(), 90*time.Second, nil)

		require.Equal(t, outcome{result: "found"}, got)
		assert.Equal(t, invokeCall(), received)
		assert.Equal(t, "carried", seen.Value(key{}), "the tool's context is the caller's")
		require.True(t, bounded)
		assert.Equal(t, start.Add(90*time.Second), deadline, "bounded by the timeout given, not the tool's own")
		assert.NoError(t, live, "live while the tool runs")
		assert.ErrorIs(t, seen.Err(), context.Canceled, "and ended once invoke returns")
	})
}

func TestInvoke_ACallersContextThatEndsLeavesNothingToRecord(t *testing.T) {
	errTaken := fmt.Errorf("execution: %w", ErrLeaseLost)

	cases := []struct {
		name string
		// run builds the tool. end ends the caller's context, and release is
		// closed when the test is over.
		run func(end func(), release <-chan struct{}) ToolFunc
		// logged is how many entries the log holds afterwards.
		logged int
	}{
		{
			name: "while a tool that ignores it runs on",
			run: func(end func(), release <-chan struct{}) ToolFunc {
				return func(context.Context, Invocation) (string, error) {
					end()
					<-release
					return "much too late", nil
				}
			},
		},
		{
			name: "and the tool returns the context's error",
			run: func(end func(), _ <-chan struct{}) ToolFunc {
				return func(ctx context.Context, _ Invocation) (string, error) {
					end()
					<-ctx.Done()
					return "", ctx.Err()
				}
			},
		},
		{
			name: "and the tool returns a result all the same",
			run: func(end func(), _ <-chan struct{}) ToolFunc {
				return func(context.Context, Invocation) (string, error) {
					end()
					return "sent", nil
				}
			},
		},
		{
			name: "and the tool returns an error of its own",
			run: func(end func(), _ <-chan struct{}) ToolFunc {
				return func(context.Context, Invocation) (string, error) {
					end()
					return "", errors.New("no such record")
				}
			},
		},
		{
			name: "and the tool panics",
			run: func(end func(), _ <-chan struct{}) ToolFunc {
				return func(context.Context, Invocation) (string, error) {
					end()
					panic("gave up")
				}
			},
			logged: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &invokeLog{}
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				defer close(release)
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				tool := invokeTool(tc.run(func() { cancel(errTaken) }, release))

				got := invoke(ctx, tool, invokeCall(), invokeAmple, logs.logger())

				// Whatever the tool did, the caller is told why its own
				// context ended, and is given no result it might record.
				assert.Same(t, errTaken, got.retry)
				assert.Empty(t, got.result)
				assert.False(t, got.isError)

				synctest.Wait()
				assert.Len(t, logs.all(), tc.logged)
			})
		})
	}
}

func TestInvoke_ACallersContextThatHasEndedDoesNotRunTheTool(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		want  error
	}{
		{"with a cause", ErrLeaseLost, ErrLeaseLost},
		{"with none of its own", nil, context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ran := false
				tool := invokeTool(func(context.Context, Invocation) (string, error) {
					ran = true
					return "ran", nil
				})
				ctx, cancel := context.WithCancelCause(context.Background())
				cancel(tc.cause)

				got := invoke(ctx, tool, invokeCall(), invokeAmple, nil)

				assert.Same(t, tc.want, got.retry)
				assert.Empty(t, got.result)
				assert.False(t, got.isError)
				// Nothing is left running that could still call the tool.
				synctest.Wait()
				assert.False(t, ran)
			})
		})
	}
}

func TestInvoke_ACallersDeadlineIsNotTheToolsTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		tool := invokeTool(func(ctx context.Context, _ Invocation) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		})
		start := time.Now()

		got := invoke(ctx, tool, invokeCall(), 2*time.Minute, nil)

		// The fixed text is for the timeout invoke was given. A context that
		// ran out first is the caller's business.
		assert.Equal(t, context.DeadlineExceeded, got.retry)
		assert.Empty(t, got.result)
		assert.False(t, got.isError)
		assert.Equal(t, time.Minute, time.Since(start))
	})
}

func TestInvoke_IsSafeForManyCallsAtOnce(t *testing.T) {
	tool := invokeTool(func(_ context.Context, in Invocation) (string, error) {
		return fmt.Sprintf("found %d", in.Seq), nil
	})

	var calls sync.WaitGroup
	for seq := range 32 {
		calls.Go(func() {
			in := invokeCall()
			in.Seq = seq
			got := invoke(context.Background(), tool, in, invokeAmple, nil)
			assert.Equal(t, outcome{result: fmt.Sprintf("found %d", seq)}, got)
		})
	}
	calls.Wait()
}

// invokeJournalRun is the run every actionFor test describes a call of.
func invokeJournalRun() Run {
	return Run{ID: invokeRunID, Agent: "alpha", Status: StatusRunnable}
}

func TestActionFor(t *testing.T) {
	noop := func(context.Context, Invocation) (string, error) { return "", nil }
	// The four attributes, as actionFor adds them for invokeCall.
	four := func(tool string) map[string]any {
		return map[string]any{"agent": "alpha", "tool": tool, "run": invokeRunID, "seq": 4}
	}
	with := func(attrs map[string]any, more map[string]any) map[string]any {
		for k, v := range more {
			attrs[k] = v
		}
		return attrs
	}
	exact := json.Number("12345678901234567890.000000000000000001")

	cases := []struct {
		name string
		tool Tool
		want Action
	}{
		{
			name: "a plain tool is described by default as a run of it",
			tool: Tool{Name: "lookup", Run: noop},
			want: Action{Kind: "run", Target: "lookup", Attrs: four("lookup")},
		},
		{
			name: "a delegating tool is described by default as a delegation to its agent",
			tool: Tool{Name: "ask_researcher", Delegate: "researcher"},
			want: Action{Kind: "delegate", Target: "researcher", Attrs: four("ask_researcher")},
		},
		{
			name: "a tool's own Action is used as it is, with the four added",
			tool: Tool{Name: "pay", Run: noop, Action: func(Invocation) Action {
				return Action{Kind: "pay", Target: "account-7", Attrs: map[string]any{
					"amount": json.Number("1200.50"), "external": true,
				}}
			}},
			want: Action{Kind: "pay", Target: "account-7", Attrs: with(four("pay"), map[string]any{
				"amount": json.Number("1200.50"), "external": true,
			})},
		},
		{
			name: "a delegating tool's own Action is used in place of the default",
			tool: Tool{Name: "ask_researcher", Delegate: "researcher", Action: func(Invocation) Action {
				return Action{Kind: "consult", Target: "research"}
			}},
			want: Action{Kind: "consult", Target: "research", Attrs: four("ask_researcher")},
		},
		{
			name: "an Action with no attributes gets the four",
			tool: Tool{Name: "pay", Run: noop, Action: func(Invocation) Action {
				return Action{Kind: "pay", Target: "account-7"}
			}},
			want: Action{Kind: "pay", Target: "account-7", Attrs: four("pay")},
		},
		{
			name: "an empty Action stays empty but for the four",
			tool: Tool{Name: "pay", Run: noop, Action: func(Invocation) Action { return Action{} }},
			want: Action{Attrs: four("pay")},
		},
		{
			name: "each of the four is left alone where the tool set it",
			tool: Tool{Name: "pay", Run: noop, Action: func(Invocation) Action {
				return Action{Kind: "pay", Attrs: map[string]any{
					"agent": "billing", "tool": "transfer", "run": "another", "seq": int64(99),
				}}
			}},
			want: Action{Kind: "pay", Attrs: map[string]any{
				"agent": "billing", "tool": "transfer", "run": "another", "seq": int64(99),
			}},
		},
		{
			name: "one the tool set to nothing is present, and left alone",
			tool: Tool{Name: "pay", Run: noop, Action: func(Invocation) Action {
				return Action{Kind: "pay", Attrs: map[string]any{"agent": nil, "tool": "", "run": 0, "seq": false}}
			}},
			want: Action{Kind: "pay", Attrs: map[string]any{"agent": nil, "tool": "", "run": 0, "seq": false}},
		},
		{
			name: "the ones the tool did not set are added beside the ones it did",
			tool: Tool{Name: "pay", Run: noop, Action: func(Invocation) Action {
				return Action{Kind: "pay", Attrs: map[string]any{"tool": "transfer", "seq": "fourth"}}
			}},
			want: Action{Kind: "pay", Attrs: map[string]any{
				"agent": "alpha", "tool": "transfer", "run": invokeRunID, "seq": "fourth",
			}},
		},
		{
			name: "nothing the tool wrote is normalised",
			tool: Tool{Name: "pay", Run: noop, Action: func(Invocation) Action {
				return Action{Kind: " Pay ", Target: "ACCOUNT/7 ", Attrs: map[string]any{
					"Agent": "Billing ", "amount": exact, "tags": []any{"A", "a"},
				}}
			}},
			want: Action{Kind: " Pay ", Target: "ACCOUNT/7 ", Attrs: with(four("pay"), map[string]any{
				"Agent": "Billing ", "amount": exact, "tags": []any{"A", "a"},
			})},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := actionFor(invokeJournalRun(), tc.tool, invokeCall())

			assert.Equal(t, tc.want, got)
		})
	}
}

func TestActionFor_TheRunNamesTheAgentAndTheRun(t *testing.T) {
	in := invokeCall()
	in.RunID, in.Agent = "an invocation's own idea of the run", "beta"
	tool := Tool{Name: "lookup", Run: func(context.Context, Invocation) (string, error) { return "", nil }}

	got := actionFor(invokeJournalRun(), tool, in)

	assert.Equal(t, "alpha", got.Attrs[AttrAgent])
	assert.Equal(t, invokeRunID, got.Attrs[AttrRun])
	assert.Equal(t, 4, got.Attrs[AttrSeq])
	assert.Equal(t, "lookup", got.Attrs[AttrTool])
}

func TestActionFor_TheToolsActionIsGivenTheInvocation(t *testing.T) {
	var received Invocation
	tool := Tool{Name: "pay", Action: func(in Invocation) Action {
		received = in
		return Action{Kind: "pay"}
	}}

	actionFor(invokeJournalRun(), tool, invokeCall())

	assert.Equal(t, invokeCall(), received)
}

func TestActionFor_DoesNotWriteToTheMapTheToolReturned(t *testing.T) {
	// A tool may hand back the same map for every call.
	shared := map[string]any{"external": true}
	tool := Tool{Name: "send", Action: func(Invocation) Action {
		return Action{Kind: "send", Target: "outside", Attrs: shared}
	}}

	var calls sync.WaitGroup
	for seq := range 16 {
		calls.Go(func() {
			in := invokeCall()
			in.Seq = seq
			got := actionFor(invokeJournalRun(), tool, in)
			assert.Equal(t, map[string]any{
				"external": true, "agent": "alpha", "tool": "send", "run": invokeRunID, "seq": seq,
			}, got.Attrs)
		})
	}
	calls.Wait()

	assert.Equal(t, map[string]any{"external": true}, shared)
}
