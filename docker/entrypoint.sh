#!/bin/sh
set -eu

DATA_DIR=/var/lib/chattoneko

if [ "$(id -u)" = "0" ]; then
  # Bind-mounted data dirs arrive with host ownership (usually root).
  # -R also repairs leftovers from earlier root runs; fall back to the
  # top-level dir if something inside is immutable (e.g. a ro mount).
  chown -R 1000:1000 "$DATA_DIR" 2>/dev/null || chown 1000:1000 "$DATA_DIR"
  exec su-exec 1000:1000 /opt/chattoneko "$@"
fi

# Already non-root (docker run --user ...): run as-is.
exec /opt/chattoneko "$@"
