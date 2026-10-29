# Etapa 1: Compilador Go
FROM golang:1.22-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /movistar-proxy ./cmd/server

# Etapa 2: Imagen Mínima Final (~20 MB)
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /movistar-proxy /app/movistar-proxy

VOLUME ["/data", "/app/tokens"]

EXPOSE 5000 8080

ENTRYPOINT ["/app/movistar-proxy"]
