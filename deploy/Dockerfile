FROM golang:1.25-alpine@sha256:8d22e29d960bc50cd025d93d5b7c7d220b1ee9aa7a239b3c8f55a57e987e8d45 AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /synergia-manager ./cmd/synergia-manager

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

# ca-certificates required for TLS; curl removed (not needed at runtime)
RUN apk add --no-cache ca-certificates \
 && find / -perm /6000 -type f -exec chmod a-s {} \; 2>/dev/null || true

COPY --from=builder --chown=nobody:nobody /synergia-manager /usr/local/bin/synergia-manager

USER nobody:nobody

EXPOSE 7500 7501

# Tries HTTPS first (self-signed cert skipped for localhost health check),
# falls back to HTTP for insecure mode deployments.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD wget -qO /dev/null --no-check-certificate https://127.0.0.1:7500/healthz 2>/dev/null \
   || wget -qO /dev/null http://127.0.0.1:7500/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/synergia-manager"]
