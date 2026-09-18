package media

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"

	"github.com/disintegration/imaging"
)

// Variant is one derivative size to produce from a decoded source.
type Variant struct {
	// Name identifies the variant (e.g. "large", "thumb") and is typically
	// folded into the storage key by the caller.
	Name string
	// MaxWidth caps the derivative's width, preserving aspect ratio. Zero
	// keeps the source's width unchanged.
	MaxWidth int
	// Quality is the JPEG encode quality, 1-100.
	Quality int
}

// Resized is one Variant's output image, before encoding.
type Resized struct {
	Variant Variant
	Image   image.Image
}

// Chain resizes src into every variant in order, each one resampled from the
// previous result rather than from src again.
//
// Callers must list variants widest first. Resampling a 1200px "large" down
// to a 400px "thumb" costs less than resampling the full source twice, and
// produces a sharper thumbnail because it is derived from an
// already-decoded image with no intervening lossy encode. Chain does not
// sort the input; an incorrectly ordered list produces a visibly wrong
// result rather than being silently corrected.
func Chain(src image.Image, variants []Variant) []Resized {
	out := make([]Resized, len(variants))
	cur := src
	for i, v := range variants {
		resized := DownscaleTo(cur, v.MaxWidth)
		cur = resized
		out[i] = Resized{Variant: v, Image: resized}
	}
	return out
}

// DownscaleTo narrows src to maxWidth, preserving aspect ratio, and never
// upscales: a source already narrower than maxWidth is returned unchanged.
// Upscaling would produce a derivative that is larger, blurrier, and heavier
// than its source, with no benefit. maxWidth <= 0 returns src unchanged.
func DownscaleTo(src image.Image, maxWidth int) image.Image {
	if maxWidth <= 0 || src.Bounds().Dx() <= maxWidth {
		return src
	}
	return imaging.Resize(src, maxWidth, 0, imaging.Lanczos)
}

// EncodeJPEG encodes img as a JPEG at the given quality (1-100).
func EncodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("media: encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}
