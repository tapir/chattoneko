# ChattoNeko

![ChattoNeko](ss.png)

Your own cute AI assistant cat, self-hosted.

*Chatto* is the Japanese pronunciation of the English word "chat"; *neko* is Japanese for "cat." ChattoNeko is a chat client for OpenAI-compatible APIs (it plays best with OpenRouter, which exposes extra model metadata, but it works fully with any other OpenAI-compatible provider). You run it on your own server. It is made for personal, self-hosted use, not as a SaaS product: no accounts, no multi-tenancy, no billing.

It is extremely small. Everything is a single static Go binary with the web UI embedded, about **8 MB** after UPX packing. The Docker image is roughly **14 MB** and the Android APK is about **10 MB**. Small as it is, it has what you'd expect from a chat app: streaming, reasoning display, attachments, tools, history, search, per-chat settings, optional login, plus a few unique features.

## What it does

- Chat with any model through an OpenAI-compatible API (chat completions, transcriptions, speech).
- Nothing is hidden. You can access tool calls, chain of thought, etc. in collapsed UI compartments.
- Send images, text files, audio, and PDFs as attachments. All images and audio are always converted to `JPG` and `MP3` as they are the smallest size, globally accepted formats by every LLM model, so what lands in the chat is always the same shape whatever you pick. Accepted: `png bmp tga jpg gif webp` images, `wav mp3 ogg oga opus flac alac m4a m4b aac` audio (a video container — `webm mkv mov mp4` — keeps its soundtrack), `pdf`, and any `text` file. A file the selected model can't read is still kept and mentioned in the message by its stored id, so switching models never loses it and the model can hand that id to a specialist model that *can* read it (the `vision`, `document`, and `transcribe` tools), once you flag one in the settings.
- The model can call tools, plus any MCP server (HTTP-only) you add.
- The model can hand files back to you as download links, or show images, PDFs, and audio inline in the chat window, together with previews whenever possible.
- Regenerate a reply, or edit an earlier message and take the conversation elsewhere.
- Each chat remembers its own model, reasoning effort, and tools.
- Globally editable system prompt.
- Optional single-user login.

## Run it

Docker, the easy way:

```bash
docker pull ghcr.io/tapir/chattoneko:latest
docker run -d --name chattoneko \
  -p 8080:8080 \
  -v chattoneko-data:/var/lib/chattoneko \
  ghcr.io/tapir/chattoneko
```

If you prefer not to use Docker images, fully static server and web client binaries can be built with `make build`. `ffmpeg` is required: the server refuses to start without it, because every uploaded picture and recording is converted through it. `jq` is optional — without it everything else runs but the `jq` tool is greyed out. The Docker image takes care of both for you with incredibly slim builds: 2.8 MB of `ffmpeg` and 0.8 MB of `jq`.

The Android APK is built with `make mobile-apk`.

## Configuration

There is no config file. On first start the database is seeded with defaults and the server comes up. All configuration happens after your first visit to the page: enter your provider's base URL and API key, pick your models, done. Settings are stored in the database and apply live, no restart needed.

If your provider is OpenRouter, much of this is automated. ChattoNeko reads the provider's `/models` endpoint, and OpenRouter reports everything it uses: context length, input modalities, supported reasoning efforts, and the default effort. Pick a model and its capabilities are filled in for you, which is also how the app knows whether a model can see images or read PDFs. Other OpenAI-compatible providers report less; missing values fall back to sensible defaults and can be edited by hand.

### Recommended Configuration

This is my personal config trying to achieve cost-efficiency with good performance. It is quite capable compared to Gemini, ChatGPT, and Claude for daily assistance. If you're having multiple conversations a day, it's still cheaper than their lowest-tier monthly subscriptions with a service like OpenRouter.

| Purpose | Model | Default Reasoning | Info |
| --- | --- | --- | --- |
| Chat | DeepSeek 4.1 Flash | high | It's cheap and perfectly capable for everyday assistance; I rarely look for any other model |
| Chat | Qwen 3.8 2.4T A95B  | xhigh | Relentless thinker. Use only if you need a detailed report or analysis on a subject. It's not super cheap but 10x cheaper than comparable models like Opus, etc...  |
| Task | Gemma 4 31B | low | Very fast, cheap |
| Vision | Gemma 4 31B | low | Very good vision capability, cheap |
| File (Document) | Muse Spark 1.2 | low | OKish price, top-notch PDF performance |
| Transcription | Whisper Large 3 | --- | Industry standard |
| Speech | Kokoro 82M | --- | Any `/audio/speech` model works |

I also use Exa.ai's web search tool. I usually disable its fetch tool since we have an integrated fetcher that is quite capable.

### Environment variables

| Variable | Meaning |
| --- | --- |
| `CHATTO_USERNAME` | Login name. Set both this and the password to require a sign-in; if either is missing there is no auth at all. |
| `CHATTO_PASSWORD` | Login password, used as-is. Nothing about the login is written to the database; changing it means restarting. |
| `CHATTO_LOCATION_STRING` | Free-form location, e.g. `Berlin, Germany`. Appended to the `time` tool's result so agents know where you are. Read once at startup. |
| `CHATTO_FFMPEG` | The ffmpeg that converts uploaded pictures and recordings. Required — the server exits at startup when it cannot be found. Defaults to `ffmpeg` on your PATH; the Docker image ships its own at `/usr/local/bin/ffmpeg`. Read once at startup. |
| `CHATTO_JQ` | The jq the `jq` tool runs. Optional — when it is missing the tool is never offered and its rows are greyed out. Defaults to `jq` on your PATH; the Docker image ships its own at `/usr/local/bin/jq`. Read once at startup. |

### Command-line flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-db` | `chatto.db` next to the executable | SQLite database file. |
| `-listen` | `:8080` | HTTP listen address, fixed for the lifetime of the process. |
| `-debug` | off | Debug logging. |

## Tools

Eight integrated tools, each toggleable per chat and globally in settings:

- `time` — the server's current date, time, and timezone, plus your location when `CHATTO_LOCATION_STRING` is set. Lets the model ground "tomorrow," "next Friday," or "near me."
- `jq` — runs a jq filter over a JSON value the model supplies, fed in on stdin, and returns what the filter produces as compact JSON. Exact arithmetic, genuine regular expressions (`test`/`match`/`capture`), and querying, reshaping, sorting, grouping or counting structured data in one filter instead of a whole program.
- `attach` — shows you a file in the conversation window: an image appears inline, a text file opens as a preview, audio shows up as a player, PDFs get an inline preview, and anything else downloads when you click it. The model either writes the file (text as text, binary as base64) or points at files already stored in the chat — the ones `fetch` downloaded — and this puts them on its reply.
- `speak` — reads text aloud and puts the recording in the conversation as an audio player, the same way `attach` puts a file there. The model either writes the words out or points at a text file already stored in the chat, which is read as written. Any message can also be read aloud straight from its action row, which streams the audio without storing it.
- `fetch` — reads a URL and returns its text to the model: a page, a JSON API, anything textual, truncated and labelled when very large. A body that isn't text — an image, a PDF, an archive — is stored as a file in the chat and its id comes back instead of bytes the model can't read, so the model can then ask `vision` about it, transcribe it, or hand it to you with `attach`. Uses `tls-client` to impersonate a real Chrome — TLS JA3/JA4 fingerprint, HTTP/2 framing and header order included.
- `vision`, `document`, `transcribe` — ask a specialist model about an attachment the chat model can't handle itself and bring the answer back: an image, a PDF, or the recording's own text (the transcription model is reached through the audio transcription endpoint, so what comes back is a transcript the chat model then works from). You flag the vision and document models on their cards in settings; the transcription model is a single id in the **Audio** section, since there is only ever one. Each specialist is offered only to a chat model that actually lacks that input — a model that sees images is never offered `vision`, and its row in the chat's Tools panel is greyed out and inert while that model is selected. The same row is greyed out while its model isn't designated at all, but the settings **Tool defaults** list always shows it: that's where you set the toggle, before or after picking the model. Each exchange is one question and one answer: the specialist gets the file and the question, never your conversation — the transcription specialist gets the file only, since that endpoint takes no question.

Beyond those you can add MCP servers in settings: an HTTP (streamable) endpoint with optional headers. Their tools join the catalog as soon as you save, no restart. Each server card carries a **Fetch** button at the top right, next to the delete icon, that dials it and lists its tools right there with their own on/off defaults — so the global **Tool defaults** list stays purely the integrated tools above. Every MCP tool row also has an optional **title** box: the friendly label the chat shows while that tool runs (`web_exa_search` → "Searching web…") instead of the raw name. Leave it empty to keep the tool's own title, or the name when there is none — the integrated tools have their titles built in.

## FAQ

**Why no multi-user?**

It is small and meant for self-hosted personal use. Accounts, permissions, quotas, and isolation are most of the complexity in a chat product, and none of the benefit when the one user is you.

**What if I want my family to use it?**

Run one instance per person. The image is 13 MB and idles at almost nothing, so ten of them on a single commodity server is not a thought you need to have twice. Everybody gets a private instance with their own database, their own models, and their own API key.

**Will there ever be image or video generation?**

No. Providers share no shape for either. OpenAI's `/images/generations` is text-to-image only, with img2img on a separate `/images/edits` route; OpenRouter serves neither and uses `POST /api/v1/images` with its own parameter names. ChattoNeko sticks to routes every OpenAI-compatible provider implements the same way, and video has no standard route at all.

**iOS?**

Open to contributions. The mobile app is Capacitor plus a WebView around the same web UI, so an iOS build should not be much work. The blocker is hardware: there's no Mac or iPhone here, so I can't develop or test.

## Disclaimer

All cat pictures are from [magnific.com](https://magnific.com).
