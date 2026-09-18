package media

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"

	"github.com/disintegration/imaging"
)

// DefaultMaxBytes caps how much of an untrusted source Decode will buffer
// before giving up. A typical photo from a phone or a third-party feed is
// in the hundreds of kilobytes to a few megabytes; this is set well above
// that and well below a size that would threaten a job's memory limit if a
// source serves an unexpectedly large file.
//
// This bounds the encoded input only. A small, validly-encoded file can
// still decode to a very large pixel buffer — see DefaultMaxPixels, the
// separate bound that exists for exactly that case.
const DefaultMaxBytes = 24 << 20 // 24 MiB

// DefaultMaxPixels caps the decoded image's width times height. A 168 KB
// PNG can legitimately decode to well over 100 million pixels if it is
// mostly a uniform color, since PNG's compression has no relationship to
// decoded size; DefaultMaxBytes does not protect against this at all. Set
// well above any real photo (a 48-megapixel phone sensor is under 50
// million pixels) and well below a size that would threaten a job's memory
// limit once decoded to raw pixels.
const DefaultMaxPixels = 60_000_000 // 60 megapixels

// ErrTooLarge is returned when a source exceeds Options.MaxBytes.
var ErrTooLarge = errors.New("media: source exceeds max bytes")

// ErrTooManyPixels is returned when a source's decoded dimensions exceed
// Options.MaxPixels.
var ErrTooManyPixels = errors.New("media: source exceeds max pixels")

// ErrUnsupportedType is returned when the sniffed content type is not in
// Options.AllowedTypes.
var ErrUnsupportedType = errors.New("media: unsupported content type")

// DefaultAllowedTypes are the content types Decode accepts when
// Options.AllowedTypes is nil. Limited to what imaging.Decode can actually
// decode — accepting a type here that the decoder cannot read would turn a
// clear "unsupported type" error into a confusing decode failure instead.
var DefaultAllowedTypes = []string{"image/jpeg", "image/png", "image/gif"}

// DecodeOptions configures Decode. The zero value is a working default:
// 24MiB byte cap, 60-megapixel decoded-size cap, JPEG/PNG/GIF only.
type DecodeOptions struct {
	MaxBytes     int64
	MaxPixels    int64
	AllowedTypes []string
}

func (o DecodeOptions) withDefaults() DecodeOptions {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxPixels <= 0 {
		o.MaxPixels = DefaultMaxPixels
	}
	if o.AllowedTypes == nil {
		o.AllowedTypes = DefaultAllowedTypes
	}
	return o
}

// Decoded is the result of a successful Decode.
type Decoded struct {
	Image       image.Image
	ContentType string
	Bytes       int64
}

// Decode reads r up to opts.MaxBytes, sniffs the real content type from the
// bytes themselves (never a caller-supplied or header-claimed one), rejects
// anything outside opts.AllowedTypes, and decodes with EXIF orientation
// applied — a photo shot in portrait on a phone held sideways comes out
// upright here, before a resize bakes the wrong orientation into every
// derivative it produces downstream.
//
// It reads one byte past the limit so an exactly-at-the-limit source is
// distinguishable from a truncated-because-longer one; DefaultMaxBytes never
// legitimately fits a real photo that tightly, so tripping it is itself the
// signal that something is wrong with the source, not the read.
func Decode(r io.Reader, opts DecodeOptions) (*Decoded, error) {
	opts = opts.withDefaults()

	body, err := io.ReadAll(io.LimitReader(r, opts.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("media: read source: %w", err)
	}
	if int64(len(body)) > opts.MaxBytes {
		return nil, fmt.Errorf("%w: limit %d bytes", ErrTooLarge, opts.MaxBytes)
	}

	contentType := sniff(body)
	if !allowedType(contentType, opts.AllowedTypes) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedType, contentType)
	}

	// image.DecodeConfig reads only the header — width and height — without
	// allocating a pixel buffer, so a decoded-size bomb is caught before the
	// expensive full decode below ever runs.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("media: decode header: %w", err)
	}
	pixels := int64(cfg.Width) * int64(cfg.Height)
	if pixels > opts.MaxPixels {
		return nil, fmt.Errorf("%w: %dx%d = %d pixels, limit %d", ErrTooManyPixels, cfg.Width, cfg.Height, pixels, opts.MaxPixels)
	}

	// imaging.Decode (not image.Decode) reads and applies the EXIF
	// Orientation tag itself, so orientation correction happens exactly once,
	// here, rather than needing every caller that resizes or crops the result
	// to remember it.
	img, err := imaging.Decode(bytes.NewReader(body), imaging.AutoOrientation(true))
	if err != nil {
		return nil, fmt.Errorf("media: decode: %w", err)
	}

	return &Decoded{Image: img, ContentType: contentType, Bytes: int64(len(body))}, nil
}

// sniff identifies the real content type from the bytes themselves.
// http.DetectContentType never errors; an unrecognised source falls back to
// "application/octet-stream", which allowedType then rejects — the caller
// learns "not a supported image" rather than being handed a false positive.
func sniff(body []byte) string {
	n := len(body)
	if n > 512 {
		n = 512
	}
	return http.DetectContentType(body[:n])
}

func allowedType(contentType string, allowed []string) bool {
	for _, a := range allowed {
		if a == contentType {
			return true
		}
	}
	return false
}
