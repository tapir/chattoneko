#!/usr/bin/env bash
# Fully static jq: musl, -Oz, LTO, nothing cut but the build-time extras.
set -euo pipefail

JQ_VERSION=1.8.2

ROOT=$(cd -- "$(dirname -- "$0")" && pwd)
SRC=$ROOT/src
OUT=$ROOT/out
JOBS=$(nproc)
# Static musl, same reasoning as ../ffmpeg/build.sh. Alpine's gcc IS musl, so
# an Alpine build passes CC=gcc.
CC=${CC:-musl-gcc}
# -fno-pie: Arch gcc defaults to PIE, and configure's link tests reject the
# objects unless LDFLAGS carries -no-pie too.
# -Wno-unused-but-set-variable: jq's bison skeleton sets yynerrs and never reads
# it. Patching generated code would break on every version bump; the tree here
# is pinned and we edit none of it, so nothing of ours goes unreported.
SIZE_CFLAGS="-Oz -fno-pie -fno-ident -fno-unwind-tables -fno-asynchronous-unwind-tables -ffunction-sections -fdata-sections -Wno-unused-but-set-variable -flto=$JOBS"
LDFLAGS="-no-pie -static -flto=$JOBS -Wl,--gc-sections -Wl,--build-id=none -Wl,-z,noseparate-code"

mkdir -p "$SRC" "$OUT"

# publishes atomically, so an interrupted download can never poison the cache
[ -f "$SRC/jq-$JQ_VERSION.tar.gz" ] || {
  timeout 600 curl -fL --retry 3 -o "$SRC/jq-$JQ_VERSION.tar.gz.part" \
    "https://github.com/jqlang/jq/releases/download/jq-$JQ_VERSION/jq-$JQ_VERSION.tar.gz" &&
    mv "$SRC/jq-$JQ_VERSION.tar.gz.part" "$SRC/jq-$JQ_VERSION.tar.gz"
}
# tar, not cp: oniguruma is an autotools tree and regenerates itself if the
# tarball's timestamps are disturbed, which then demands automake.
[ -d "$SRC/jq-$JQ_VERSION" ] || tar xf "$SRC/jq-$JQ_VERSION.tar.gz" -C "$SRC"

cd "$SRC/jq-$JQ_VERSION"
# The release tarball already vendors oniguruma and decNumber, so this is the
# only download. --enable-all-static is jq's own flag for a static link.
# decNumber stays in: --disable-decnum saves 8 KB and rounds every integer past
# 2^53 on the way in, so a 19-digit id in provider or MCP JSON would not even
# survive being read. Arithmetic is IEEE754 double either way.
CC="$CC" CFLAGS="$SIZE_CFLAGS" LDFLAGS="$LDFLAGS" ./configure \
  --with-oniguruma=builtin \
  --disable-docs --disable-maintainer-mode --disable-valgrind \
  --enable-all-static --disable-shared

make -j"$JOBS"
strip --strip-all jq
install -Dm755 jq "$OUT/bin/jq"

echo
echo "built: $OUT/bin/jq"
ls -lh "$OUT/bin"
