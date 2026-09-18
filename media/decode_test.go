package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func encodeJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, nil))
	return buf.Bytes()
}

func TestDecode(t *testing.T) {
	t.Run("decodes a plain jpeg", func(t *testing.T) {
		body := encodeJPEG(t, 8, 4)
		d, err := Decode(bytes.NewReader(body), DecodeOptions{})
		require.NoError(t, err)
		require.Equal(t, "image/jpeg", d.ContentType)
		require.Equal(t, 8, d.Image.Bounds().Dx())
		require.Equal(t, 4, d.Image.Bounds().Dy())
	})

	t.Run("decodes a plain png", func(t *testing.T) {
		img := image.NewNRGBA(image.Rect(0, 0, 3, 5))
		var buf bytes.Buffer
		require.NoError(t, png.Encode(&buf, img))
		d, err := Decode(bytes.NewReader(buf.Bytes()), DecodeOptions{})
		require.NoError(t, err)
		require.Equal(t, "image/png", d.ContentType)
	})

	t.Run("rejects a source over the byte cap", func(t *testing.T) {
		body := encodeJPEG(t, 64, 64)
		_, err := Decode(bytes.NewReader(body), DecodeOptions{MaxBytes: 10})
		require.ErrorIs(t, err, ErrTooLarge)
	})

	t.Run("rejects an unsupported content type", func(t *testing.T) {
		_, err := Decode(strings.NewReader("not an image, just text padding to be sniffable as plain text"), DecodeOptions{})
		require.ErrorIs(t, err, ErrUnsupportedType)
	})

	t.Run("respects a caller-narrowed allow-list", func(t *testing.T) {
		img := image.NewNRGBA(image.Rect(0, 0, 3, 3))
		var buf bytes.Buffer
		require.NoError(t, png.Encode(&buf, img))
		_, err := Decode(bytes.NewReader(buf.Bytes()), DecodeOptions{AllowedTypes: []string{"image/jpeg"}})
		require.ErrorIs(t, err, ErrUnsupportedType)
	})
}

// buildExifOrientationJPEG wraps a plain JPEG (built by the standard library,
// with no EXIF at all) in a minimal hand-built EXIF APP1 segment carrying a
// single Orientation tag, so Decode's EXIF handling can be exercised without
// a fixture file or an EXIF-writing dependency.
func buildExifOrientationJPEG(t *testing.T, width, height int, orientation uint16) []byte {
	t.Helper()
	raw := encodeJPEG(t, width, height)
	require.True(t, len(raw) > 2 && raw[0] == 0xFF && raw[1] == 0xD8, "expected a JPEG SOI marker")

	var tiff bytes.Buffer
	tiff.WriteString("II")
	require.NoError(t, binary.Write(&tiff, binary.LittleEndian, uint16(42)))
	require.NoError(t, binary.Write(&tiff, binary.LittleEndian, uint32(8)))
	require.NoError(t, binary.Write(&tiff, binary.LittleEndian, uint16(1))) // one IFD entry
	require.NoError(t, binary.Write(&tiff, binary.LittleEndian, uint16(0x0112)))
	require.NoError(t, binary.Write(&tiff, binary.LittleEndian, uint16(3))) // SHORT
	require.NoError(t, binary.Write(&tiff, binary.LittleEndian, uint32(1)))
	require.NoError(t, binary.Write(&tiff, binary.LittleEndian, orientation))
	require.NoError(t, binary.Write(&tiff, binary.LittleEndian, uint16(0))) // pad value field to 4 bytes
	require.NoError(t, binary.Write(&tiff, binary.LittleEndian, uint32(0))) // no next IFD

	exifPayload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	app1Len := uint16(len(exifPayload) + 2)

	var out bytes.Buffer
	out.Write(raw[:2])
	out.WriteByte(0xFF)
	out.WriteByte(0xE1)
	require.NoError(t, binary.Write(&out, binary.BigEndian, app1Len))
	out.Write(exifPayload)
	out.Write(raw[2:])
	return out.Bytes()
}

func TestDecode_EXIFOrientation(t *testing.T) {
	tests := []struct {
		name        string
		orientation uint16
		wantSwapped bool // true when width and height should come out swapped
	}{
		{name: "orientation 1 (normal) keeps dimensions", orientation: 1, wantSwapped: false},
		{name: "orientation 3 (180 rotate) keeps dimensions", orientation: 3, wantSwapped: false},
		{name: "orientation 6 (90 rotate) swaps dimensions", orientation: 6, wantSwapped: true},
		{name: "orientation 8 (270 rotate) swaps dimensions", orientation: 8, wantSwapped: true},
	}

	const width, height = 6, 2

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := buildExifOrientationJPEG(t, width, height, tt.orientation)
			d, err := Decode(bytes.NewReader(body), DecodeOptions{})
			require.NoError(t, err)

			gotW, gotH := d.Image.Bounds().Dx(), d.Image.Bounds().Dy()
			if tt.wantSwapped {
				require.Equal(t, height, gotW, "width")
				require.Equal(t, width, gotH, "height")
			} else {
				require.Equal(t, width, gotW, "width")
				require.Equal(t, height, gotH, "height")
			}
		})
	}
}
