#!/bin/sh
set -e

TOKENS_DIR="${TOKENS_DIR:-/app/tokens}"

# Si el contenedor inicia como root (por ejemplo al actualizar desde versiones <= v2.0.7),
# aseguramos que el volumen persistente de tokens pertenezca al usuario no-root "app" (UID 10001)
# antes de soltar privilegios con su-exec.
if [ "$(id -u)" = "0" ]; then
    mkdir -p "$TOKENS_DIR"
    chown -R app:app "$TOKENS_DIR" 2>/dev/null || true
    chmod 700 "$TOKENS_DIR" 2>/dev/null || true
    exec su-exec app:app /app/movistar-proxy "$@"
fi

exec /app/movistar-proxy "$@"
