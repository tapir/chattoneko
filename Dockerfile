# syntax=docker/dockerfile:1
#
# Chattoneko — minimal container image.
#
#   Stage 1 (build):  archlinux — ffmpeg/build.sh links the static ffmpeg, then
#                     `make build` produces the Go binary with the SPA embedded.
#                     Arch because build.sh is tuned for that toolchain
#                     (musl-gcc, a PIE-by-default gcc, its binutils' handling of
#                     eh_frame.ld); the ~430 MB base is discarded whole.
#   Stage 2 (final):  busybox — those two static binaries and the CA bundle they
#                     verify outbound TLS against. No shell entrypoint and no
#                     su-exec: main.go starts as root, makes the data dir
#                     writable and drops itself to 1000:1000 (droproot_linux.go).
#
# /opt/chattoneko is the binary; all runtime state (neko.db — config AND
# chats live in SQLite) lives under /var/lib/chattoneko, so a single volume
# mounted there covers everything. There is no config file: first start seeds
# defaults and the rest is set through the API/UI.

# ── Stage 1: build both binaries ─────────────────────────────────────────────
FROM archlinux:latest AS build
# which + pkgconf + xz: ffmpeg's configure and build.sh's untar want them and
# the base image carries none of them. musl + nasm: build.sh's static ffmpeg.
RUN pacman -Syu --noconfirm --needed base-devel musl nasm curl which pkgconf \
        ca-certificates xz go nodejs npm upx \
 && pacman -Scc --noconfirm
# `go install` puts sqlc in /root/go/bin, which Arch does not add to PATH.
ENV PATH=/root/go/bin:$PATH
RUN go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
WORKDIR /src
# The ffmpeg comes first, from its own COPY, so its layer survives every app
# edit: a frontend or Go change does not rebuild it (~2 min).
COPY ffmpeg/ ./ffmpeg/
RUN cd ffmpeg && ./build.sh
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The release workflow passes the git tag (vX.Y.Z); a plain `docker build`
# keeps the Makefile's local default. The tag's leading v is stripped here so
# the binary carries the same bare number the APK's versionName does.
ARG VERSION=1.0.0-local
RUN make build VERSION="${VERSION#v}"

# ── Stage 2: runtime — busybox + the two static binaries ─────────────────────
# busybox rather than scratch for two things: the directories below (scratch
# cannot mkdir, and /tmp has to exist and be writable by 1000 for the temp
# files internal/media stages) and a shell for `docker exec … sh`.
FROM busybox:musl
# Outbound TLS only — the provider endpoint, its /models metadata, MCP servers
# and the fetch tool all verify the remote certificate against this bundle.
# Arch keeps the real file under /etc/ca-certificates and symlinks it into
# /etc/ssl/certs, so copy the target to the path Go looks for.
COPY --from=build /etc/ca-certificates/extracted/tls-ca-bundle.pem /etc/ssl/certs/ca-certificates.crt
COPY --from=build /src/chattoneko /opt/chattoneko
# Found on PATH by internal/media; $CHATTO_FFMPEG overrides it.
COPY --from=build /src/ffmpeg/out/bin/ffmpeg /usr/local/bin/ffmpeg
RUN mkdir -p /tmp /var/lib/chattoneko \
 && chmod 1777 /tmp \
 && chmod +x /opt/chattoneko \
 && chown 1000:1000 /var/lib/chattoneko
ENV PATH=/usr/local/bin:/usr/bin:/bin
VOLUME ["/var/lib/chattoneko"]
ENTRYPOINT ["/opt/chattoneko"]
CMD ["-db", "/var/lib/chattoneko/neko.db"]
# NOTE: no USER here — the app starts as root, fixes bind-mount ownership, then
# drops to 1000:1000 itself before it opens the database.
