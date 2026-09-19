#!/usr/bin/env bash
# Checks out/bin/jq for what a caller shelling out to it depends on.
# Upstream's own suite is `make check` inside src/jq-*/: 6 of 7 pass here, and
# the 7th (shtest) asserts strflocaltime honours LC_ALL, which musl cannot do.
set -uo pipefail

HERE=$(cd -- "$(dirname -- "$0")" && pwd)
JQ=$HERE/out/bin/jq
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
fail=0

# read from build.sh so a version bump cannot leave this asserting the old one
WANT_VERSION=$(sed -n 's/^JQ_VERSION=//p' "$HERE/build.sh")

check() { # <label> <expected> <actual>
  if [ "$2" = "$3" ]; then printf '  %-22s ok\n' "$1"
  else printf '  %-22s FAIL  want [%s] got [%s]\n' "$1" "$2" "$3"; fail=1; fi
}
run() { printf '%s' "$2" | "$JQ" -c "$1" 2>&1; }

[ -x "$JQ" ] || { echo "no binary — run ./build.sh first"; exit 1; }

echo "== identity =="
check "version" "jq-$WANT_VERSION" "$("$JQ" --version 2>&1)"
check "static"  "yes" "$(file -b "$JQ" | grep -q 'statically linked' && echo yes || echo no)"

echo "== core =="
check "slurp -s"   '3'        "$(printf '1\n2\n3\n' | "$JQ" -cs 'length' 2>&1)"
check "raw out -r" 'hi'       "$(printf '"hi"' | "$JQ" -r '.' 2>&1)"
check "@base64d"   '"hi"'     "$(run '@base64d' '"aGk="')"
check "utf8"       '"é日"'    "$(run '.' '"é日"')"

echo "== regex (guards --with-oniguruma) =="
check "test"    'true'                  "$(run 'test("w.rld")' '"hello world"')"
check "capture" '{"a":"hello"}'         "$(run 'capture("(?<a>\\w+)")' '"hello world"')"
check "sub"     '"hell0 w0rld"'         "$(run 'sub("o";"0";"g")' '"hello world"')"
check "splits"  '["he","","o wor","d"]' "$(run '[splits("l")]' '"hello world"')"

# decNumber stays built in, so big integers survive a round trip. Pinned so
# re-adding --disable-decnum is a visible diff rather than a silent one.
echo "== decNumber =="
check "19-digit int"  '12345678901234567890' "$(run '.' '12345678901234567890')"
check "exact decimal" '1.00000000000000000000001' "$(run '.' '1.00000000000000000000001')"

echo "== bad input =="
printf 'not json' | "$JQ" -c '.a' >"$T/out" 2>"$T/err"
check "exit code"  "5"   "$?"
check "no output"  ""    "$(cat "$T/out")"
check "stderr"     "yes" "$(grep -q 'parse error' "$T/err" && echo yes || echo no)"

echo
[ "$fail" = 0 ] && echo "ALL OK" || echo "FAILURES ABOVE"
exit "$fail"
