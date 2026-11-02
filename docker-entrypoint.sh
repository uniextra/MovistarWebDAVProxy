#!/bin/sh
set -e

TOKENS_DIR="${TOKENS_DIR:-/app/tokens}"
export TOKENS_DIR

can_access_as_app() {
    su-exec app:app sh -c '
        test -r "$1" && test -w "$1" || exit 1
        for f in "$1"/.env "$1"/*.env; do
            if [ -e "$f" ]; then
                test -r "$f" && test -w "$f" || exit 1
            fi
        done
    ' -- "$TOKENS_DIR"
}

# Si el contenedor inicia como root (por ejemplo al actualizar desde versiones <= v2.0.7),
# intentamos transferir la propiedad de /app/tokens al usuario "app" (UID 10001).
# Si el host (ej. Synology DSM) bloquea chown o algún fichero .env sigue sin permisos de lectura/escritura
# para "app", mantenemos la ejecución como root para no perder acceso a las credenciales existentes.
if [ "$(id -u)" = "0" ]; then
    mkdir -p "$TOKENS_DIR"
    chown -R app:app "$TOKENS_DIR" 2>/dev/null || true
    chmod 700 "$TOKENS_DIR" 2>/dev/null || true
    chmod 600 "$TOKENS_DIR"/.env "$TOKENS_DIR"/*.env 2>/dev/null || true

    if can_access_as_app; then
        exec su-exec app:app /app/movistar-proxy "$@"
    else
        exec /app/movistar-proxy "$@"
    fi
fi

exec /app/movistar-proxy "$@"
