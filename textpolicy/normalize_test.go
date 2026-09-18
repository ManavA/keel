package textpolicy

import "testing"

// Invisible test characters are built with string(rune(...)) rather than
// written as literal bytes: several of them (the BOM in particular) are
// rejected by the Go compiler as raw source bytes outside the file's first
// position, and staticcheck flags the rest as easy to miss on sight. Building
// them at runtime keeps the test source itself plain ASCII.
func invisibleChar(codepoint rune) string {
	return string(codepoint)
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "trims surrounding space",
			in:   "  hello world  ",
			want: "hello world",
		},
		{
			name: "collapses internal whitespace",
			in:   "hello    world\t\tagain",
			want: "hello world again",
		},
		{
			name: "strips zero width space splitting a word",
			in:   "bad" + invisibleChar(0x200B) + "word",
			want: "badword",
		},
		{
			name: "strips zero width joiner and non-joiner",
			in:   "a" + invisibleChar(0x200C) + "b" + invisibleChar(0x200D) + "c",
			want: "abc",
		},
		{
			name: "strips word joiner",
			in:   "a" + invisibleChar(0x2060) + "b",
			want: "ab",
		},
		{
			name: "strips a leading byte order mark",
			in:   invisibleChar(0xFEFF) + "hello",
			want: "hello",
		},
		{
			name: "strips soft hyphen",
			in:   "hy" + invisibleChar(0x00AD) + "phen",
			want: "hyphen",
		},
		{
			name: "strips a variation selector",
			in:   "text" + invisibleChar(0xFE0F) + "more",
			want: "textmore",
		},
		{
			name: "NFKC-folds full-width letters to ASCII",
			in:   "Ｂａｄｗｏｒｄ", // fullwidth "Badword"
			want: "Badword",
		},
		{
			name: "empty string stays empty",
			in:   "",
			want: "",
		},
		{
			name: "only invisible characters collapses to empty",
			in:   invisibleChar(0x200B) + invisibleChar(0x200C) + invisibleChar(0x200D),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalize(tt.in)
			if got != tt.want {
				t.Errorf("normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestJoined(t *testing.T) {
	tests := []struct {
		name  string
		parts []string
		want  string
	}{
		{name: "joins non-empty parts with a space", parts: []string{"hello", "world"}, want: "hello world"},
		{name: "skips empty parts", parts: []string{"hello", "", "world"}, want: "hello world"},
		{name: "all empty yields empty", parts: []string{"", ""}, want: ""},
		{name: "no parts yields empty", parts: nil, want: ""},
		{name: "single part is itself", parts: []string{"solo"}, want: "solo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Joined(tt.parts...)
			if got != tt.want {
				t.Errorf("Joined(%v) = %q, want %q", tt.parts, got, tt.want)
			}
		})
	}
}
