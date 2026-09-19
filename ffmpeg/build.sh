#!/usr/bin/env bash
# Fully static ffmpeg: image decode, PNG encode, audio decode, MP3 encode.
set -euo pipefail

ZLIB_VERSION=1.3.2
LAME_VERSION=3.100
FFMPEG_VERSION=9.0.2

ROOT=$(cd -- "$(dirname -- "$0")" && pwd)
SRC=$ROOT/src
DEPS=$ROOT/deps
OUT=$ROOT/out
JOBS=$(nproc)
# Static musl: ~25% smaller than static glibc, and no SIMD-memcpy variants.
# Alpine's gcc IS musl, so the image build passes CC=gcc.
CC=${CC:-musl-gcc}
# -fno-pie: Arch gcc defaults to PIE, so ffmpeg's configure would otherwise add -fPIC to a static non-PIE binary
# every configure below needs LDFLAGS=-no-pie too, or its test links reject the -fno-pie objects
SIZE_CFLAGS='-Oz -fno-pie -fno-ident -fno-unwind-tables -fno-asynchronous-unwind-tables -ffunction-sections -fdata-sections'

mkdir -p "$SRC" "$DEPS" "$OUT"

fetch() { # <dest-filename> <url> - publishes atomically, so an interrupted download can never poison the cache
  [ -f "$SRC/$1" ] || { timeout 600 curl -fL --retry 3 -o "$SRC/$1.part" "$2" && mv "$SRC/$1.part" "$SRC/$1"; }
}
untar() { # <filename> [patches...] - patches are applied once, on a fresh extraction
  [ -d "$SRC/${1%.tar*}" ] && return
  tar xf "$SRC/$1" -C "$SRC"
  for p in "${@:2}"; do patch -d "$SRC/${1%.tar*}" -p1 < "$p"; done
}

fetch "zlib-$ZLIB_VERSION.tar.gz" \
  "https://zlib.net/zlib-$ZLIB_VERSION.tar.gz"
fetch "lame-$LAME_VERSION.tar.gz" \
  "https://sourceforge.net/projects/lame/files/lame/$LAME_VERSION/lame-$LAME_VERSION.tar.gz/download"
fetch "ffmpeg-$FFMPEG_VERSION.tar.xz" \
  "https://ffmpeg.org/releases/ffmpeg-$FFMPEG_VERSION.tar.xz"
untar "zlib-$ZLIB_VERSION.tar.gz"
untar "lame-$LAME_VERSION.tar.gz"
untar "ffmpeg-$FFMPEG_VERSION.tar.xz" "$ROOT"/patches/ffmpeg-*.patch

# ---------------------------------------------------------------- deps (static)
if [ ! -f "$DEPS/lib/libz.a" ]; then
  ( cd "$SRC/zlib-$ZLIB_VERSION"
    CC="$CC" CFLAGS="$SIZE_CFLAGS -DDYNAMIC_CRC_TABLE" LDFLAGS=-no-pie \
      ./configure --static --prefix="$DEPS"
    make -j"$JOBS" && make install )
fi

if [ ! -f "$DEPS/lib/libmp3lame.a" ]; then
  ( cd "$SRC/lame-$LAME_VERSION"
    CC="$CC" CFLAGS="$SIZE_CFLAGS" LDFLAGS=-no-pie ./configure --prefix="$DEPS" \
      --enable-static --disable-shared --disable-frontend --disable-decoder --disable-analyzer-hooks
    make -j"$JOBS" && make install )
fi

# ------------------------------------------------------------------- components
# ffmpeg has no native MP3 encoder: libmp3lame is the only one that exists.
# Decoding a PNG needs zlib for its inflate wrapper.
DECODERS='
  mjpeg,png,webp,webp_anim,gif,targa,bmp,
  aac,alac,flac,mp3,opus,vorbis,
  pcm_s16le,pcm_s24le,pcm_s32le,pcm_f32le,pcm_f64le,pcm_u8,pcm_mulaw,pcm_alaw,
  pcm_s16be,pcm_s24be,pcm_s32be,pcm_f32be,pcm_f64be,pcm_s16le_planar,pcm_s8'

# *_pipe demuxers are what makes a single image file probe correctly.
# Their configure names are image_*_pipe even though -formats prints them as *_pipe.
# TGA has no magic bytes, so it is reached by extension through image2.
DEMUXERS='
  image2,image2pipe,
  image_png_pipe,image_jpeg_pipe,image_webp_pipe,webp_anim,image_gif_pipe,image_bmp_pipe,
  wav,aac,mp3,flac,ogg,matroska,mov'

MUXERS='image2,mp3'
ENCODERS='mjpeg,libmp3lame'
FILTERS='aresample,scale'   # the rest come from ffmpeg_select
PARSERS='aac,mpegaudio,opus,vorbis,flac,png,mjpeg,webp,gif,bmp'   # image parsers: only needed when the input is a pipe
PROTOCOLS='file,pipe'

cd "$SRC/ffmpeg-$FFMPEG_VERSION"
export PKG_CONFIG_PATH="$DEPS/lib/pkgconfig:$DEPS/lib64/pkgconfig"

# nasm is required: configure fails loudly rather than silently building a ~40% slower binary
# --optflags: --enable-small appends -Os after --extra-cflags, so -Oz has to go here to win
# --disable-iconv: configure probes libc iconv even with --disable-autodetect (subtitle recoding only)
# --disable-unstable: drops the experimental swscale ops backend; the legacy path is tried first anyway
./configure \
  --cc="$CC" \
  --disable-everything \
  --disable-shared --enable-static \
  --disable-autodetect --disable-doc --disable-debug --disable-network \
  --disable-avdevice --disable-hwaccels --disable-iconv --disable-iamf --disable-unstable \
  --disable-programs --enable-ffmpeg \
  --enable-small --enable-lto --optflags=-Oz \
  --enable-zlib --enable-libmp3lame \
  --pkg-config-flags=--static \
  --extra-cflags="$SIZE_CFLAGS -I$DEPS/include" \
  --extra-ldflags="-no-pie -L$DEPS/lib -L$DEPS/lib64" \
  --extra-ldexeflags="-static -Wl,--gc-sections -Wl,--build-id=none -Wl,-z,noseparate-code -Wl,-T,$ROOT/eh_frame.ld -flto=$JOBS" \
  --enable-decoder="$(tr -d ' \n' <<<"$DECODERS")" \
  --enable-encoder="$ENCODERS" \
  --enable-demuxer="$(tr -d ' \n' <<<"$DEMUXERS")" \
  --enable-muxer="$MUXERS" \
  --enable-filter="$FILTERS" \
  --enable-parser="$PARSERS" \
  --enable-protocol="$PROTOCOLS"

make -j"$JOBS"
strip --strip-all ffmpeg
install -Dm755 ffmpeg "$OUT/bin/ffmpeg"

echo
echo "built: $OUT/bin/ffmpeg"
ls -lh "$OUT/bin"
