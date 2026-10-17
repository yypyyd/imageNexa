# syntax=docker/dockerfile:1

# --- Stage 1: build the Go binary from source ---
FROM golang:1.26.6-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
# Cache deps first for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/init-bucket ./cmd/init-bucket

# One-shot storage setup uses our existing S3 client and build toolchain.
FROM scratch AS bucketinit
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/init-bucket /init-bucket
USER 65534:65534
ENTRYPOINT ["/init-bucket"]

# --- Stage 2: minimal runtime image ---
FROM alpine:3.20
# ca-certificates: outbound HTTPS to the AI providers. tzdata: POSTGRES_DSN sets
# TimeZone=Asia/Shanghai. wget: container healthcheck. Node runs Dola's local
# protocol signer. Retired providers require no browser or extra certificates.
RUN apk add --no-cache ca-certificates tzdata wget nodejs npm \
    && addgroup -S chrome \
    && adduser -S -G chrome chrome
WORKDIR /app
COPY scripts/dola-protocol /app/scripts/dola-protocol
RUN cd /app/scripts/dola-protocol && npm ci --omit=dev --ignore-scripts && npm cache clean --force
COPY --chown=chrome:chrome --from=build /out/api /app/api
# Local fallback for generated media / reference uploads (RustFS/S3 is primary).
# Dola browser profiles live here so a server restart reuses the server-created
# device identity instead of copying one from another machine.
RUN mkdir -p /app/data/generated /app/data/dola-profiles \
		&& chown -R chrome:chrome /app/data \
		&& chmod +x /app/api
ENV HTTP_ADDR=0.0.0.0:6666
EXPOSE 6666
USER chrome
ENTRYPOINT ["/app/api"]
