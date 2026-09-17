package tools

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif" // gif.Decode yields the first frame only, which is what we want
	_ "image/jpeg"
	"image/png"
	"math"

	"github.com/delthas/octreequant"
	_ "golang.org/x/image/bmp" // registers "bmp" with image.Decode
	"golang.org/x/image/draw"
)

// The server half of the browser's pre-upload image conversion
// (web/src/lib/media.js): a picture create_file is handed is decoded, capped
// and re-encoded to PNG — a 256-colour one when image_quantization is on — so a
// file the model drew or fetched lands in the chat in the shape a user's upload
// does, and in a mime attach.SendsAsImage lets a vision model read.
const (
	maxImageSide = 1280 // media.js MAX_SIDE
	paletteSize  = 256  // png-enc.js COLORS

	// maxDecodePixels guards the decode, not the file: a 64 MiB JPEG can claim
	// 65535x65535, and honouring that allocates gigabytes. This is far past
	// any real photo and still a bounded raster.
	// ponytail: a flat pixel cap; make it per-format if a legit 100 MP scan
	// ever gets refused.
	maxDecodePixels = 64 << 20
)

// scaleToFit is media.js scaleToFit: cap the longest side at limit, keep the
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

// toPNG decodes one picture, caps it with scaleToFit and re-encodes it as a
// PNG — the three steps media.js convertImage runs in the browser. An animated
// GIF or WebP keeps its first frame, which is all a decode (and a canvas draw
// of it) yields. quantize is the image_quantization setting: on, the picture
// ends as a 256-colour indexed PNG, several times smaller than the lossless
// one it replaces.
//
// EXIF orientation is not applied: image/jpeg ignores the tag and nothing else
// in the accepted set carries one, so a phone photo stored sideways stays
// sideways. The browser gets this free from createImageBitmap and there is no
// stdlib equivalent.
// ponytail: skipped deliberately; read the JPEG APP1 orientation tag here if
// rotated tool images ever get reported.
func toPNG(data []byte, quantize bool) ([]byte, error) {
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
	// NRGBA is the layout a canvas holds, and one png.Encode has a fast path for.
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	// ApproxBiLinear is the nearest thing to a canvas drawImage, and scaleToFit's
	// 2x floor means it never downscales far enough for bilinear to alias.
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, src, draw.Src, nil)

	// A palette carries one alpha per colour, so quantizing a picture with soft
	// edges fringes them: those stay lossless even when quantization is on.
	out := image.Image(dst)
	if quantize && binaryAlpha(dst.Pix) {
		out = octreequant.Paletted(dst, paletteSize)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// binaryAlpha reports whether NRGBA pixels hold no partially transparent one.
func binaryAlpha(pix []byte) bool {
	for i := 3; i < len(pix); i += 4 {
		if a := pix[i]; a != 0 && a != 255 {
			return false
		}
	}
	return true
}
