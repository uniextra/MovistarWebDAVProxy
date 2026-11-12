# Etapa 1: Compilador Go
FROM golang:1.26-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /movistar-proxy ./cmd/server

# Etapa 2: Imagen Mínima Final (~20 MB)
FROM alpine:3.21

ENV TZ=Europe/Madrid

RUN apk add --no-cache ca-certificates tzdata su-exec && \
    adduser -D -u 10001 app && \
    mkdir -p /app/tokens && \
    chown -R app:app /app

WORKDIR /app

COPY --from=builder /movistar-proxy /app/movistar-proxy
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh

VOLUME ["/app/tokens"]

EXPOSE 5000 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/movistar-proxy", "-healthcheck"]

ENTRYPOINT ["/app/docker-entrypoint.sh"]
