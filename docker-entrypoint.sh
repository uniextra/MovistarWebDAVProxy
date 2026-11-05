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
        if [ -n "$2" ] && [ -d "$2" ]; then
            test -r "$2" && test -w "$2" && test -x "$2" || exit 1
        fi
    ' -- "$TOKENS_DIR" "$WEBDAV_TMPDIR"
}

# Si el contenedor inicia como root (por ejemplo al actualizar desde versiones <= v2.0.7),
# intentamos transferir la propiedad de /app/tokens y de WEBDAV_TMPDIR al usuario "app" (UID 10001).
# Si el host (ej. Synology DSM o permisos estrictos en volúmenes montados) bloquea chown o
# algún fichero/directorio sigue sin permisos de lectura/escritura para "app",
# mantenemos la ejecución como root para evitar fallos catastróficos en runtime (principio de resiliencia).
if [ "$(id -u)" = "0" ]; then
    mkdir -p "$TOKENS_DIR"
    chown -R app:app "$TOKENS_DIR" 2>/dev/null || true
    chmod 700 "$TOKENS_DIR" 2>/dev/null || true
    chmod 600 "$TOKENS_DIR"/.env "$TOKENS_DIR"/*.env 2>/dev/null || true

    if [ -n "$WEBDAV_TMPDIR" ]; then
        mkdir -p "$WEBDAV_TMPDIR" 2>/dev/null || true
        chown -R app:app "$WEBDAV_TMPDIR" 2>/dev/null || true
        chmod 700 "$WEBDAV_TMPDIR" 2>/dev/null || true
    fi

    if can_access_as_app; then
        exec su-exec app:app /app/movistar-proxy "$@"
    else
        exec /app/movistar-proxy "$@"
    fi
fi

exec /app/movistar-proxy "$@"
