#!/usr/bin/env bash
# Generate fixtures with the system ffmpeg, then check out/bin against the required list.
set -uo pipefail

HERE=$(cd -- "$(dirname -- "$0")" && pwd)
FF=$HERE/out/bin/ffmpeg
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
fail=0

sys() { ffmpeg -hide_banner -v error -y "$@" </dev/null; }

# ------------------------------------------------- every requested component present
expect() { # <type> <names...>
  local type=$1; shift
  local have
  have=$("$FF" -hide_banner "-$type" 2>/dev/null | tail -n +4 | awk '{print $2}' | tr ',' '\n')
  for n in "$@"; do
    grep -qx -- "$n" <<<"$have" && printf '  %-16s ok\n' "$n" \
      || { printf '  %-16s MISSING\n' "$n"; fail=1; }
  done
}
echo "== components =="
expect decoders mjpeg png webp webp_anim gif targa bmp \
                aac alac flac mp3 opus vorbis pcm_s16le pcm_s24le pcm_f32le pcm_u8 pcm_mulaw
expect encoders png libmp3lame
expect demuxers wav aac mp3 flac ogg matroska mov image2 \
                png_pipe jpeg_pipe webp_pipe webp_anim gif_pipe bmp_pipe
expect muxers image2 mp3
expect filters palettegen paletteuse scale aresample

absent() { # <type> <names...> - guard against components creeping back in
  local type=$1; shift
  local have
  have=$("$FF" -hide_banner "-$type" 2>/dev/null | tail -n +4 | awk '{print $2}' | tr ',' '\n')
  for n in "$@"; do
    grep -qx -- "$n" <<<"$have" && { printf '  %-16s STILL PRESENT\n' "$n"; fail=1; } || true
  done
}
echo "== removed (must be absent) =="
absent decoders qoa qoi av1 libdav1d jpeg2000 tiff
absent demuxers qoa qoi_pipe tiff_pipe j2k_pipe
absent encoders libwebp libwebp_anim

# ---------------------------------------------------------------- fixtures
sys -f lavfi -i testsrc2=s=160x120:d=1 -frames:v 1 "$T/img.png"
sys -i "$T/img.png" "$T/img.jpg"
sys -i "$T/img.png" -c:v libwebp "$T/img.webp"
sys -i "$T/img.png" -c:v libwebp -lossless 1 "$T/imgl.webp"
sys -i "$T/img.png" "$T/img.gif"
sys -i "$T/img.png" "$T/img.tga"
sys -i "$T/img.png" "$T/img.bmp"
sys -f lavfi -i testsrc2=s=64x64:d=1 -r 5 "$T/imga.gif"
sys -f lavfi -i testsrc2=s=64x64:d=1 -r 5 -c:v libwebp_anim "$T/imga.webp"
sys -f lavfi -i testsrc2=s=64x64:d=1 -r 5 -plays 0 -f apng "$T/imga.png"

sys -f lavfi -i "sine=frequency=440:duration=1" -c:a pcm_s16le "$T/a.wav"
sys -i "$T/a.wav" -c:a libmp3lame -q:a 5 "$T/a.mp3"
sys -i "$T/a.wav" -c:a libopus -b:a 64k "$T/a.opus"
sys -i "$T/a.wav" -c:a aac -b:a 96k "$T/a.m4a"
sys -i "$T/a.wav" -c:a flac "$T/a.flac"
sys -i "$T/a.wav" -c:a alac "$T/a.alac.m4a"
sys -i "$T/a.wav" -c:a libvorbis -q:a 3 "$T/a.ogg"
sys -i "$T/a.wav" -c:a libopus -b:a 64k "$T/a.webm"
sys -i "$T/a.wav" -c:a aac -f adts "$T/a.aac"

echo "== convert (images -> png, audio -> mp3, result read back by the system ffmpeg) =="
convert() { # <fixture>
  local f=$T/$1 out=$T/out-$1 opts=
  if [ ! -f "$f" ]; then printf '  %-16s SKIP (no fixture)\n' "$1"; return; fi
  case $1 in img*) out=$out.png opts='-frames:v 1' ;; *) out=$out.mp3 ;; esac   # first frame of animations, see README
  if "$FF" -nostdin -v error -y -i "$f" $opts "$out" 2>"$T/e" && sys -i "$out" -f null -; then
    printf '  %-16s ok  -> %s\n' "$1" "${out##*.}"
  else printf '  %-16s FAIL %s\n' "$1" "$(head -1 "$T/e")"; fail=1; fi
}
for f in img.jpg img.png img.webp imgl.webp img.gif img.tga img.bmp imga.gif imga.webp imga.png \
         a.wav a.mp3 a.opus a.m4a a.flac a.alac.m4a a.ogg a.webm a.aac; do convert "$f"; done

echo "== other paths =="
enc() { local label=$1; shift
  if "$FF" -nostdin -v error -y "$@" 2>"$T/e"; then printf '  %-16s ok\n' "$label"
  else printf '  %-16s FAIL %s\n' "$label" "$(head -1 "$T/e")"; fail=1; fi; }
enc palettegen     -i "$T/img.png" -vf palettegen "$T/pal.png"
enc "png quantized" -i "$T/img.png" -i "$T/pal.png" -lavfi paletteuse "$T/oq.png"
enc print_graphs   -i "$T/img.jpg" -print_graphs_file "$T/g.json" -print_graphs_format json "$T/o3.png"   # aborts in unpatched --enable-small builds
enc "png from pipe" -i pipe:0 -f image2 -c:v png "$T/o4.png" <"$T/img.jpg"   # needs the image parsers
sys -i "$T/oq.png" -f null - || { echo "  quantized png unreadable"; fail=1; }

echo
echo "== everything this build has =="
"$FF" -hide_banner -decoders 2>/dev/null | tail -n +4 | awk '{print $2}' | tr '\n' ' '; echo
echo
[ "$fail" = 0 ] && echo "ALL OK" || echo "FAILURES ABOVE"
exit "$fail"
