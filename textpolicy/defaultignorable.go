package textpolicy

import "unicode"

//go:generate go run generate.go

// defaultIgnorable is Unicode's Default_Ignorable_Code_Point property,
// version 18.0.0, transcribed in full from DerivedCoreProperties.txt (see
// generate.go). It is not filtered by general category: several of its
// ranges are Cn (unassigned — reserved so that whatever character Unicode
// eventually assigns there stays invisible by default) rather than Cf, Mn or
// Lo, and a predicate built only from named, assigned categories misses
// those reserved code points entirely. shouldStrip strips this table
// wholesale, in addition to (not instead of) every Cc and Cf character, so
// it can only ever strip a superset of Default_Ignorable_Code_Point —
// TestShouldStripIsSupersetOfDefaultIgnorable checks that against an
// independently transcribed copy of the same property.
var defaultIgnorable = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x00AD, Hi: 0x00AD, Stride: 1},
		{Lo: 0x034F, Hi: 0x034F, Stride: 1},
		{Lo: 0x061C, Hi: 0x061C, Stride: 1},
		{Lo: 0x115F, Hi: 0x1160, Stride: 1},
		{Lo: 0x17B4, Hi: 0x17B5, Stride: 1},
		{Lo: 0x180B, Hi: 0x180F, Stride: 1},
		{Lo: 0x200B, Hi: 0x200F, Stride: 1},
		{Lo: 0x202A, Hi: 0x202E, Stride: 1},
		{Lo: 0x2060, Hi: 0x206F, Stride: 1},
		{Lo: 0x3164, Hi: 0x3164, Stride: 1},
		{Lo: 0xFE00, Hi: 0xFE0F, Stride: 1},
		{Lo: 0xFEFF, Hi: 0xFEFF, Stride: 1},
		{Lo: 0xFFA0, Hi: 0xFFA0, Stride: 1},
		{Lo: 0xFFF0, Hi: 0xFFF8, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x1BCA0, Hi: 0x1BCA3, Stride: 1},
		{Lo: 0x1D173, Hi: 0x1D17A, Stride: 1},
		{Lo: 0xE0000, Hi: 0xE0FFF, Stride: 1},
	},
}

// shouldStrip reports whether normalize removes r.
func shouldStrip(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(defaultIgnorable, r)
}
