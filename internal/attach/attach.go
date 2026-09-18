// Package attach decides what a file is and sanitizes filenames. Nothing here
// decodes pixels, parses a container or reads a magic byte: media is recognised
// by its EXTENSION alone, the name is trusted, and whatever it claims is
// converted to the one shape the database stores (internal/media), so a file
// that lies about its suffix is rejected by the conversion instead. Anything
// without a media or PDF suffix has to pass the text heuristic.
//
// Classify is the upload path and refuses what it cannot place; ClassifyAny is
// the tool path and keeps such a file as a download, so whatever a model found
// on the web still reaches the user.
package attach

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Kinds of attachments.
const (
	KindImage = "image"
	KindText  = "text"
	// KindFile is any other binary, stored verbatim: audio (whose mime the
	// inline player needs), PDF, and every download-only file a tool fetches.
	KindFile = "file"
)

// The mimes a file can be stored under. Every picture becomes a PNG and every
// recording a mono MP3 (internal/media), so those two plus PDF are the whole set
// of recognized content; anything else a tool hands over is a download.
const (
	MimePNG = "image/png"
	MimeMP3 = "audio/mpeg"
	MimePDF = "application/pdf"
	// MimeBinary is what an unrecognized download is stored as. The attachment
	// handler serves every non-audio binary as this anyway, so naming a format
	// nothing sniffs any more would only decorate the <file> block.
	MimeBinary = "application/octet-stream"
)

// ErrUnsupported is returned when the content cannot be accepted (HTTP 415).
var ErrUnsupported = errors.New("unsupported file type")

// ErrTooLarge is returned when the file exceeds the configured limit (HTTP 413).
var ErrTooLarge = errors.New("file too large")

// The extensions a file may carry to be recognised as media. tiff is absent
// because the ffmpeg build has no TIFF decoder, ico because nothing decodes it
// at all: both are refused here and rejected by ffmpeg there, and one rule is
// enough.
var (
	imageExts = extSet("png", "bmp", "tga", "jpg", "jpeg", "gif", "webp")
	// A video container counts as audio: ffmpeg decodes no picture, so what
	// comes out is the soundtrack.
	audioExts = extSet("wav", "mp3", "ogg", "oga", "opus", "webm", "mkv", "mov",
		"flac", "alac", "m4a", "m4b", "mp4", "aac")
)

func extSet(exts ...string) map[string]bool {
	m := make(map[string]bool, len(exts))
	for _, e := range exts {
		m[e] = true
	}
	return m
}

// The conversions Classify can ask the caller to run before storing.
const (
	ConvertNone  = ""
	ConvertImage = "image"
	ConvertAudio = "audio"
)

// Accepted names what the extension list holds, for a refusal. Rendered from the
// sets themselves, so the prose cannot drift from them; text is taken too, named
// last.
var Accepted = acceptedExts()

func acceptedExts() string {
	names := make([]string, 0, len(imageExts)+len(audioExts)+1)
	for ext := range imageExts {
		names = append(names, ext)
	}
	for ext := range audioExts {
		names = append(names, ext)
	}
	names = append(names, "pdf")
	sort.Strings(names)
	return strings.Join(names, ", ") + " and text"
}

// textExts hints the mime for text; anything missing is text/plain. Acceptance
// is content-based (IsText), not extension-based.
var textExts = map[string]string{
	"md": "text/markdown", "markdown": "text/markdown",
	"yaml": "text/yaml", "yml": "text/yaml",
	"json": "application/json", "jsonl": "application/jsonl", "ndjson": "application/jsonl",
	"xml": "application/xml", "csv": "text/csv", "tsv": "text/tab-separated-values",
	"html": "text/html", "css": "text/css",
}

// mimeExts names the extension a Content-Type implies, for a download whose URL
// carries no suffix. The media half is the one that matters: the suffix is what
// Classify reads, so an extension-less "image/jpeg" body only becomes a picture
// because this hands it a ".jpg". Servers spell formats differently from
// filenames (audio/mpeg, audio/x-wav, video/x-matroska), so the mapping is
// written out rather than derived from the extension sets.
var mimeExts = map[string]string{
	"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp",
	"image/gif": "gif", "image/bmp": "bmp", "image/x-tga": "tga",
	"audio/mpeg": "mp3", "audio/wav": "wav", "audio/x-wav": "wav", "audio/wave": "wav",
	"audio/ogg": "ogg", "audio/opus": "opus", "audio/flac": "flac", "audio/aac": "aac",
	"audio/mp4": "m4a", "audio/x-m4a": "m4a", "audio/webm": "webm",
	"video/mp4": "mp4", "video/webm": "webm", "video/quicktime": "mov",
	"video/x-matroska": "mkv",
	"application/pdf":  "pdf",
	"text/markdown":    "md", "text/yaml": "yaml", "application/json": "json",
	"application/jsonl": "jsonl", "application/xml": "xml", "text/csv": "csv",
	"text/tab-separated-values": "tsv", "text/html": "html", "text/css": "css",
}

// ExtForMime returns the display extension (".json", dot included) for a
// Content-Type, parameters ignored, or "" when the type is not one this knows.
// create_file uses it to give an extension-less download a name whose suffix
// says what the server served.
func ExtForMime(ctype string) string {
	if i := strings.IndexByte(ctype, ';'); i >= 0 {
		ctype = ctype[:i]
	}
	if ext, ok := mimeExts[strings.ToLower(strings.TrimSpace(ctype))]; ok {
		return "." + ext
	}
	return ""
}

// MaxRawUploadBytes caps the bytes read for a single file, and the HTTP layer
// sizes its multipart ceiling from the same constant instead of duplicating it.
const MaxRawUploadBytes = 64 * 1024 * 1024 // 64 MiB

// MaxFilenameBytes is the stored filename budget, shared by CleanFilename and
// the callers that have to trim a name before it reaches that check.
const MaxFilenameBytes = 200

// File is Classify's verdict on one file: the kind, mime and name it is STORED
// under — already the converted ones — and the conversion the caller runs on its
// bytes first. It carries no bytes: what is stored is what comes out of
// internal/media, or the caller's own data when Convert is ConvertNone.
type File struct {
	Kind    string
	Mime    string
	Name    string
	Ext     string // the input's own suffix, dot included: media names its temp file after it
	Convert string // ConvertNone, ConvertImage or ConvertAudio
}

// Classify decides what one file is and what has to happen to its bytes before
// they may be stored. Media is recognised by EXTENSION alone, and the mime and
// name it returns are the ones the conversion produces, so an image is always
// stored as a PNG under a .png name and a recording as an MP3 under .mp3.
// Everything else has to be text; anything left is refused with ErrUnsupported.
func Classify(filename string, data []byte, maxBytes int64) (*File, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty file", ErrUnsupported)
	}
	if int64(len(data)) > MaxRawUploadBytes {
		return nil, ErrTooLarge
	}
	ext := extOf(filename)
	u := &File{Name: filename}
	switch {
	case imageExts[ext]:
		u.Kind, u.Mime, u.Convert = KindImage, MimePNG, ConvertImage
		u.Name = ensureExt(filename, ".png")
	case audioExts[ext]:
		u.Kind, u.Mime, u.Convert = KindFile, MimeMP3, ConvertAudio
		u.Name = ensureExt(filename, ".mp3")
	case ext == "pdf":
		// Stored as picked: nothing converts a PDF, and the viewer needs the
		// original bytes.
		u.Kind, u.Mime = KindFile, MimePDF
	case IsText(data):
		// Text before the refusal: an SVG is text, and stored as text it is
		// served text/plain and can never execute in the app's origin.
		u.Kind, u.Mime = KindText, textExts[ext]
		if u.Mime == "" {
			u.Mime = "text/plain"
		}
	default:
		return nil, fmt.Errorf("%w: only %s", ErrUnsupported, Accepted)
	}
	if u.Convert != ConvertNone {
		// The suffix only exists so ffmpeg can probe the input: TGA has no magic
		// bytes, so the name is what reaches its decoder.
		u.Ext = "." + ext
	} else if maxBytes > 0 && int64(len(data)) > maxBytes {
		// The stored cap applies to what lands in the database: now, for the two
		// kinds stored verbatim, and to the conversion's output for media.
		return nil, ErrTooLarge
	}
	return u, nil
}

// ClassifyAny is Classify for a TOOL's file: the same extension rules and the
// same conversions, except that a file which is neither on the list nor text is
// kept as a download instead of refused, so whatever the model found on the web
// reaches the user — an unrecognized one simply has no preview.
func ClassifyAny(filename string, data []byte, maxBytes int64) (*File, error) {
	f, err := Classify(filename, data, maxBytes)
	if err == nil || !errors.Is(err, ErrUnsupported) {
		return f, err
	}
	// An empty file is nobody's to keep, and a download is stored verbatim, so
	// it carries the stored cap Classify skipped on its way to the refusal.
	switch {
	case len(data) == 0:
		return nil, err
	case maxBytes > 0 && int64(len(data)) > maxBytes:
		return nil, ErrTooLarge
	}
	return &File{Kind: KindFile, Mime: MimeBinary, Name: filename}, nil
}

// ensureExt rewrites name's extension to ext (appended when missing).
func ensureExt(name, ext string) string {
	if ext == "" || strings.HasSuffix(strings.ToLower(name), ext) {
		return name
	}
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	return name + ext
}

func extOf(filename string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(filename)), ".")
}

// CleanFilename validates and normalizes an uploaded or tool-provided
// filename: a plain name of bounded length, no directories, no control
// characters. Names end up in the DB, in LLM prompts and in download
// headers, so every call site (user uploads, create_file, fetch) shares this
// one check.
func CleanFilename(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", errors.New("filename is required")
	case len(name) > MaxFilenameBytes:
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

// IsText reports whether non-empty data is acceptable as plain text:
// valid UTF-8, no NUL bytes, >=95% printable/whitespace runes.
func IsText(data []byte) bool {
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

// SendsAsImage reports whether a stored image mime may go to a model as an
// image part: PNG only, the one image mime the conversion every picture goes
// through produces (internal/media). A row stored before that was true previews
// in the browser but goes out through the <file> reference path, and the vision
// tool refuses it in-band. History is rebuilt every turn, so a mime the provider
// rejects would break that chat permanently.
func SendsAsImage(mime string) bool {
	return mime == MimePNG
}

// Type names an attachment for the <file> block the model reads. The kind is
// already the answer for images and text; inside the binary kind the stored
// mime picks "audio" or "document" (PDF), and anything else binary — a zip the
// tools fetched — stays "file".
func Type(kind, mime string) string {
	switch kind {
	case KindImage:
		return "image"
	case KindText:
		return "text"
	}
	switch {
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case mime == MimePDF:
		return "document"
	}
	return KindFile
}

// FileTag opens one <file> block. The filename is HTML-escaped (which covers
// its quotes too); id and type are Go-quoted. Exported for the creating tools'
// results, so a file the model attached carries its id in the same shape.
func FileTag(filename, id, typ string) string {
	return fmt.Sprintf("<file name=\"%s\" id=%q type=%q>", html.EscapeString(filename), id, typ)
}

// SerializeText formats a text attachment for injection into a user message:
//
//	<file name="notes.md" id="a1b2c3d4" type="text">
//	...content...
//	</file id="a1b2c3d4">
//
// The filename is escaped and the closing tag repeats the attachment's
// unguessable random id, so a stray or hostile "</file>" inside the file
// content cannot terminate the block early.
func SerializeText(filename, id, content string) string {
	return fmt.Sprintf("%s\n%s\n</file id=%q>", FileTag(filename, id, KindText), content, id)
}

// SerializeRef is SerializeText for a file the model cannot read: the same
// envelope, but the body points at the stored bytes instead of carrying them.
// Used for binary attachments (audio, PDF) and for images on a model whose
// metadata has no image input, so the id survives in the prompt for a later
// tool call to pick up.
func SerializeRef(filename, id, kind, mime string, size int64) string {
	body := fmt.Sprintf(
		"Content not included: the current model cannot read %s. The file's %d bytes are stored in the database under attachment id %s.",
		mime, size, id)
	return fmt.Sprintf("%s\n%s\n</file id=%q>", FileTag(filename, id, Type(kind, mime)), body, id)
}
