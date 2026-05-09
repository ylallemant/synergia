FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /synergia-manager ./cmd/synergia-manager

FROM alpine:3.20

# ca-certificates required for TLS; curl removed (not needed at runtime)
RUN apk add --no-cache ca-certificates

COPY --from=builder /synergia-manager /usr/local/bin/synergia-manager

USER nobody:nobody

EXPOSE 7500 7501

ENTRYPOINT ["/usr/local/bin/synergia-manager"]
