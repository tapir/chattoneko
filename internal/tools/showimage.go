package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"chattoneko/internal/attach"
	"chattoneko/internal/config"
	"chattoneko/internal/mcphub"
	"chattoneko/internal/webimage"
)

// maxRawImageBytes caps the RAW fetched bytes before conversion. The real
// per-file limit is applied to the converted PNG below (same split as user
// uploads), so this is only the decompression/memory safety net.
const maxRawImageBytes = attach.MaxRawUploadBytes

// ShowImage returns the "show_image" tool bound to the given stores: the
// model passes a direct web URL of an image; we download it
// (browser-impersonating TLS so bot protection lets us through), run it
// through the same conversion pipeline as user uploads (image -> PNG,
// capped and downscaled), and store it as an attachment linked to the
// assistant message. The UI renders image attachments inline, so the user
// SEES the picture instead of a link.
//
// All user/LLM-facing text is hardcoded here — edit in place to change it.
func ShowImage(files FileStore, limits *config.Store) Tool {
	return Tool{
		Name: "show_image",
		Description: "Fetch an image from a direct web URL and display it inline in the chat " +
			"as part of your reply. Use it whenever the user should SEE a picture you found " +
			"or were given a link to (photo, screenshot, meme, chart, ...). Only works for " +
			"direct image URLs — ones that return the image file itself, not HTML pages. " +
			"Supported formats: PNG, JPEG, GIF (first frame), WebP — SVG is not supported. " +
			"After a successful call the image is already visible to the user: do not repeat " +
			"the URL as a link in your reply.",
		Schema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {
				"type": "string",
				"description": "Direct http(s) URL of the image file."
			},
			"filename": {
				"type": "string",
				"description": "Optional display filename; derived from the URL when omitted."
			}
		},
		"required": ["url"],
		"additionalProperties": false
	}`),
		DefaultEnabled: true,
		Handler: func(ctx context.Context, argsJSON string, meta mcphub.CallMeta) (string, error) {
			return showImage(ctx, argsJSON, meta, files, limits)
		},
	}
}

func showImage(ctx context.Context, argsJSON string, meta mcphub.CallMeta, files FileStore, limits *config.Store) (string, error) {
	if files == nil {
		return "", errors.New("file storage is not available")
	}
	var args struct {
		URL      string `json:"url"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments JSON: %v", err)
	}
	if strings.TrimSpace(args.URL) == "" {
		return "", errors.New("url is required")
	}
	if meta.ChatID == "" || meta.MessageID == "" {
		return "", errors.New("no chat context for this call")
	}

	// Per-file stored-size cap comes from the live upload limit (same as
	// user uploads); the fetch itself is capped at the raw safety net.
	limit := int64(config.DefaultUploadMaxFileBytes)
	if limits != nil {
		limit = limits.Get().Limits.UploadMaxFileBytes
	}
	data, finalURL, contentType, err := webimage.Fetch(ctx, args.URL, maxRawImageBytes)
	if err != nil {
		if errors.Is(err, webimage.ErrTooLarge) {
			return "", fmt.Errorf("the image is larger than the %s raw fetch limit", humanSize(maxRawImageBytes))
		}
		return "", err
	}

	name := imageFilename(args.Filename, finalURL)
	// Same content rules as user uploads: sniffed + re-encoded to PNG,
	// the cap enforced on the converted PNG with downscaling fallbacks.
	res, err := attach.Process(name, data, limit)
	if errors.Is(err, attach.ErrTooLarge) {
		return "", fmt.Errorf("the image exceeds the %s size limit even after downscaling", humanSize(limit))
	}
	if err != nil {
		return "", fmt.Errorf("the URL did not return a valid image%s: %v", ctypeHint(contentType), err)
	}
	if res.Kind != attach.KindImage {
		return "", fmt.Errorf("the URL did not return an image%s (text/HTML content is not supported)", ctypeHint(contentType))
	}
	m, err := files.CreateLinkedAttachment(ctx, meta.ChatID, meta.MessageID, name, res.Kind, res.Mime, res.Size, res.Data)
	if err != nil {
		return "", fmt.Errorf("store image: %v", err)
	}
	dims := ""
	if w, h := pngDimensions(res.Data); w > 0 && h > 0 {
		dims = fmt.Sprintf("%dx%d, ", w, h)
	}
	return fmt.Sprintf("Image %q (%s%s) from %s is now displayed inline in the chat. It is already visible to the user — don't repeat the URL or the image in your reply.",
		m.Filename, dims, humanSize(m.Size), finalURL), nil
}

// imageFilename picks the display name: an explicit model-provided name
// (sanitized) when given, else the last path segment of the FINAL URL
// (after redirects). The stored bytes are always a re-encoded PNG, so the
// extension is forced to .png either way.
func imageFilename(explicit string, final *url.URL) string {
	name := ""
	if explicit != "" {
		if n, err := attach.CleanFilename(explicit); err == nil {
			name = n
		}
	}
	if name == "" && final != nil {
		if seg := path.Base(final.Path); seg != "" && seg != "." && seg != "/" {
			name = seg
		}
	}
	if name == "" {
		name = "image"
	}
	name = ensurePNGExt(name)
	// Same 200-byte cap as attach.CleanFilename; long CDN paths must not
	// blow the filename budget. Back off the cut one byte at a time until it
	// is valid UTF-8 again so a multibyte name is never split mid-rune.
	if len(name) > 200 {
		cut := name[:200-len(".png")]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		name = cut + ".png"
	}
	return name
}

// ensurePNGExt rewrites name's extension to .png (appended when missing).
func ensurePNGExt(name string) string {
	if strings.HasSuffix(strings.ToLower(name), ".png") {
		return name
	}
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	return name + ".png"
}

// pngDimensions reads the PNG header for the stored image's dimensions.
func pngDimensions(data []byte) (int, int) {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// ctypeHint names the Content-Type the server actually sent, so failures
// like "the server sent image/avif" (a format our pipeline can't decode) or
// "text/html" (a bot interstitial) are self-explanatory to the model.
func ctypeHint(ctype string) string {
	ctype = strings.ToLower(strings.TrimSpace(ctype))
	if ctype == "" {
		return ""
	}
	if i := strings.IndexByte(ctype, ';'); i >= 0 {
		ctype = strings.TrimSpace(ctype[:i])
	}
	return " (the server sent " + ctype + ")"
}
