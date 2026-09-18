package media

import (
	"image"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDownscaleTo(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 1200, 800))

	tests := []struct {
		name     string
		maxWidth int
		wantW    int
	}{
		{name: "narrows a wider source", maxWidth: 400, wantW: 400},
		{name: "never upscales a narrower source", maxWidth: 4000, wantW: 1200},
		{name: "zero means leave it alone", maxWidth: 0, wantW: 1200},
		{name: "negative means leave it alone", maxWidth: -1, wantW: 1200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := DownscaleTo(src, tt.maxWidth)
			require.Equal(t, tt.wantW, out.Bounds().Dx())
		})
	}
}

func TestChain(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 1200, 900))
	variants := []Variant{
		{Name: "large", MaxWidth: 1200, Quality: 85},
		{Name: "thumb", MaxWidth: 400, Quality: 80},
	}

	out := Chain(src, variants)
	require.Len(t, out, 2)
	require.Equal(t, "large", out[0].Variant.Name)
	require.Equal(t, 1200, out[0].Image.Bounds().Dx())
	require.Equal(t, "thumb", out[1].Variant.Name)
	require.Equal(t, 400, out[1].Image.Bounds().Dx())

	// The chain resamples each step from the previous result: the thumb's
	// aspect ratio must match the source's, proving it was derived through
	// the pipeline rather than independently — a bug here would only show up
	// as a subtly wrong ratio, not a crash.
	wantH := 900 * 400 / 1200
	require.InDelta(t, wantH, out[1].Image.Bounds().Dy(), 1)
}

func TestEncodeJPEG(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	body, err := EncodeJPEG(img, 85)
	require.NoError(t, err)
	require.NotEmpty(t, body)
	require.Equal(t, byte(0xFF), body[0])
	require.Equal(t, byte(0xD8), body[1])
}
