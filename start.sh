#!/bin/bash
set -e

# Asegurar variables de entorno
export DISPLAY="${DISPLAY:-:99}"

# Limpiar posibles locks anteriores y asegurar directorio X11
rm -f /tmp/.X99-lock /tmp/.X11-unix/X99 2>/dev/null || true
mkdir -p /tmp/.X11-unix
chmod 1777 /tmp/.X11-unix

# Desactivar avisos de fondo de fluxbox
mkdir -p /root/.fluxbox
echo "background: none" > /root/.fluxbox/overlay
ln -sf /bin/true /usr/bin/fbsetbg 2>/dev/null || true

# Iniciar X virtual framebuffer (Pantalla virtual)
if ! pgrep -f "Xvfb.*:99" > /dev/null 2>&1; then
    echo "[start.sh] Iniciando Xvfb en :99..."
    Xvfb :99 -screen 0 1600x900x24 > /tmp/xvfb.log 2>&1 &
    sleep 2
fi

# Iniciar gestor de ventanas mínimo
if ! pgrep -f "fluxbox" > /dev/null 2>&1; then
    echo "[start.sh] Iniciando fluxbox..."
    fluxbox > /tmp/fluxbox.log 2>&1 &
fi

# Iniciar servidor VNC anclado a la pantalla virtual
if ! pgrep -f "x11vnc" > /dev/null 2>&1; then
    echo "[start.sh] Iniciando x11vnc..."
    x11vnc -display :99 -nopw -listen localhost -xkb -ncache 10 -ncache_cr -forever > /tmp/x11vnc.log 2>&1 &
fi

# Crear un enlace simbólico por si Debian usa vnc_lite.html en vez de vnc.html
ln -sf /usr/share/novnc/vnc_lite.html /usr/share/novnc/vnc.html 2>/dev/null || true

# Iniciar puente WebSockets para noVNC (Sirve VNC por HTTP)
if ! pgrep -f "websockify" > /dev/null 2>&1; then
    echo "[start.sh] Iniciando websockify en 0.0.0.0:8084..."
    websockify --web /usr/share/novnc/ 0.0.0.0:8084 127.0.0.1:5900 > /tmp/websockify.log 2>&1 &
fi

echo "[start.sh] Servicios VNC y pantalla virtual inicializados."

# Si se proporcionaron argumentos al contenedor (ej: CMD en Portainer), ejecutarlos.
# De lo contrario, iniciar la aplicación Flask por defecto.
if [ $# -gt 0 ]; then
    echo "[start.sh] Ejecutando comando proporcionado: $@"
    exec "$@"
else
    echo "[start.sh] Iniciando servidor Flask..."
    exec python /app/app.py
fi
