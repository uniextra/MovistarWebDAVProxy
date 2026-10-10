import os
import time
import subprocess

def is_process_running(pattern):
    try:
        res = subprocess.run(["pgrep", "-f", pattern], capture_output=True, text=True)
        return res.returncode == 0
    except Exception:
        return False

def ensure_vnc_services():
    """
    Verifica y levanta de forma autónoma Xvfb, fluxbox, x11vnc y websockify (noVNC).
    Esto garantiza que el entorno gráfico siempre esté activo sin importar si el
    contenedor fue iniciado via start.sh, python app.py o Portainer Command override.
    """
    if os.name != "posix":
        # Entorno no Linux (por ejemplo desarrollo en Windows)
        return

    os.environ["DISPLAY"] = ":99"

    # 1. Asegurar directorio y permisos de sockets X11
    os.makedirs("/tmp/.X11-unix", exist_ok=True)
    try:
        os.chmod("/tmp/.X11-unix", 0o1777)
    except Exception:
        pass

    # 2. Xvfb (Virtual Framebuffer)
    if not is_process_running("Xvfb.*:99"):
        print("[vnc_helper] Xvfb no está en ejecución. Iniciando Xvfb en :99...", flush=True)
        # Limpiar locks huérfanos si los hubiera
        for lock in ["/tmp/.X99-lock", "/tmp/.X11-unix/X99"]:
            if os.path.exists(lock):
                try:
                    os.remove(lock)
                except Exception:
                    pass

        try:
            xvfb_out = open("/tmp/xvfb.log", "a")
            subprocess.Popen(
                ["Xvfb", ":99", "-screen", "0", "1600x900x24"],
                stdout=xvfb_out,
                stderr=xvfb_out
            )
            time.sleep(1.5)
            print("[vnc_helper] Xvfb iniciado correctamente.", flush=True)
        except Exception as e:
            print(f"[vnc_helper] Error al iniciar Xvfb: {e}", flush=True)

    # 3. Fluxbox (Gestor de ventanas mínimo)
    if not is_process_running("fluxbox"):
        print("[vnc_helper] Iniciando gestor de ventanas fluxbox...", flush=True)
        try:
            os.makedirs("/root/.fluxbox", exist_ok=True)
            with open("/root/.fluxbox/overlay", "w") as f:
                f.write("background: none\n")
            if not os.path.exists("/usr/bin/fbsetbg"):
                try:
                    os.symlink("/bin/true", "/usr/bin/fbsetbg")
                except Exception:
                    pass

            fb_out = open("/tmp/fluxbox.log", "a")
            subprocess.Popen(["fluxbox"], stdout=fb_out, stderr=fb_out)
        except Exception as e:
            print(f"[vnc_helper] Error al iniciar fluxbox: {e}", flush=True)

    # 4. x11vnc (Servidor VNC)
    if not is_process_running("x11vnc"):
        print("[vnc_helper] Iniciando servidor x11vnc en display :99...", flush=True)
        try:
            vnc_out = open("/tmp/x11vnc.log", "a")
            subprocess.Popen(
                [
                    "x11vnc",
                    "-display", ":99",
                    "-nopw",
                    "-listen", "localhost",
                    "-xkb",
                    "-ncache", "10",
                    "-ncache_cr",
                    "-forever"
                ],
                stdout=vnc_out,
                stderr=vnc_out
            )
        except Exception as e:
            print(f"[vnc_helper] Error al iniciar x11vnc: {e}", flush=True)

    # 5. noVNC HTML symlink
    if os.path.exists("/usr/share/novnc/vnc_lite.html") and not os.path.exists("/usr/share/novnc/vnc.html"):
        try:
            os.symlink("/usr/share/novnc/vnc_lite.html", "/usr/share/novnc/vnc.html")
        except Exception:
            pass

    # 6. websockify (noVNC HTTP / WebSocket proxy)
    if not is_process_running("websockify"):
        print("[vnc_helper] Iniciando websockify en 0.0.0.0:8084...", flush=True)
        try:
            ws_out = open("/tmp/websockify.log", "a")
            subprocess.Popen(
                [
                    "websockify",
                    "--web", "/usr/share/novnc/",
                    "0.0.0.0:8084",
                    "127.0.0.1:5900"
                ],
                stdout=ws_out,
                stderr=ws_out
            )
        except Exception as e:
            print(f"[vnc_helper] Error al iniciar websockify: {e}", flush=True)

def get_vnc_status():
    status = {
        "xvfb_running": is_process_running("Xvfb.*:99"),
        "fluxbox_running": is_process_running("fluxbox"),
        "x11vnc_running": is_process_running("x11vnc"),
        "websockify_running": is_process_running("websockify"),
        "display_env": os.environ.get("DISPLAY", "")
    }
    try:
        res = subprocess.run(["ps", "-eo", "pid,comm,args"], capture_output=True, text=True)
        status["processes"] = res.stdout.splitlines()[:50]
    except Exception as e:
        status["processes_error"] = str(e)
    return status
