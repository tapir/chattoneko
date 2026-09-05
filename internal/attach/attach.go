// Package attach processes file uploads: content sniffing, image -> PNG
// conversion (GIF first frame, WebP via x/image/webp), text validation, and
// filename sanitization.
package attach

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/gif"  // register gif decoder (first frame)
	_ "image/jpeg" // register jpeg decoder
	"image/png"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register webp decoder
)

// Kinds of attachments.
const (
	KindImage = "image"
	KindText  = "text"
)

// ErrUnsupported is returned when the content cannot be accepted (HTTP 415).
var ErrUnsupported = errors.New("unsupported file type")

// ErrTooLarge is returned when the file exceeds the configured limit (HTTP 413).
var ErrTooLarge = errors.New("file too large")

// maxImageSide caps the longest side of converted images (limits resend payload).
const maxImageSide = 2048

// encodePNG renders img as PNG at the largest size whose encoded bytes fit
// within maxBytes (0 = no limit). A noisy photo can produce a PNG larger than
// the source upload; when that would blow the per-file cap we downscale and
// retry, so large phone photos are still accepted and only the compact PNG is
// kept (matches the "only the converted PNG is stored" invariant).
func encodePNG(img image.Image, maxBytes int64) ([]byte, error) {
	const attempts = 5
	for i := 0; i < attempts; i++ {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("encode png: %w", err)
		}
		if maxBytes <= 0 || int64(buf.Len()) <= maxBytes {
			return buf.Bytes(), nil
		}
		// Too big: shrink to 75% per axis for the next attempt.
		b := img.Bounds()
		if b.Dx() <= 1 && b.Dy() <= 1 {
			break // cannot shrink further
		}
		img = scale(img, b.Dx()*3/4, b.Dy()*3/4)
	}
	return nil, ErrTooLarge
}

// scale resamples img to w×h. x/image/draw replaces a hand-rolled
// nearest-neighbor loop: it is faster, bilinear (no aliasing on downscaled
// photos), and x/image is already a dependency for WebP decoding.
func scale(img image.Image, w, h int) image.Image {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
	return dst
}

// textExts hints the mime for accepted text; anything missing is text/plain.
// Acceptance is content-based (looksText), not extension-based.
var textExts = map[string]string{
	"md": "text/markdown", "markdown": "text/markdown",
	"yaml": "text/yaml", "yml": "text/yaml",
	"json": "application/json", "jsonl": "application/jsonl", "ndjson": "application/jsonl",
	"xml": "application/xml", "csv": "text/csv", "tsv": "text/tab-separated-values",
	"html": "text/html", "css": "text/css",
}

// Result of processing one uploaded file.
type Result struct {
	Kind string
	Mime string // detected mime (image/png for converted images)
	Data []byte // PNG bytes for images, raw UTF-8 for text
	Size int64  // stored byte count (== len(Data))
}

// maxDecodeSide rejects absurdly large source images. It is enforced from the
// image header, before any pixel buffer is allocated, so a tiny file claiming
// huge dimensions cannot OOM the process; typical photos are far below this.
const maxDecodeSide = 12000

// MaxRawUploadBytes caps the raw bytes read for a single file before decoding
// (a safety net; the real limit is applied to the converted PNG below).
// Exported so the HTTP layer can size its multipart ceiling from the same
// constant instead of duplicating it.
const MaxRawUploadBytes = 64 * 1024 * 1024 // 64 MiB

// Process sniffs and converts one uploaded file.
// Content-based validation: extension is only a hint for mime. Images are
// always re-encoded to PNG (JPEG/GIF/WebP sources live only in memory during
// this call); the per-file cap is enforced on the *converted PNG*, downscaling
// as needed, so typical large phone photos are accepted and only the compact
// PNG is stored. Text files are enforced against the cap directly.
func Process(filename string, data []byte, maxBytes int64) (*Result, error) {
	if int64(len(data)) > MaxRawUploadBytes {
		return nil, ErrTooLarge
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty file", ErrUnsupported)
	}
	// Image first: parse only the header (DecodeConfig) so a "decompression
	// bomb" — a tiny file claiming absurd dimensions — is rejected before any
	// pixel buffer is allocated.
	if cfg, format, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		if cfg.Width > maxDecodeSide || cfg.Height > maxDecodeSide {
			return nil, fmt.Errorf("%w: image dimensions too large", ErrUnsupported)
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("%w: corrupt %s image", ErrUnsupported, format)
		}
		// Bound the pixel count BEFORE any further copies: the orientation
		// transform below allocates two full-size RGBA buffers, and downscaling
		// commutes with rotation/flip, so shrinking first caps that work at
		// maxImageSide² instead of the (much larger) source resolution.
		img = downscale(img, maxImageSide)
		// Phone JPEGs carry their intended rotation in EXIF while the pixels
		// stay in sensor orientation. stdlib's decoder ignores EXIF and the
		// PNG re-encode below drops it, so bake the rotation into the pixels
		// — otherwise camera photos render rotated (see exif.go).
		if format == "jpeg" {
			img = applyOrientation(img, jpegOrientation(data))
		}
		pngData, err := encodePNG(img, maxBytes)
		if err != nil {
			return nil, err
		}
		return &Result{
			Kind: KindImage,
			Mime: "image/png",
			Data: pngData,
			Size: int64(len(pngData)),
		}, nil
	} else if ct := http.DetectContentType(data); strings.HasPrefix(ct, "image/") {
		// Known image magic bytes with an unparseable header.
		return nil, fmt.Errorf("%w: corrupt %s image", ErrUnsupported, strings.TrimPrefix(ct, "image/"))
	}
	// Text: content-based check. Cap applies to the stored text bytes.
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, ErrTooLarge
	}
	if !looksText(data) {
		return nil, ErrUnsupported
	}
	mime := "text/plain"
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(filename)), ".")
	if m, ok := textExts[ext]; ok {
		mime = m
	}
	return &Result{
		Kind: KindText,
		Mime: mime,
		Data: data,
		Size: int64(len(data)),
	}, nil
}

// CleanFilename validates and normalizes an uploaded or tool-provided
// filename: a plain name of bounded length, no directories, no control
// characters. Names end up in the DB, in LLM prompts and in download
// headers, so both call sites (user uploads, create_text_file) share this
// one check.
func CleanFilename(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", errors.New("filename is required")
	case len(name) > 200:
		return "", errors.New("filename too long (max 200 bytes)")
	case name == "." || name == "..":
		return "", errors.New("invalid filename")
	case strings.ContainsAny(name, "/\\"):
		return "", errors.New("filename must be a plain name without directories")
	}
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			return "", errors.New("filename contains control characters")
		// Bidi override/embedding/isolate marks can visually reorder a name
		// (disguising a download's real extension), so reject them. Deliberately
		// NOT the zero-width joiners (U+200C/D): they join emoji, not text direction.
		case r == 0x200e || r == 0x200f,
			r >= 0x202a && r <= 0x202e,
			r >= 0x2066 && r <= 0x2069:
			return "", errors.New("filename contains bidirectional control characters")
		}
	}
	return name, nil
}

// looksText reports whether non-empty data is acceptable as plain text:
// valid UTF-8, no NUL bytes, >=95% printable/whitespace runes.
func looksText(data []byte) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	if !utf8.Valid(data) {
		return false
	}
	total := 0
	good := 0
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		data = data[size:]
		total++
		if r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r != 0x7F) {
			good++
		}
	}
	return total > 0 && float64(good)/float64(total) >= 0.95
}


// downscale reduces img so its longest side is at most maxSide, preserving
// the aspect ratio.
func downscale(img image.Image, maxSide int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxSide && h <= maxSide {
		return img
	}
	if w >= h {
		return scale(img, maxSide, h*maxSide/w)
	}
	return scale(img, w*maxSide/h, maxSide)
}

// SerializeText formats a text attachment for injection into a user message:
//
//	<file name="notes.md" id="a1b2c3d4">
//	...content...
//	</file id="a1b2c3d4">
//
// The filename is escaped and the closing tag repeats the
// attachment's unguessable random id (M2), so a stray or hostile "</file>"
// inside the file content cannot terminate the block early.
func SerializeText(filename, id, content string) string {
	return fmt.Sprintf("<file name=\"%s\" id=%q>\n%s\n</file id=%q>",
		html.EscapeString(filename), id, content, id)
}

// SerializeImageDescription formats a vision-model description of an image
// attachment for injection into a user message. It uses the same <file>
// envelope as SerializeText (keeping the image's original filename so the
// model can associate the description with the attachment); a header line
// marks the block as an image description rather than file content.
func SerializeImageDescription(filename, id, description string) string {
	body := fmt.Sprintf("[Detailed text description of the image file %q, generated by a vision model]\n%s",
		filename, description)
	return SerializeText(filename, id, body)
}

