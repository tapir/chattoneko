# Slim jq

Static jq build (musl, `-Oz`, LTO): 773 KB, no dynamic loader. Upstream's
`jq-linux-amd64` release is the same 1.8.2 at 2.2 MB.

No runtime feature is cut: `-Oz`, LTO, `--gc-sections` and `strip` account for
the whole difference. oniguruma is in, so `test`, `match`, `capture`, `sub` and
`gsub` work; decNumber is in, so a 19-digit ID survives being read and printed
instead of being rounded to the nearest double — arithmetic is IEEE754 double
either way, exact only within ±2^53. `verify.sh` pins both.

## Missing anyway

| | |
|---|---|
| **locales** | musl has none, so `strflocaltime` always renders C/English. This is why upstream's `shtest` cannot pass here (6 of 7 do). |
| **docs, maintainer mode, valgrind, shared libs** | build-time only. |

The release tarball vendors oniguruma and decNumber itself, so there is one
download and no `deps/` stage like `ffmpeg/` needs for zlib and lame.

## Build

```bash
sudo pacman -S musl
./build.sh     # ~10 s; downloads jq, writes out/bin/jq
./verify.sh    # asserts identity, core filters, regex, decNumber, bad input
```

`CC=gcc` builds it on Alpine (there gcc *is* musl). Nothing here needs the
`eh_frame.ld` dance or the patches `ffmpeg/` carries.

The `jq` tool runs it as `$CHATTO_JQ` when set, else `jq` on `PATH`
(`internal/tools.Binary`, the same lookup `internal/media` does for ffmpeg).
The image puts it at `/usr/local/bin/jq`.
