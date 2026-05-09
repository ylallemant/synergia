FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /synergia-manager ./cmd/synergia-manager

# Distroless: no shell, no package manager, no curl — CA certs included, runs as uid 65532
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /synergia-manager /synergia-manager

EXPOSE 7500 7501

ENTRYPOINT ["/synergia-manager"]
