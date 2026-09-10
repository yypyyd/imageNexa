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

# --- Stage 2: minimal runtime image ---
FROM alpine:3.20
# ca-certificates: outbound HTTPS to the AI providers. tzdata: POSTGRES_DSN sets
# TimeZone=Asia/Shanghai. wget: container healthcheck. chromium executes
# OreateAI's official browser-side Banti signer for each Seedance request.
# cdn.oreateai.com omits its GlobalSign intermediate certificate. Keep
# Chromium verification enabled and install the exact AIA certificate with a
# pinned digest instead of using --ignore-certificate-errors. Chromium 131 uses
# NSS independently of Alpine's CA bundle, so keep the intermediate (without
# root trust) in the dedicated browser user's NSS database as well.
RUN apk add --no-cache ca-certificates tzdata wget chromium nodejs npm openssl nss-tools \
    && addgroup -S chrome \
    && adduser -S -G chrome chrome \
    && wget -q https://secure.globalsign.com/cacert/gsgccr3dvtlsca2020.crt \
        -O /tmp/globalsign-gcc-r3-dv-tls-ca-2020.der \
    && echo "762538439509c411c437d3c567563e1378671281fc4a1464add031870843676e  /tmp/globalsign-gcc-r3-dv-tls-ca-2020.der" | sha256sum -c - \
    && openssl x509 -inform DER \
        -in /tmp/globalsign-gcc-r3-dv-tls-ca-2020.der \
        -out /usr/local/share/ca-certificates/globalsign-gcc-r3-dv-tls-ca-2020.crt \
    && update-ca-certificates \
    && mkdir -p /home/chrome/.pki/nssdb \
    && certutil -N -d sql:/home/chrome/.pki/nssdb --empty-password \
    && certutil -A -d sql:/home/chrome/.pki/nssdb \
        -n oreate-globalsign-intermediate -t ",," \
        -i /usr/local/share/ca-certificates/globalsign-gcc-r3-dv-tls-ca-2020.crt \
    && certutil -L -d sql:/home/chrome/.pki/nssdb \
        -n oreate-globalsign-intermediate >/dev/null \
    && chown -R chrome:chrome /home/chrome/.pki \
    && rm -f /tmp/globalsign-gcc-r3-dv-tls-ca-2020.der \
    && apk del openssl nss-tools
WORKDIR /app
COPY scripts/dola-protocol /app/scripts/dola-protocol
RUN cd /app/scripts/dola-protocol && npm ci --omit=dev --ignore-scripts && npm cache clean --force
COPY --chown=chrome:chrome --from=build /out/api /app/api
COPY --chown=chrome:chrome scripts/oreate-chromium /usr/local/bin/oreate-chromium
# Local fallback for generated media / reference uploads (RustFS/S3 is primary).
# Dola browser profiles live here so a server restart reuses the server-created
# device identity instead of copying one from another machine.
RUN mkdir -p /app/data/generated /app/data/dola-profiles \
		&& chown -R chrome:chrome /app/data \
		&& chmod +x /app/api /usr/local/bin/oreate-chromium
ENV OREATE_CHROME=/usr/local/bin/oreate-chromium
# Docker's default seccomp profile blocks Chromium's user namespaces. The
# entire API already runs as the unprivileged chrome UID; browser processes
# also receive an empty environment, ephemeral profile, parent-death signal,
# and process-group teardown as compensating controls.
ENV OREATE_NO_SANDBOX=true
ENV HTTP_ADDR=0.0.0.0:6666
EXPOSE 6666
USER chrome
ENTRYPOINT ["/app/api"]
