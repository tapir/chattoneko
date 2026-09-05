// EXIF orientation normalization for uploaded JPEGs.
//
// Phone cameras store pixels in the sensor's native orientation and record
// the intended orientation in the EXIF Orientation tag (0x0112). Go's
// standard-library JPEG decoder ignores EXIF entirely, and this package
// re-encodes every image to PNG (which carries no EXIF), so an uncorrected
// photo would arrive rotated 90/180/270° everywhere downstream (message
// rendering, vision model). Fix: read the tag from the raw upload and bake
// the rotation into the pixels before downscale + re-encode.
package attach

import (
	"encoding/binary"
	"image"
	"image/draw"
)

// jpegOrientation extracts the EXIF orientation (1-8) from raw JPEG bytes.
// It returns 1 (the "no transform needed" normal orientation) whenever the
// tag is absent, malformed, or out of range — a broken EXIF block must
// never fail an upload, just leave pixels untouched.
func jpegOrientation(data []byte) int {
	// JPEG: SOI (FFD8) followed by marker segments. Walk them looking for
	// the first APP1 (FFE1) segment carrying the "Exif\0\0" header; stop
	// at SOS (pixel data) or EOI.
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(data) {
		if data[i] != 0xFF {
			return 1 // not a valid marker stream
		}
		marker := data[i+1]
		switch {
		case marker == 0xFF: // fill byte (padding between markers)
			i++
			continue
		case marker == 0xDA: // SOS: the rest is compressed pixel data
			return 1
		case marker == 0x00 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) ||
			marker == 0xD8 || marker == 0xD9:
			// Standalone markers (FF00 stuffed byte, TEM, RST*, SOI, EOI):
			// no length field follows; D9 (EOI) also ends the scan. SOS
			// (0xDA) is handled above: it HAS a header, but everything
			// after it is entropy-coded data that must not be walked.
			if marker == 0xD9 {
				return 1
			}
			i += 2
			continue
		}
		segLen := int(data[i+2])<<8 | int(data[i+3])
		if segLen < 2 {
			return 1 // corrupt: length must cover itself
		}
		body := data[i+4 : min(i+2+segLen, len(data))]
		if marker == 0xE1 && len(body) >= 6 && string(body[:6]) == "Exif\x00\x00" {
			return tiffOrientation(body[6:])
		}
		i += 2 + segLen
	}
	return 1
}

// tiffOrientation parses the TIFF header embedded in an EXIF APP1 payload
// and returns the Orientation value of IFD0.
func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	if bo.Uint16(t[2:4]) != 42 { // TIFF magic
		return 1
	}
	return ifdOrientation(t, bo, int(bo.Uint32(t[4:8])))
}

// ifdOrientation scans one IFD for tag 0x0112 (Orientation, type SHORT).
func ifdOrientation(t []byte, bo binary.ByteOrder, off int) int {
	if off < 0 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off : off+2]))
	off += 2
	for e := 0; e < n; e++ {
		eoff := off + e*12
		if eoff+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[eoff:]) != 0x0112 {
			continue
		}
		if bo.Uint16(t[eoff+2:]) != 3 || bo.Uint32(t[eoff+4:]) != 1 {
			return 1 // must be exactly one SHORT value
		}
		if o := int(bo.Uint16(t[eoff+8:])); o >= 1 && o <= 8 {
			return o
		}
		return 1
	}
	return 1
}

// applyOrientation returns img with the EXIF orientation baked into the
// pixels. Orientations 5-8 swap width and height. The mapping is the
// inverse (dst→src) form of the standard EXIF transforms:
//
//	1: identity            5: transpose
//	2: horizontal flip     6: rotate 90° CW
//	3: rotate 180°         7: transverse (transpose + 180°)
//	4: vertical flip       8: rotate 90° CCW
func applyOrientation(img image.Image, orient int) image.Image {
	if orient <= 1 || orient > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	// Copy into a 0-based RGBA so the mapping loop can index pixels by
	// raw offsets (decoded JPEGs are *image.YCbCr — At() per pixel would
	// be much slower than straight Pix copies).
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(src, src.Bounds(), img, b.Min, draw.Src)

	var dst *image.RGBA
	if orient >= 5 {
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	} else {
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	db := dst.Bounds()
	for y := 0; y < db.Dy(); y++ {
		for x := 0; x < db.Dx(); x++ {
			var sx, sy int
			switch orient {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			di := dst.PixOffset(x, y)
			si := src.PixOffset(sx, sy)
			copy(dst.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return dst
}
