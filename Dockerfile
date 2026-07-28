# syntax=docker/dockerfile:1

# ---- build stage ----
# Pure-Go build (taglib runs as WASM via wazero, so no cgo / no C libraries).
FROM golang:1.26-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ENV CGO_ENABLED=0 GOOS=linux
# The web UI is embedded via //go:embed, so the binary is fully self-contained.
RUN go build -trimpath -ldflags="-s -w" -o /out/spotiflac .

# ---- runtime stage ----
# Only external dependency is ffmpeg (used for convert/resample and some
# download post-processing) plus CA certificates for HTTPS.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/spotiflac /usr/local/bin/spotiflac

ENV CONFIG_DIR=/config \
    DOWNLOAD_DIR=/downloads \
    ADDR=:8080
VOLUME ["/downloads", "/config"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/spotiflac"]
