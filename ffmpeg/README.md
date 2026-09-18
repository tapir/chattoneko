# Slim ffmpeg

Extremely slimmed-down static ffmpeg build (musl, SIMD, explicit component
allowlist): 2.8 MB, no dynamic loader. Images and audio only —
no video codecs, no network, no devices, no hardware acceleration, no ffprobe.

## Supported

| | |
|---|---|
| **Image decode** | JPEG, PNG/APNG, WebP, GIF, TGA, BMP |
| **Audio decode** | WAV/PCM, MP3, Opus, AAC, FLAC, ALAC, Vorbis |
| **Containers** | wav, mp3, aac, flac, ogg, webm/mkv, mp4/m4a/m4b/mov |
| **Encode** | PNG (optionally palette-quantized), MP3 |

Anything else exits non-zero with `no decoder found` or `Invalid data found` and writes
nothing.

## Build

```bash
sudo pacman -S musl nasm
./build.sh     # downloads zlib, lame, ffmpeg; needs patches/ and eh_frame.ld next to it
./verify.sh    # generates fixtures with the system ffmpeg, checks everything
```

Production invocations in `cli.md`.