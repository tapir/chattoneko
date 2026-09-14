package tools

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif" // gif.Decode yields the first frame only, which is what we want
	_ "image/jpeg"
	_ "image/png"
	"math"

	"github.com/skrashevich/go-webp"
	_ "golang.org/x/image/bmp" // registers "bmp" with image.Decode
	"golang.org/x/image/draw"
)

// The server half of the browser's pre-upload image conversion
// (web/src/lib/media.js): a picture create_file is handed is decoded, capped
// and re-encoded to WebP, so a file the model drew or fetched lands in the chat
// in the shape a user's upload does — and, being WebP, is one of the two mimes
// attach.SendsAsImage lets a vision model actually read.
const (
	maxImageSide = 1280 // media.js MAX_SIDE
	imageQuality = 75   // media.js IMAGE_QUALITY, on go-webp's 0-100 scale

	// maxDecodePixels guards the decode, not the file: a 64 MiB JPEG can claim
	// 65535x65535, and honouring that allocates gigabytes. This is far past
	// any real photo and still a bounded raster.
	// ponytail: a flat pixel cap; make it per-format if a legit 100 MP scan
	// ever gets refused.
	maxDecodePixels = 64 << 20
)

// scaleToFit is media.js scaleToFit: cap the LONGEST side at limit, keep the
// aspect ratio, never upscale, and never shrink by more than 2x — past that,
// squeezing a 5000px photo into 1280 throws away more detail than it saves
// bytes, so it stops at half size instead.
func scaleToFit(w, h, limit int) (int, int) {
	long := max(w, h)
	if long <= limit {
		return w, h
	}
	k := math.Max(float64(limit), float64(long)/2) / float64(long)
	fit := func(v int) int { return max(1, int(math.Round(float64(v)*k))) }
	return fit(w), fit(h)
}

// toWebP decodes one picture, caps it with scaleToFit and re-encodes it as lossy
// WebP at imageQuality — the three steps media.js convertImage runs in the
// browser. An animated GIF or WebP keeps its first frame, which is all a decode
// (and a canvas draw of it) yields.
//
// EXIF orientation is NOT applied: image/jpeg ignores the tag and nothing else
// in the accepted set carries one, so a phone photo stored sideways stays
// sideways. The browser gets this free from createImageBitmap and there is no
// stdlib equivalent.
// ponytail: skipped deliberately; read the JPEG APP1 orientation tag here if
// rotated tool images ever get reported.
func toWebP(data []byte) ([]byte, error) {
	// Config before pixels: it reads the header only, so the cap costs nothing.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return nil, fmt.Errorf("the image is %dx%d pixels, over the %d-megapixel limit",
			cfg.Width, cfg.Height, maxDecodePixels>>20)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	src := img.Bounds()
	w, h := scaleToFit(src.Dx(), src.Dy(), maxImageSide)
	// NRGBA is what a canvas hands toBlob, and the one layout go-webp's
	// has-alpha check has a fast path for.
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	// ApproxBiLinear is the nearest thing to a canvas drawImage, and scaleToFit's
	// 2x floor means it never downscales far enough for bilinear to alias.
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, src, draw.Src, nil)

	var buf bytes.Buffer
	if err := webp.Encode(&buf, dst, &webp.Options{Lossy: true, Quality: imageQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
