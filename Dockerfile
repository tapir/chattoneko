# syntax=docker/dockerfile:1
#
# Chattoneko — minimal container image.
#
#   Stage 1 (build):  golang/alpine with everything `make build` needs
#                     (nodejs/npm for the frontend, sqlc, make, upx);
#                     runs the Makefile as-is, no custom build steps.
#   Stage 2 (final):  alpine — the static, upx-compressed binary plus
#                     su-exec and an entrypoint that fixes bind-mount
#                     ownership at start, then drops to 1000:1000.
#
# /opt/chattoneko is the binary; all runtime state (neko.db — config AND
# chats live in SQLite) lives under /var/lib/chattoneko, so a single volume
# mounted there covers everything. There is no config file: first start seeds
# defaults and the rest is set through the API/UI.

# ── Stage 1: build via `make build` ─────────────────────────────────────────
FROM golang:1.27-alpine AS build
RUN apk add --no-cache make nodejs npm upx
RUN go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The release workflow passes the git tag (vX.Y.Z); a plain `docker build`
# keeps the Makefile's local default. The tag's leading v is stripped here so
# the binary carries the same bare number the APK's versionName does.
ARG VERSION=1.0.0-local
RUN make build VERSION="${VERSION#v}"

# ── Stage 2: runtime — alpine + su-exec entrypoint ───────────────────────────
FROM alpine:3.21
RUN apk add --no-cache ca-certificates su-exec \
 && mkdir -p /var/lib/chattoneko
COPY --from=build /src/chattoneko /opt/chattoneko
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chown -R 1000:1000 /var/lib/chattoneko \
 && chmod +x /usr/local/bin/entrypoint.sh /opt/chattoneko
VOLUME ["/var/lib/chattoneko"]
ENTRYPOINT ["entrypoint.sh"]
CMD ["-db", "/var/lib/chattoneko/neko.db"]
# NOTE: no USER here — the entrypoint starts as root, fixes bind-mount
# ownership, then drops to 1000:1000 before exec'ing the app.
