// Package attach classifies files by CONTENT — never by extension — and
// sanitizes filenames. Nothing here decodes pixels or audio samples:
// conversion, resizing and EXIF orientation happen in the browser before
// upload (web/src/lib/media.js), and a tool's file is stored exactly as it was
// fetched. The server's whole job is one magic-byte check per file.
//
// Two policies share that check. Uploads accept only what the browser produces
// (PNG/WebP, WebM, PDF) plus text; tools also accept any media a browser can
// render unaided, and keep every other binary as a download.
package attach

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"net/http"
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

// The mimes a file can be stored under, all decided by magic bytes. WebP is
// what the browser encodes an upload to, PNG what it falls back to where WebP
// encoding does not exist (Safari, every version, silently hands back a PNG).
// The rest only ever arrive from a tool, which stores what it fetched.
const (
	MimePNG  = "image/png"
	MimeJPEG = "image/jpeg"
	MimeWebP = "image/webp"
	MimeGIF  = "image/gif"
	MimeBMP  = "image/bmp"
	MimeICO  = "image/x-icon"

	MimeAudio = "audio/webm" // the only audio an upload can carry
	MimeMP3   = "audio/mpeg"
	MimeWAV   = "audio/wav"
	MimeOGG   = "audio/ogg"
	MimeFLAC  = "audio/flac"

	MimePDF = "application/pdf"
)

// ErrUnsupported is returned when the content cannot be accepted (HTTP 415).
var ErrUnsupported = errors.New("unsupported file type")

// ErrTooLarge is returned when the file exceeds the configured limit (HTTP 413).
var ErrTooLarge = errors.New("file too large")

// uploadMedia and toolMedia are the two classification policies: accepted mime
// → the kind it is stored under. WebM and Ogg count as audio whatever codec
// they carry, because an audio player is the only one the app has.
var (
	uploadMedia = map[string]string{
		MimePNG: KindImage, MimeWebP: KindImage,
		MimeAudio: KindFile, MimePDF: KindFile,
	}
	toolMedia = map[string]string{
		MimePNG: KindImage, MimeJPEG: KindImage, MimeWebP: KindImage,
		MimeGIF: KindImage, MimeBMP: KindImage, MimeICO: KindImage,
		MimeAudio: KindFile, MimeMP3: KindFile, MimeWAV: KindFile,
		MimeOGG: KindFile, MimeFLAC: KindFile, MimePDF: KindFile,
	}
)

// policy is one classification path: which media mimes it stores, whether an
// unrecognized binary is kept as a download or refused, and what a refusal says.
type policy struct {
	media      map[string]string
	acceptsAny bool // tools keep a zip; uploads refuse it
	accepted   string
}

var (
	// Accepted and ToolAccepted name what each policy stores, for refusals.
	Accepted     = acceptedList(uploadMedia)
	ToolAccepted = acceptedList(toolMedia)

	uploadPolicy = policy{media: uploadMedia, accepted: Accepted}
	toolPolicy   = policy{media: toolMedia, acceptsAny: true, accepted: ToolAccepted}
)

// acceptedList renders a refusal's format list from the policy's own map, so
// the prose cannot drift from it. Every policy takes text, named last.
func acceptedList(media map[string]string) string {
	names := make([]string, 0, len(media))
	for mime := range media {
		names = append(names, strings.TrimPrefix(ExtForMime(mime), "."))
	}
	sort.Strings(names)
	return strings.Join(names, ", ") + " and text"
}

// mediaExts maps a supported media or document extension to the mime it claims.
// Acceptance never reads it — the bytes decide that, always. It exists so a
// correctly named file is left alone when its stored name is reconciled with
// its content: "photo.jpeg" and "photo.jpg" both claim image/jpeg and both keep
// their spelling.
var mediaExts = map[string]string{
	"png": MimePNG, "jpg": MimeJPEG, "jpeg": MimeJPEG, "webp": MimeWebP,
	"gif": MimeGIF, "bmp": MimeBMP, "ico": MimeICO,
	"mp3": MimeMP3, "wav": MimeWAV, "flac": MimeFLAC,
	"ogg": MimeOGG, "opus": MimeOGG, "webm": MimeAudio,
	"pdf": MimePDF,
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

// mimeExts inverts both tables, so a Content-Type can be turned back into a
// display extension. Where a mime has several spellings the shortest wins,
// which keeps the pick stable across runs.
var mimeExts = func() map[string]string {
	claims := make(map[string]string, len(mediaExts)+len(textExts))
	exts := make([]string, 0, len(claims))
	for _, table := range []map[string]string{mediaExts, textExts} {
		for ext, mime := range table {
			claims[ext] = mime
			exts = append(exts, ext)
		}
	}
	sort.Slice(exts, func(i, j int) bool {
		if len(exts[i]) != len(exts[j]) {
			return len(exts[i]) < len(exts[j])
		}
		return exts[i] < exts[j]
	})
	m := make(map[string]string, len(claims))
	for _, ext := range exts {
		if _, ok := m[claims[ext]]; !ok {
			m[claims[ext]] = ext
		}
	}
	return m
}()

// ExtForMime returns the display extension (".json", dot included) for a
// Content-Type, parameters ignored, or "" when the type is not one process
// knows. Callers use it to give an extension-less download a name whose suffix
// matches the mime process will derive from its bytes.
func ExtForMime(ctype string) string {
	if i := strings.IndexByte(ctype, ';'); i >= 0 {
		ctype = ctype[:i]
	}
	if ext, ok := mimeExts[strings.ToLower(strings.TrimSpace(ctype))]; ok {
		return "." + ext
	}
	return ""
}

// Result of processing one file. Data is always the bytes verbatim — nothing is
// re-encoded — so Size == len(Data). Name is the filename to store: the
// caller's, with the extension reconciled against the sniffed mime for media.
type Result struct {
	Kind string
	Mime string // sniffed from the content
	Name string // filename to store (CleanFilename'd by the caller first)
	Data []byte
	Size int64
}

// MaxRawUploadBytes caps the bytes read for a single file, and the HTTP layer
// sizes its multipart ceiling from the same constant instead of duplicating it.
const MaxRawUploadBytes = 64 * 1024 * 1024 // 64 MiB

// MaxFilenameBytes is the stored filename budget, shared by CleanFilename and
// the callers that have to trim a name before it reaches that check.
const MaxFilenameBytes = 200

// Process classifies one UPLOADED file: the media the browser produces before
// it sends (PNG/WebP, WebM, PDF) plus text. Anything else is refused — a client
// that skipped conversion gets a 415 rather than a file nobody can preview.
func Process(filename string, data []byte, maxBytes int64) (*Result, error) {
	return process(filename, data, maxBytes, uploadPolicy)
}

// ProcessAny is the TOOL path: the same classification plus every media format
// a browser can render unaided (`toolMedia`). Any other binary is kept as a
// download rather than refused, so a file the model found on the web reaches
// the user whatever it is. Nothing is converted on either path — the bytes are
// stored exactly as they arrived.
func ProcessAny(filename string, data []byte, maxBytes int64) (*Result, error) {
	return process(filename, data, maxBytes, toolPolicy)
}

func process(filename string, data []byte, maxBytes int64, p policy) (*Result, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty file", ErrUnsupported)
	}
	// One cap for everything: nothing is re-encoded, so the stored size IS the
	// upload size.
	if int64(len(data)) > MaxRawUploadBytes || (maxBytes > 0 && int64(len(data)) > maxBytes) {
		return nil, ErrTooLarge
	}

	mime := sniffMime(data)
	if kind, ok := p.media[mime]; ok {
		return media(kind, mime, filename, data), nil
	}
	// Text before the binary fallback: an SVG is text, and stored as text it is
	// served text/plain and can never execute in the app's origin.
	if IsText(data) {
		textMime := "text/plain"
		if m, ok := textExts[extOf(filename)]; ok {
			textMime = m
		}
		return &Result{Kind: KindText, Mime: textMime, Name: filename, Data: data, Size: int64(len(data))}, nil
	}
	if !p.acceptsAny {
		return nil, fmt.Errorf("%w: only %s", ErrUnsupported, p.accepted)
	}
	return &Result{Kind: KindFile, Mime: mime, Name: filename, Data: data, Size: int64(len(data))}, nil
}

// media builds the Result for one recognized media file. The suffix is only
// touched when it disagrees with the bytes — "photo.jpeg" and "photo.jpg" both
// claim image/jpeg and both stay, while WebP bytes in a "photo.jpg" become
// "photo.webp" and an extension-less download gains the suffix its content
// implies. Text never comes through here: its mime comes FROM the extension,
// so rewriting the name would be circular.
func media(kind, mime, name string, data []byte) *Result {
	if ext := extOf(name); ext == "" || mediaExts[ext] != mime {
		name = ensureExt(name, ExtForMime(mime))
	}
	return &Result{
		Kind: kind,
		Mime: mime,
		Name: name,
		Data: data,
		Size: int64(len(data)),
	}
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

// sniffMime labels bytes by magic — never by name — and normalizes what it
// finds to the mime the app stores. http.DetectContentType covers the images,
// WAV, ID3-tagged MP3, Ogg and EBML; FLAC and a bare-frame MP3 report
// octet-stream there, so their own magic is checked by hand.
//
// Video is not supported. An EBML file is WebM only when its DocType says so —
// Matroska shares the magic and is labelled as itself — and ISO-BMFF (MP4, M4A,
// MOV) keeps the sniffer's video/mp4, which no policy accepts: a tool's file
// becomes a download, an upload is refused.
//
// A label here is served as a Content-Type only for audio and images, which
// cannot execute; every other binary is forced to octet-stream by the
// attachment handler, so a wrong guess costs a label, not a hole.
func sniffMime(data []byte) string {
	ct := http.DetectContentType(data)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "video/webm":
		// The DocType element is within the first few dozen bytes of the EBML
		// header, so a substring check is enough to tell the containers apart.
		if bytes.Contains(data[:min(len(data), 64)], []byte("matroska")) {
			return "video/x-matroska"
		}
		return MimeAudio
	case "audio/wave":
		return MimeWAV
	case "application/ogg":
		return MimeOGG
	case "application/octet-stream":
		switch {
		case bytes.HasPrefix(data, []byte("fLaC")):
			return MimeFLAC
		case isMP3Frame(data):
			return MimeMP3
		}
	}
	return ct
}

// isMP3Frame reports whether data opens with a plausible MP3 frame header: the
// 11-bit sync, a non-reserved MPEG version and layer, a non-reserved sample
// rate and a bitrate index that is not the invalid one. Bare-frame MP3s (no
// ID3 tag) are the one accepted audio format whose magic Go's sniffer misses.
// ponytail: a header is 4 bytes of weak signature — an unrelated binary that
// happens to match is labelled audio/mpeg and fails to play. Scan for a second
// frame at the offset the first one implies if that ever bites.
func isMP3Frame(data []byte) bool {
	if len(data) < 4 || data[0] != 0xff || data[1]&0xe0 != 0xe0 {
		return false
	}
	version := (data[1] >> 3) & 3 // 01 = reserved
	layer := (data[1] >> 1) & 3   // 00 = reserved
	bitrate := (data[2] >> 4) & 0xf
	rate := (data[2] >> 2) & 3 // 11 = reserved
	return version != 1 && layer != 0 && bitrate != 0xf && rate != 3
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

// AudioFormat returns the input_audio format name for a stored audio mime, or
// "" for anything else. Only WebM maps: audio a TOOL attached (mp3, wav, flac,
// …) is stored and played for the user but never sent to a specialist model,
// so the agent tool refuses it in-band rather than guessing a format the
// provider would misread.
func AudioFormat(mime string) string {
	if mime == MimeAudio {
		return "webm"
	}
	return ""
}

// SendsAsImage reports whether a stored image mime may go to a model as an
// image part. Only the two formats a browser converts an upload to qualify: a
// JPEG, GIF, BMP or ICO a tool fetched previews fine but takes the <file>
// reference path instead, and the agent tool refuses it in-band. History is
// rebuilt every turn, so a mime the provider rejects would break that chat for
// good.
func SendsAsImage(mime string) bool {
	return mime == MimePNG || mime == MimeWebP
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

// fileTag opens one <file> block. The filename is HTML-escaped (which covers
// its quotes too); id and type are Go-quoted.
func fileTag(filename, id, typ string) string {
	return fmt.Sprintf("<file name=\"%s\" id=%q type=%q>", html.EscapeString(filename), id, typ)
}

// SerializeText formats a text attachment for injection into a user message:
//
//	<file name="notes.md" id="a1b2c3d4" type="text">
//	...content...
//	</file id="a1b2c3d4">
//
// The filename is escaped and the closing tag repeats the
// attachment's unguessable random id (M2), so a stray or hostile "</file>"
// inside the file content cannot terminate the block early.
func SerializeText(filename, id, content string) string {
	return fmt.Sprintf("%s\n%s\n</file id=%q>", fileTag(filename, id, KindText), content, id)
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
	return fmt.Sprintf("%s\n%s\n</file id=%q>", fileTag(filename, id, Type(kind, mime)), body, id)
}
