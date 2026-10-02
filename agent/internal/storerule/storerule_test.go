package storerule_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent/internal/storerule"
)

func TestKept(t *testing.T) {
	tests := []struct {
		name   string
		given  string
		kept   string
		inJSON string
	}{
		{"nothing to replace", "plain café \U0001F600", "plain café \U0001F600", "plain café \U0001F600"},
		{"empty", "", "", ""},
		{"a NUL", "a\x00b", "a�b", "a\x00b"},
		{"two NULs together", "a\x00\x00b", "a��b", "a\x00\x00b"},
		{"a byte that is not UTF-8", "caf\xff", "caf�", "caf�"},
		{"a three-byte character cut after two", "x\xe2\x82y", "x��y", "x��y"},
		{"a four-byte character cut after three", "x\xf0\x9f\x98", "x���", "x���"},
		{"a NUL beside a byte that is not UTF-8", "x\x00\xffy", "x��y", "x\x00�y"},
		{"a surrogate written as three bytes", "\xed\xa0\x80", "���", "���"},
		{"an overlong encoding", "\xc0\xaf", "��", "��"},
		{"the replacement character itself", "a�b", "a�b", "a�b"},
		{"good characters between bad bytes", "\xffé\xfe\U0001F600\x00", "�é�\U0001F600�", "�é�\U0001F600\x00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.kept, storerule.Kept(tt.given), "Kept")
			assert.Equal(t, tt.inJSON, storerule.KeptInJSON(tt.given), "KeptInJSON")
			assert.Equal(t, tt.given == tt.kept, storerule.Comparable(tt.given), "Comparable")
			// What is kept can be kept again as it is, and compared.
			assert.Equal(t, tt.kept, storerule.Kept(tt.kept))
			assert.True(t, storerule.Comparable(tt.kept))
		})
	}
}

// The rule is encoding/json's: a string inside JSON reads back as it does
// through Marshal and Unmarshal, byte for byte, for any bytes at all.
func TestKeptInJSON_IsWhatEncodingJSONDoes(t *testing.T) {
	samples := []string{"", "plain", "a\x00b", "x\xe2\x82y", "x\xf0\x9f\x98", "\xed\xa0\x80", "\xc0\xaf\xff\xfe", " <>&\"\\"}
	// Every byte, alone and between two letters.
	for b := range 256 {
		samples = append(samples, string([]byte{byte(b)}), string([]byte{'a', byte(b), 'z'}), string([]byte{0xe2, byte(b)}))
	}
	for _, s := range samples {
		text, err := json.Marshal(s)
		require.NoError(t, err)
		var back string
		require.NoError(t, json.Unmarshal(text, &back))
		require.Equal(t, back, storerule.KeptInJSON(s), "%q", s)
		require.Equal(t, strings.ReplaceAll(back, "\x00", "�"), storerule.Kept(s), "%q", s)
	}
}

func TestIsUUID(t *testing.T) {
	id := "3f2b8c1e-9a4d-4e6f-8b7a-0c1d2e3f4a5b"
	for name, tt := range map[string]struct {
		id   string
		want bool
	}{
		"as uuid.NewString writes one": {id, true},
		"a new one":                    {uuid.NewString(), true},
		"all zeros":                    {"00000000-0000-0000-0000-000000000000", true},
		"upper case":                   {strings.ToUpper(id), false},
		"without hyphens":              {strings.ReplaceAll(id, "-", ""), false},
		"in braces":                    {"{" + id + "}", false},
		"as a urn":                     {"urn:uuid:" + id, false},
		"with space around":            {" " + id, false},
		"empty":                        {"", false},
		"not one at all":               {"no-such-id", false},
	} {
		assert.Equal(t, tt.want, storerule.IsUUID(tt.id), name)
	}
}

func TestCursor(t *testing.T) {
	at := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	id := uuid.NewString()
	tests := []struct {
		name      string
		id        string
		at        time.Time
		wantStart bool
		wantErr   bool
	}{
		{"the zero cursor is the start", "", time.Time{}, true, false},
		{"an id and a time are a position", id, at, false, false},
		{"an id and no time are a position", id, time.Time{}, false, false},
		{"a time and no id", "", at, false, true},
		{"an id that is no UUID", "no-such-id", at, false, true},
		{"an id in upper case", strings.ToUpper(id), at, false, true},
		{"an id that is no UUID, and no time", "x", time.Time{}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, err := storerule.Cursor(tt.id, tt.at)
			assert.Equal(t, tt.wantStart, start)
			assert.Equal(t, tt.wantErr, err != nil, "%v", err)
		})
	}
}

func TestValidRaw(t *testing.T) {
	for name, tt := range map[string]struct {
		raw     string
		wantErr string
	}{
		"an object":                      {`{"a": 1}`, ""},
		"null":                           {`null`, ""},
		"a NUL as its escape":            {`{"a":"\u0000"}`, ""},
		"half a character as its escape": {`"\ud800"`, ""},
		"none":                           {``, ""},
		"cut short":                      {`{"a":`, "not valid JSON"},
		"a control character raw":        {"\"a\x00b\"", "not valid JSON"},
		"only space":                     {` `, "not valid JSON"},
		"two values":                     {`1 2`, "not valid JSON"},
		"not UTF-8":                      {"\"caf\xff\"", "not UTF-8"},
	} {
		err := storerule.ValidRaw([]byte(tt.raw))
		if tt.wantErr == "" {
			assert.NoError(t, err, name)
		} else {
			assert.ErrorContains(t, err, tt.wantErr, name)
		}
	}
	assert.NoError(t, storerule.ValidRaw(nil))
}

func TestOrNull(t *testing.T) {
	assert.Equal(t, json.RawMessage(`null`), storerule.OrNull(nil))
	assert.Equal(t, json.RawMessage(`null`), storerule.OrNull(json.RawMessage{}))
	given := json.RawMessage(`{"a": 1}`)
	got := storerule.OrNull(given)
	assert.Equal(t, given, got)
	got[0] = '['
	assert.Equal(t, json.RawMessage(`{"a": 1}`), given, "what is handed back is a copy")
}

func TestSets(t *testing.T) {
	for _, cause := range []string{"guard", "tool", "interrupted"} {
		assert.True(t, storerule.IsCause(cause), cause)
	}
	for _, cause := range []string{"", "Guard", "policy", "guard "} {
		assert.False(t, storerule.IsCause(cause), "%q", cause)
	}
	for _, status := range []string{"proposed", "waiting", "started", "completed", "blocked", "declined"} {
		assert.True(t, storerule.IsStepStatus(status), status)
	}
	for _, status := range []string{"", "done", "Completed", "runnable"} {
		assert.False(t, storerule.IsStepStatus(status), "%q", status)
	}
}

func TestListLimit(t *testing.T) {
	for limit, want := range map[int]int{0: 50, -1: 50, 1: 1, 50: 50, 200: 200, 201: 200, math.MaxInt: 200} {
		assert.Equal(t, want, storerule.ListLimit(limit), "limit %d", limit)
	}
}

func TestAttrs(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	tests := []struct {
		name  string
		attrs map[string]any
		want  map[string]any
	}{
		{"none", nil, map[string]any{}},
		{"empty", map[string]any{}, map[string]any{}},
		{
			"Go values become what JSON gives",
			map[string]any{"n": 2, "f": 1.5, "b": true, "nil": nil, "names": []string{"a", "b"}, "m": map[string]int{"k": 1}},
			map[string]any{"n": float64(2), "f": 1.5, "b": true, "nil": nil, "names": []any{"a", "b"}, "m": map[string]any{"k": float64(1)}},
		},
		{
			"a NUL and a byte that is not UTF-8, in keys and in strings at any depth",
			map[string]any{"a\x00b\xff": "a\x00b\xff", "deep": []any{map[string]any{"x\xe2\x82": []string{"\x00"}}}},
			map[string]any{"a�b�": "a�b�", "deep": []any{map[string]any{"x��": []any{"�"}}}},
		},
		{
			"raw JSON is read: half a character, a NUL, numbers",
			map[string]any{"input": raw(`{"a":"\ud800","n":[1, 1e3],"z":"\u0000"}`), "num": json.Number("12345678901234567890")},
			map[string]any{"input": map[string]any{"a": "�", "n": []any{float64(1), float64(1000)}, "z": "�"}, "num": float64(12345678901234567890)},
		},
		{"the six characters of the escape are not a NUL", map[string]any{"s": `\u0000`}, map[string]any{"s": `\u0000`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := storerule.Attrs(tt.attrs)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)

			// What comes back is plain JSON a database takes: it is its own
			// round trip, and holds no NUL.
			assert.False(t, holdsNUL(got))
			again, err := storerule.Attrs(got)
			require.NoError(t, err)
			assert.Equal(t, got, again)
		})
	}

	refused := map[string]map[string]any{
		"a channel":                 {"reply": make(chan string)},
		"a number past float64":     {"n": json.Number("1e400000")},
		"one inside raw JSON":       {"input": raw(`{"n":-1e999}`)},
		"raw JSON that is not JSON": {"input": raw(`{"n":`)},
		"a number that is none":     {"n": json.Number("twelve")},
		"not a number":              {"n": math.NaN()},
	}
	for name, attrs := range refused {
		got, err := storerule.Attrs(attrs)
		require.Error(t, err, name)
		assert.Nil(t, got, name)
	}
}

func TestMetadata(t *testing.T) {
	got := storerule.Metadata(nil)
	assert.Equal(t, map[string]string{}, got, "none is an empty map")

	given := map[string]string{"plain": "kept", "a\x00b": "x\xe2\x82y", "markup": "<a href=\"x\">&</a>"}
	got = storerule.Metadata(given)
	assert.Equal(t, map[string]string{"plain": "kept", "a�b": "x��y", "markup": "<a href=\"x\">&</a>"}, got)
	got["plain"] = "changed"
	assert.Equal(t, "kept", given["plain"], "what is handed back is a copy")
}

func TestTimes(t *testing.T) {
	zone := time.FixedZone("elsewhere", 5*60*60+30*60)
	base := time.Date(2026, 3, 2, 10, 0, 0, 0, zone)
	const micro = time.Microsecond

	for name, tt := range map[string]struct {
		given, instant, expiry time.Time
	}{
		"a whole microsecond":    {base.Add(7 * micro), base.Add(7 * micro), base.Add(7 * micro)},
		"a nanosecond past one":  {base.Add(7*micro + 1), base.Add(7 * micro), base.Add(8 * micro)},
		"a nanosecond short":     {base.Add(8*micro - 1), base.Add(7 * micro), base.Add(8 * micro)},
		"a whole second":         {base, base, base},
		"before the year 1970":   {time.Date(1969, 12, 31, 23, 59, 59, 1, time.UTC), time.Date(1969, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(1969, 12, 31, 23, 59, 59, 1000, time.UTC)},
		"the zero time":          {time.Time{}, time.Time{}, time.Time{}},
		"with a monotonic clock": {time.Now(), time.Now().Truncate(micro), time.Time{}},
	} {
		if name == "with a monotonic clock" {
			// Only that the reading is gone, so that two kept times compare.
			got := storerule.Instant(tt.given)
			assert.Equal(t, got, got.Round(0), name)
			continue
		}
		assert.True(t, tt.instant.Equal(storerule.Instant(tt.given)), "%s: Instant gave %s", name, storerule.Instant(tt.given))
		assert.True(t, tt.expiry.Equal(storerule.Expiry(tt.given)), "%s: Expiry gave %s", name, storerule.Expiry(tt.given))
	}
	assert.Equal(t, zone, storerule.Instant(base.Add(1)).Location(), "a time keeps its zone")

	kept := storerule.InstantPtr(nil)
	assert.Nil(t, kept)
	given := base.Add(1500)
	kept = storerule.InstantPtr(&given)
	require.NotNil(t, kept)
	assert.True(t, base.Add(micro).Equal(*kept))
	assert.True(t, base.Add(1500).Equal(given), "the time given is not changed")
}

func TestActiveMillis(t *testing.T) {
	start := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	for name, tt := range map[string]struct {
		start *time.Time
		end   time.Time
		want  int64
	}{
		"never started":                        {nil, start, 0},
		"a second and a half":                  {&start, start.Add(1500 * time.Millisecond), 1500},
		"a nanosecond short of a millisecond":  {&start, start.Add(time.Millisecond - 1), 0},
		"between the times as they are kept":   {ptr(start.Add(999)), start.Add(1500*time.Millisecond + 500), 1500},
		"finished before it started":           {ptr(start.Add(10 * time.Second)), start.Add(7500 * time.Millisecond), -2500},
		"finished before it started, by a bit": {ptr(start.Add(time.Millisecond)), start.Add(999 * time.Microsecond), 0},
	} {
		assert.Equal(t, tt.want, storerule.ActiveMillis(tt.start, tt.end), name)
	}
}

func ptr(t time.Time) *time.Time { return &t }

// holdsNUL reports whether a value that has been through JSON has a NUL in
// a string or a key, at any depth.
func holdsNUL(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.ContainsRune(v, 0)
	case []any:
		for _, e := range v {
			if holdsNUL(e) {
				return true
			}
		}
	case map[string]any:
		for key, e := range v {
			if strings.ContainsRune(key, 0) || holdsNUL(e) {
				return true
			}
		}
	}
	return false
}
