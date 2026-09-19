package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"chattoneko/internal/attach"
	"chattoneko/internal/config"
	"chattoneko/internal/llm"
	"chattoneko/internal/mcphub"
	"chattoneko/internal/media"
)

// Speak returns the "speak" tool: the model hands the user a recording of text
// read aloud. It is attach for one fixed kind of content — the words go to
// /audio/speech, and the mp3 that comes back is stored and linked to the
// assistant message being generated, so a successful call always means the user
// can hear it and no tool result has to name another tool.
func Speak(files fileStore, cfgs *config.Store) tool {
	s := &speakTool{files: files, cfgs: cfgs, cache: llm.NewCache(cfgs)}
	return tool{
		Name: "speak",
		Description: "Read text aloud for the user: the recording appears in the chat on your reply as " +
			"soon as this call succeeds. Pass the exact words to speak in `text` — they are spoken " +
			"verbatim, so write them out as speech: no markdown, no stage directions, and nothing that " +
			"only makes sense on a page. Use it when the user asks to hear something rather than read " +
			"it. Describe the recording in your reply instead of repeating the words.",
		Schema: json.RawMessage(`{
	"type": "object",
	"properties": {
		"text": {
			"type": "string",
			"description": "The exact words to speak."
		}
	},
	"required": ["text"],
	"additionalProperties": false
}`),
		DefaultEnabled: true,
		Title:          "Speaking…",
		Model:          func(m config.ModelsConfig) string { return m.DefaultSpeechModel },
		// A full provider round trip over the whole text, not local work, so
		// the integrated tools' 30s default is far too tight (as for the
		// specialists).
		Timeout: specialistTimeout,
		Handler: s.call,
	}
}

type speakTool struct {
	files fileStore
	cfgs  *config.Store
	cache *llm.Cache
}

func (s *speakTool) call(ctx context.Context, argsJSON string, meta mcphub.CallMeta) (string, error) {
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments JSON: %v", err)
	}
	if strings.TrimSpace(args.Text) == "" {
		return "", errors.New("text is required")
	}
	// The recording is shown on the message being generated, so both
	// coordinates are needed before any work happens.
	if meta.ChatID == "" || meta.MessageID == "" {
		return "", errors.New("no chat context for this call")
	}

	cfg := s.cfgs.Get()
	if cfg.Models.DefaultSpeechModel == "" {
		// Named by what it does rather than by the settings row, because this
		// text is what the chat model relays to the user.
		return "", errors.New("no model is designated for speech in the server settings, so nothing can be read aloud")
	}
	cli := s.cache.Get(ctx, cfg.Models.DefaultSpeechModel)
	if cli == nil {
		return "", errors.New("the provider is not configured yet")
	}
	data, err := cli.Speak(ctx, args.Text, cfg.Models.SpeechVoice)
	if err != nil {
		return "", fmt.Errorf("speech model: %v", err)
	}

	// The same classification, conversion and size cap as attach and an
	// upload, so the recording lands in the one shape every other one is stored
	// in: a mono MP3. mp3 is what we ask the provider for, but ffmpeg probes
	// audio by content rather than by the name, so whatever container it
	// actually sent is normalized the same.
	limit := cfg.Limits.UploadMaxFileBytes
	res, err := attach.ClassifyAny("speech.mp3", data, limit)
	if err == nil {
		data, err = media.Prepare(ctx, res, data, false, limit)
	}
	switch {
	case errors.Is(err, attach.ErrTooLarge):
		return "", fmt.Errorf("the recording exceeds the %s file size limit", humanSize(limit))
	case err != nil:
		return "", fmt.Errorf("the speech model returned audio that could not be stored: %v", err)
	}
	m, err := s.files.CreateAttachment(ctx, meta.ChatID, res.Name, res.Kind, res.Mime, int64(len(data)), data)
	if err != nil {
		return "", fmt.Errorf("store audio: %v", err)
	}
	if err := s.files.LinkAttachmentToMessage(ctx, m.ID, meta.MessageID, meta.ChatID); err != nil {
		return "", fmt.Errorf("the recording was stored but could not be shown: %v", err)
	}
	return fmt.Sprintf("%s\nSpoken as %q (%s) — it now plays on your reply. Say what it is instead of repeating the words.\n</file id=%q>",
		attach.FileTag(m.Filename, m.ID, attach.Type(m.Kind, m.Mime)), m.Filename, humanSize(m.Size), m.ID), nil
}
