# Stage 1: Extract jellyfin-web static assets from Jellyfin 12.4
FROM jellyfin/jellyfin:12.4 AS jellyfin-web
RUN mkdir -p /tmp/web && \
    if [ -d /jellyfin/jellyfin-web ]; then \
        cp -a /jellyfin/jellyfin-web/. /tmp/web/; \
    elif [ -d /usr/share/jellyfin/web ]; then \
        cp -a /usr/share/jellyfin/web/. /tmp/web/; \
    else \
        echo "jellyfin-web directory not found" >&2 && exit 1; \
    fi

# Stage 2: Patch jellyfin-web config.json cleanly
FROM alpine:3.24 AS web-patcher
RUN apk add --no-cache jq
COPY --from=jellyfin-web /tmp/web /tmp/web
RUN cp -a /tmp/web /web && \
    if [ -f /web/config.json ]; then \
        jq '.multiserver = false' /web/config.json > /web/config.json.tmp && \
        mv /web/config.json.tmp /web/config.json; \
    else \
        echo '{"multiserver":false}' > /web/config.json; \
    fi

# Stage 3: Build static Go binary
FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-$(go env GOARCH)} \
    go build -ldflags="-s -w" -trimpath -o /out/mergerfin ./cmd/mergerfin

# Stage 4: Minimal runtime image
FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -g 1000 mergerfin && \
    adduser -D -u 1000 -G mergerfin mergerfin && \
    mkdir -p /data /app/web && \
    chown -R 1000:1000 /data /app/web

COPY --from=builder /out/mergerfin /usr/local/bin/mergerfin
COPY --from=web-patcher --chown=1000:1000 /web /app/web

ENV MERGERFIN_PORT=8096 \
    MERGERFIN_DATA_DIR=/data \
    MERGERFIN_WEB_DIR=/app/web

USER 1000:1000
WORKDIR /data
EXPOSE 8096
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8096/System/Info/Public >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/mergerfin"]
