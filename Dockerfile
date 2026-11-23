# Stage 1: Build embedded React Management Console
FROM --platform=$BUILDPLATFORM node:22-alpine AS web-builder
WORKDIR /app/web
COPY web/package.json web/package-lock.json* ./
RUN npm install --no-audit --no-fund
COPY web/ ./
RUN npm run build

# Stage 2: Build static pure-Go GoDesk Plane binary
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS go-builder
WORKDIR /app
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY proto/ ./proto/
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
COPY --from=web-builder /app/internal/web/dist ./internal/web/dist
RUN --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
    -o /out/godesk-plane ./cmd/godesk-plane

# Stage 3: Minimal hardened runtime image
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S godesk \
    && adduser -S -G godesk godesk \
    && mkdir -p /data \
    && chown -R godesk:godesk /data

COPY --from=go-builder /out/godesk-plane /usr/local/bin/godesk-plane

USER godesk
WORKDIR /data
ENV DATA_DIR=/data \
    HTTP_PORT=21114 \
    NAT_PORT=21115 \
    RENDEZVOUS_PORT=21116 \
    RELAY_PORT=21117 \
    RENDEZVOUS_WS_PORT=21118 \
    RELAY_WS_PORT=21119

EXPOSE 21114/tcp 21115/tcp 21116/tcp 21116/udp 21117/tcp 21118/tcp 21119/tcp
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:${HTTP_PORT}/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/godesk-plane"]
