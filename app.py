import os
import re
import signal
import subprocess
import threading
from pathlib import Path
import requests
import logging
from flask import Flask, render_template, request, jsonify, send_from_directory
from vnc_helper import ensure_vnc_services, get_vnc_status
from test_movistar_api import MovistarFilesClient, load_account_config, build_oauth_auth_header, refresh_oauth_token

# Inicialización autónoma de los servicios gráficos VNC/X11
ensure_vnc_services()

app = Flask(__name__)
APP_VERSION = "v1.0.6"

# Silenciar peticiones repetitivas de polling (/status, /health, /logs, websockets) de los logs de Docker
class EndpointFilter(logging.Filter):
    def filter(self, record):
        msg = record.getMessage()
        return not any(ep in msg for ep in ["/status", "/health", "/logs", "/ws/bridge"])

logging.getLogger("werkzeug").addFilter(EndpointFilter())

# Configuración y estado global
UPLOAD_LOCK = threading.Lock()
UPLOAD_PROCESS = None
ACTIVE_UPLOAD_PHONE = None
UPLOAD_LOGS = []
LOGIN_PROCESS = None
LOGIN_PHONE = None

TOKENS_DIR = Path("/app/tokens")
PRIMARY_ENV_PATH = TOKENS_DIR / ".env"
FALLBACK_ENV_PATH = Path("/app/.env")

def sanitize_phone(phone):
    if not phone:
        return ""
    digits = re.sub(r"\D", "", str(phone).strip())
    if len(digits) == 11 and digits.startswith("34"):
        digits = digits[2:]
    return digits

def get_account_env_path(phone):
    phone_clean = sanitize_phone(phone)
    if not phone_clean:
        return None
    acct_file = TOKENS_DIR / f"account_{phone_clean}.env"
    if acct_file.exists():
        return acct_file
    legacy_file = TOKENS_DIR / f"{phone_clean}.env"
    if legacy_file.exists():
        return legacy_file
    return acct_file

def get_env_paths(phone=None):
    paths = []
    phone_clean = sanitize_phone(phone)
    if phone_clean:
        acct_file = TOKENS_DIR / f"account_{phone_clean}.env"
        legacy_file = TOKENS_DIR / f"{phone_clean}.env"
        if acct_file.exists():
            paths.append(acct_file)
        if legacy_file.exists():
            paths.append(legacy_file)
        if not paths:
            paths.append(acct_file)
        return paths

    return []

def get_env_vars(phone=None):
    phone_clean = sanitize_phone(phone)
    if not phone_clean:
        return {}

    parsed_vars = {}
    candidates = [
        TOKENS_DIR / f"account_{phone_clean}.env",
        TOKENS_DIR / f"{phone_clean}.env"
    ]
    for p in candidates:
        try:
            if p.exists() and p.is_file():
                with open(p, 'r', encoding='utf-8') as f:
                    for line in f:
                        line = line.strip()
                        if line and not line.startswith('#') and '=' in line:
                            k, v = line.split('=', 1)
                            parsed_vars[k.strip()] = v.strip().strip('"').strip("'")
                if parsed_vars.get("MOVISTAR_JSESSIONID") or parsed_vars.get("MOVISTAR_VALIDATIONKEY"):
                    return parsed_vars
        except Exception:
            pass
    return parsed_vars

def save_env_vars(jsid, vkey, phone=None):
    phone_clean = sanitize_phone(phone)
    if not phone_clean:
        return

    # Preservar tokens de dispositivo nativo si ya existían
    existing = get_env_vars(phone_clean)
    refresh_token = existing.get("MOVISTAR_REFRESH_TOKEN", "")
    access_token = existing.get("MOVISTAR_ACCESS_TOKEN", "")
    device_id = existing.get("MOVISTAR_DEVICE_ID", "")
    device_name = existing.get("MOVISTAR_DEVICE_NAME", "")

    lines = [
        f'MOVISTAR_PHONE="{phone_clean}"',
        f'MOVISTAR_JSESSIONID="{jsid}"',
        f'MOVISTAR_VALIDATIONKEY="{vkey}"'
    ]
    if device_id:
        lines.append(f'MOVISTAR_DEVICE_ID="{device_id}"')
    if device_name:
        lines.append(f'MOVISTAR_DEVICE_NAME="{device_name}"')
    if refresh_token:
        lines.append(f'MOVISTAR_REFRESH_TOKEN="{refresh_token}"')
    if access_token:
        lines.append(f'MOVISTAR_ACCESS_TOKEN="{access_token}"')

    target = TOKENS_DIR / f"account_{phone_clean}.env"
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        with open(target, 'w', encoding='utf-8') as f:
            f.write("\n".join(lines) + "\n")
    except Exception:
        pass

def keepalive_worker():
    """Hilo demonio que realiza ping periódico a Movistar Cloud con /sapi/system/information para mantener la sesión activa."""
    import time
    while True:
        time.sleep(300)  # Cada 5 minutos
        try:
            # Buscar todos los archivos de cuentas disponibles
            env_files = []
            if TOKENS_DIR.exists():
                env_files.extend(list(TOKENS_DIR.glob("account_*.env")))
                env_files.extend(list(TOKENS_DIR.glob("*.env")))

            checked_keys = set()
            for env_path in env_files:
                if not (env_path.exists() and env_path.is_file()):
                    continue
                try:
                    jsid, vk, access_token, refresh_token = "", "", "", ""
                    with open(env_path, 'r', encoding='utf-8') as f:
                        for line in f:
                            if "MOVISTAR_JSESSIONID=" in line:
                                jsid = line.split("=", 1)[1].strip().strip('"').strip("'")
                            elif "MOVISTAR_VALIDATIONKEY=" in line:
                                vk = line.split("=", 1)[1].strip().strip('"').strip("'")
                            elif "MOVISTAR_ACCESS_TOKEN=" in line:
                                access_token = line.split("=", 1)[1].strip().strip('"').strip("'")
                            elif "MOVISTAR_REFRESH_TOKEN=" in line:
                                refresh_token = line.split("=", 1)[1].strip().strip('"').strip("'")

                    session_key = refresh_token or vk
                    if not session_key or session_key in checked_keys:
                        continue
                    checked_keys.add(session_key)

                    s = requests.Session()
                    if access_token and refresh_token:
                        s.headers.update({
                            "User-Agent": "MovistarCloud-KeepAlive/1.0",
                            "Authorization": build_oauth_auth_header(access_token, refresh_token)
                        })
                        url = "https://micloud.movistar.es/sapi/system/information?action=get"
                    elif jsid and vk:
                        s.cookies.set("JSESSIONID", jsid, domain="micloud.movistar.es", path="/")
                        s.cookies.set("validationkey", vk, domain="micloud.movistar.es", path="/")
                        s.headers.update({"User-Agent": "MovistarCloud-KeepAlive/1.0"})
                        url = f"https://micloud.movistar.es/sapi/system/information?action=get&validationkey={vk}"
                    else:
                        continue

                    r = s.get(url, timeout=15)
                    if r.status_code == 200:
                        app.logger.debug(f"Keep-Alive heartbeat OK para {env_path.name}")
                    elif r.status_code == 401 and refresh_token:
                        try:
                            refresh_oauth_token(refresh_token)
                        except Exception:
                            pass
                except Exception:
                    pass
        except Exception as e:
            app.logger.debug(f"Keep-Alive ping error: {e}")

KEEPALIVE_THREAD = threading.Thread(target=keepalive_worker, daemon=True)
KEEPALIVE_THREAD.start()

def kill_process_tree(proc):
    """
    Termina de forma limpia y forzosa un proceso y todo su grupo/árbol de procesos.
    Utiliza killpg en Linux/Docker con SIGTERM y escalado a SIGKILL, con fallback para Windows.
    """
    if proc is None:
        return
    try:
        pid = proc.pid
        if hasattr(os, "killpg") and hasattr(os, "getpgid"):
            try:
                pgid = os.getpgid(pid)
                os.killpg(pgid, signal.SIGTERM)
            except (ProcessLookupError, OSError):
                return
            try:
                proc.wait(timeout=2)
            except (subprocess.TimeoutExpired, Exception):
                try:
                    os.killpg(pgid, signal.SIGKILL)
                except (ProcessLookupError, OSError):
                    pass
                try:
                    proc.wait(timeout=1)
                except Exception:
                    pass
        else:
            proc.terminate()
            try:
                proc.wait(timeout=2)
            except (subprocess.TimeoutExpired, Exception):
                try:
                    proc.kill()
                    proc.wait(timeout=1)
                except Exception:
                    pass
    except Exception as e:
        app.logger.error(f"Error terminando proceso: {e}")

def read_upload_logs(proc):
    global UPLOAD_PROCESS, UPLOAD_LOGS
    try:
        for line in proc.stdout:
            clean_line = line.rstrip()
            UPLOAD_LOGS.append(clean_line)
            print(clean_line, flush=True)  # Muestra en vivo en docker logs / Portainer
            if len(UPLOAD_LOGS) > 1000:
                UPLOAD_LOGS.pop(0)
        proc.wait()
        end_msg = f"--- Proceso finalizado con código {proc.returncode} ---"
        UPLOAD_LOGS.append(end_msg)
        print(end_msg, flush=True)
    except Exception as e:
        err_msg = f"Error en proceso: {str(e)}"
        UPLOAD_LOGS.append(err_msg)
        print(err_msg, flush=True)
    finally:
        with UPLOAD_LOCK:
            if UPLOAD_PROCESS == proc:
                UPLOAD_PROCESS = None

@app.route("/status", methods=["GET"])
def status():
    global UPLOAD_PROCESS, ACTIVE_UPLOAD_PHONE
    is_running = UPLOAD_PROCESS is not None and UPLOAD_PROCESS.poll() is None
    return jsonify({
        "status": "running" if is_running else "stopped",
        "phone": ACTIVE_UPLOAD_PHONE or "",
        "is_running": is_running,
        "version": APP_VERSION
    })

@app.route("/version", methods=["GET"])
def version():
    return jsonify({"version": APP_VERSION})

@app.route("/health", methods=["GET"])
def health():
    return jsonify({"status": "ok", "version": APP_VERSION})

@app.route("/favicon.ico")
def favicon():
    return send_from_directory(os.path.join(app.root_path, "static"), "favicon.ico", mimetype="image/vnd.microsoft.icon")

@app.route("/")
def index():
    # Se renderiza la interfaz limpia sin filtrar credenciales en el HTML inicial
    return render_template("index.html", version=APP_VERSION)

@app.route("/verify_phone", methods=["POST"])
def verify_phone():
    global ACTIVE_UPLOAD_PHONE, UPLOAD_PROCESS
    data = request.json or {}
    raw_phone = data.get("phone", "").strip()
    phone = sanitize_phone(raw_phone)
    if not phone or len(phone) < 6:
        return jsonify({"status": "error", "message": "Número de teléfono no válido"}), 400

    env = get_env_vars(phone)
    jsid = env.get("MOVISTAR_JSESSIONID", "")
    vk = env.get("MOVISTAR_VALIDATIONKEY", "")
    refresh_token = env.get("MOVISTAR_REFRESH_TOKEN", "")
    device_id = env.get("MOVISTAR_DEVICE_ID", "")

    # Chequeo de subidas en curso
    is_running = UPLOAD_PROCESS is not None and UPLOAD_PROCESS.poll() is None
    is_running_this = is_running and (ACTIVE_UPLOAD_PHONE == phone)
    another_running = is_running and (ACTIVE_UPLOAD_PHONE != phone)

    return jsonify({
        "status": "ok",
        "phone": phone,
        "has_credentials": bool(refresh_token or (jsid and vk)),
        "has_device_token": bool(refresh_token),
        "device_id": device_id,
        "jsessionid": jsid,
        "validationkey": vk,
        "is_running_this": is_running_this,
        "another_running": another_running,
        "active_running_phone": ACTIVE_UPLOAD_PHONE if another_running else None
    })

@app.route("/start", methods=["POST"])
def start():
    global UPLOAD_PROCESS, UPLOAD_LOGS, ACTIVE_UPLOAD_PHONE
    with UPLOAD_LOCK:
        data = request.json or {}
        phone = sanitize_phone(data.get("phone", ""))
        if not phone:
            return jsonify({"status": "error", "message": "Debes especificar un número móvil para la cuenta"}), 400

        is_running = UPLOAD_PROCESS is not None and UPLOAD_PROCESS.poll() is None
        if is_running:
            if ACTIVE_UPLOAD_PHONE != phone:
                return jsonify({
                    "status": "error", 
                    "message": f"Ya hay una subida en curso para el número {ACTIVE_UPLOAD_PHONE}. Espera a que termine o cancélala antes de iniciar otra cuenta."
                }), 400
            else:
                # Si ya está corriendo para esta cuenta, detenemos el proceso anterior para reiniciar con las nuevas credenciales
                kill_process_tree(UPLOAD_PROCESS)
                UPLOAD_PROCESS = None

        path = data.get("path", "").strip()
        recursive = data.get("recursive", False)
        workers = 3  # Fijo a 3 hilos concurrentes para estabilidad óptima con la API de Movistar
        
        jsid = data.get("jsessionid", "").strip()
        vkey = data.get("validationkey", "").strip()
        takeout = data.get("takeout", False)

        existing_env = get_env_vars(phone)
        has_device_token = bool(existing_env.get("MOVISTAR_REFRESH_TOKEN"))

        if not path:
            return jsonify({"status": "error", "message": "La ruta no puede estar vacía"}), 400
        if not has_device_token and (not jsid or not vkey):
            return jsonify({"status": "error", "message": "Faltan las credenciales de sesión. Por favor, pulsa 'Vincular Dispositivo (Login SMS Oficial)' para generar tus tokens permanentes."}), 400

        if jsid and vkey:
            save_env_vars(jsid, vkey, phone=phone)

        proc_env = os.environ.copy()
        proc_env.update(get_env_vars(phone))
        if jsid:
            proc_env["MOVISTAR_JSESSIONID"] = jsid
        if vkey:
            proc_env["MOVISTAR_VALIDATIONKEY"] = vkey
        proc_env["MOVISTAR_PHONE"] = phone

        cmd = [
            "python", "-u", "/app/webdav_proxy.py"
        ]

        UPLOAD_LOGS.clear()
        UPLOAD_LOGS.append(f"$ {' '.join(cmd)}")
        ACTIVE_UPLOAD_PHONE = phone

        try:
            proc = subprocess.Popen(
                cmd, 
                stdout=subprocess.PIPE, 
                stderr=subprocess.STDOUT, 
                text=True,
                bufsize=1,
                env=proc_env,
                start_new_session=True
            )
            UPLOAD_PROCESS = proc
        except Exception as e:
            UPLOAD_LOGS.append(f"Error al iniciar proceso: {e}")
            ACTIVE_UPLOAD_PHONE = None
            return jsonify({"status": "error", "message": f"Error al iniciar proceso: {e}"}), 500

        thread = threading.Thread(target=read_upload_logs, args=(proc,))
        thread.daemon = True
        thread.start()

        return jsonify({"status": "ok", "message": "Subida iniciada"})

def stop_proxy_internal():
    global UPLOAD_PROCESS, UPLOAD_LOGS, ACTIVE_UPLOAD_PHONE
    with UPLOAD_LOCK:
        proc = UPLOAD_PROCESS
        if proc is not None and proc.poll() is None:
            UPLOAD_LOGS.append("--- Cancelando subida por el usuario... ---")
            kill_process_tree(proc)
            UPLOAD_LOGS.append("--- Proceso abortado por el usuario ---")
            UPLOAD_PROCESS = None
            ACTIVE_UPLOAD_PHONE = None
            return True
        else:
            UPLOAD_PROCESS = None
            ACTIVE_UPLOAD_PHONE = None
            return False

@app.route("/stop", methods=["POST"])
def stop():
    if stop_proxy_internal():
        return jsonify({"status": "ok", "message": "Proceso detenido con éxito"})
    else:
        return jsonify({"status": "ok", "message": "No hay proceso activo"})

@app.route("/logs", methods=["GET"])
def logs():
    phone = sanitize_phone(request.args.get("phone", ""))
    is_running = UPLOAD_PROCESS is not None and UPLOAD_PROCESS.poll() is None
    
    if is_running and phone and ACTIVE_UPLOAD_PHONE and ACTIVE_UPLOAD_PHONE != phone:
        return jsonify({
            "logs": [f"--- Hay una subida en curso activa para otra cuenta ({ACTIVE_UPLOAD_PHONE}) ---"],
            "is_running": False,
            "running": False,
            "another_running": True,
            "active_running_phone": ACTIVE_UPLOAD_PHONE
        })

    return jsonify({
        "logs": UPLOAD_LOGS, 
        "is_running": is_running, 
        "running": is_running,
        "active_phone": ACTIVE_UPLOAD_PHONE,
        "another_running": False
    })

@app.route("/folders", methods=["GET"])
def get_folders():
    current_path = request.args.get("path", "/data")
    if not os.path.abspath(current_path).startswith("/data"):
        current_path = "/data"
        
    folders = []
    if os.path.exists(current_path):
        try:
            for entry in os.scandir(current_path):
                if entry.is_dir():
                    folders.append({"path": entry.path, "name": entry.name})
        except Exception:
            pass
            
    folders.sort(key=lambda x: x["name"].lower())
    parent = os.path.dirname(current_path) if current_path != "/data" else None
    return jsonify({"current_path": current_path, "parent": parent, "folders": folders})

@app.route("/debug_vnc", methods=["GET"])
def debug_vnc():
    status = get_vnc_status()
    logs = {}
    for log_file in ["/tmp/xvfb.log", "/tmp/fluxbox.log", "/tmp/x11vnc.log", "/tmp/websockify.log"]:
        try:
            with open(log_file, "r") as f:
                logs[log_file] = f.read()
        except Exception as e:
            logs[log_file] = str(e)
    return jsonify({"status": status, "logs": logs})

@app.route("/start_login", methods=["POST"])
def start_login():
    global LOGIN_PROCESS, LOGIN_PHONE
    data = request.json or {}
    phone = sanitize_phone(data.get("phone", ""))
    if not phone:
        return jsonify({"status": "error", "message": "Debes especificar un número móvil para el login"}), 400
    LOGIN_PHONE = phone

    ensure_vnc_services()

    # Detener el proxy WebDAV para que no compita por Chromium mientras el usuario introduce el SMS
    stop_proxy_internal()

    try:
        Path("/tmp/login_in_progress.lock").touch()
    except Exception:
        pass

    if LOGIN_PROCESS is not None and LOGIN_PROCESS.poll() is None:
        kill_process_tree(LOGIN_PROCESS)
        LOGIN_PROCESS = None
            
    cmd = ["python", "-u", "/app/movistar_login.py", "--phone", phone]
    LOGIN_PROCESS = subprocess.Popen(cmd, start_new_session=True)
    return jsonify({"status": "ok"})

@app.route("/stop_login", methods=["POST"])
def stop_login():
    global LOGIN_PROCESS
    try:
        Path("/tmp/login_in_progress.lock").unlink(missing_ok=True)
    except Exception:
        pass
    if LOGIN_PROCESS is not None and LOGIN_PROCESS.poll() is None:
        kill_process_tree(LOGIN_PROCESS)
        LOGIN_PROCESS = None
        return jsonify({"status": "ok", "message": "Login cancelado"})
    LOGIN_PROCESS = None
    return jsonify({"status": "ok", "message": "No había proceso de login activo"})

@app.route("/login_status", methods=["GET"])
def login_status():
    global LOGIN_PROCESS, LOGIN_PHONE
    if LOGIN_PROCESS is None:
        return jsonify({"status": "idle"})
    ret = LOGIN_PROCESS.poll()
    if ret is None:
        return jsonify({"status": "in_progress", "phone": LOGIN_PHONE})
    elif ret == 0:
        try:
            Path("/tmp/login_in_progress.lock").unlink(missing_ok=True)
        except Exception:
            pass
        return jsonify({"status": "completed", "phone": LOGIN_PHONE})
    else:
        try:
            Path("/tmp/login_in_progress.lock").unlink(missing_ok=True)
        except Exception:
            pass
        return jsonify({"status": "failed", "phone": LOGIN_PHONE, "code": ret})

@app.route("/env", methods=["GET"])
def get_env():
    phone = sanitize_phone(request.args.get("phone", ""))
    if not phone:
        return jsonify({"phone": "", "jsessionid": "", "validationkey": "", "has_device_token": False})
    env = get_env_vars(phone)
    has_dev = bool(env.get("MOVISTAR_REFRESH_TOKEN"))
    return jsonify({
        "phone": phone,
        "jsessionid": env.get("MOVISTAR_JSESSIONID", ""),
        "validationkey": env.get("MOVISTAR_VALIDATIONKEY", ""),
        "has_device_token": has_dev,
        "device_id": env.get("MOVISTAR_DEVICE_ID", "")
    })

@app.route("/storage_info", methods=["GET"])
def storage_info():
    """Fetch real account storage quota and SAPI limits from Movistar Cloud."""
    phone = sanitize_phone(request.args.get("phone", ""))
    cfg = load_account_config(phone)
    if not (cfg.get("refresh_token") or cfg.get("access_token") or cfg.get("jsessionid")):
        return jsonify({"status": "error", "message": "No credentials configured"}), 400

    try:
        client = MovistarFilesClient(
            jsessionid=cfg.get("jsessionid"),
            validationkey=cfg.get("validationkey"),
            refresh_token=cfg.get("refresh_token"),
            access_token=cfg.get("access_token"),
            device_id=cfg.get("device_id"),
            device_name=cfg.get("device_name"),
            token_path=cfg.get("token_path")
        )
        info = client.get_storage_space()
        used = info.get("used_bytes", 0)
        total = info.get("total_bytes", 0)
        free = info.get("free_bytes", 0)

        def format_size(bytes_val):
            if bytes_val <= 0:
                return "0 B"
            val = float(bytes_val)
            for unit in ["B", "KB", "MB", "GB", "TB"]:
                if val < 1024.0:
                    return f"{val:.2f} {unit}"
                val /= 1024.0
            return f"{val:.2f} PB"

        used_str = format_size(used)
        total_str = format_size(total) if total > 0 else "Unlimited"
        free_str = format_size(free) if total > 0 else "Unlimited"

        return jsonify({
            "status": "ok",
            "used_bytes": used,
            "total_bytes": total,
            "free_bytes": free,
            "used_str": used_str,
            "total_str": total_str,
            "free_str": free_str,
            "upload_endpoint": getattr(client, "upload_url", "https://upload.micloud.movistar.es"),
            "max_upload_size_mb": getattr(client, "max_upload_size_mb", 4096)
        })
    except Exception as e:
        return jsonify({"status": "error", "message": str(e)}), 500

def autostart_proxy():
    global UPLOAD_PROCESS, ACTIVE_UPLOAD_PHONE, UPLOAD_LOGS
    
    # Check if there's any token available
    phone = ""
    env_files = []
    if TOKENS_DIR.exists():
        env_files.extend(list(TOKENS_DIR.glob("account_*.env")))
        env_files.extend(list(TOKENS_DIR.glob("*.env")))
    
    for p in env_files:
        try:
            if p.is_file():
                with open(p, 'r', encoding='utf-8') as f:
                    content = f.read()
                    if "MOVISTAR_REFRESH_TOKEN=" in content or ("MOVISTAR_JSESSIONID=" in content and "MOVISTAR_VALIDATIONKEY=" in content):
                        phone = p.stem.replace("account_", "")
                        break
        except Exception:
            continue
            
    if phone:
        print(f"[*] Credenciales encontradas para {phone}. Iniciando proxy WebDAV automáticamente...")
        proc_env = os.environ.copy()
        proc_env.update(get_env_vars(phone))
        cmd = ["python", "-u", "/app/webdav_proxy.py"]
        UPLOAD_LOGS.append("--- Auto-iniciando proxy WebDAV al arrancar el contenedor ---")
        UPLOAD_LOGS.append(f"$ {' '.join(cmd)}")
        ACTIVE_UPLOAD_PHONE = phone
        
        try:
            proc = subprocess.Popen(
                cmd, 
                stdout=subprocess.PIPE, 
                stderr=subprocess.STDOUT, 
                text=True,
                bufsize=1,
                env=proc_env,
                start_new_session=True
            )
            UPLOAD_PROCESS = proc
            thread = threading.Thread(target=read_upload_logs, args=(proc,))
            thread.daemon = True
            thread.start()
        except Exception as e:
            UPLOAD_LOGS.append(f"Error al auto-iniciar: {e}")
            ACTIVE_UPLOAD_PHONE = None
    else:
        print("[*] No se encontraron credenciales válidas. Esperando inicio manual desde el panel web.")

def background_session_renewer():
    """Hilo en segundo plano que renueva la sesión web preventivamente cada 6 horas para que nunca caduque."""
    import time
    while True:
        time.sleep(6 * 3600)
        try:
            from movistar_login import renew_web_session
            from test_movistar_api import load_account_config
            cfg = load_account_config()
            target_phone = LOGIN_PHONE or cfg.get("phone")
            print(f"[AutoRenew] Ejecutando renovación preventiva programada cada 6h (móvil: {target_phone or 'autodetectando'})...", flush=True)
            renew_web_session(phone=target_phone)
        except Exception as e:
            print(f"[AutoRenew] Error en renovación programada: {e}", flush=True)

if __name__ == "__main__":
    ensure_vnc_services()
    autostart_proxy()
    renewer_th = threading.Thread(target=background_session_renewer, daemon=True)
    renewer_th.start()
    app.run(host="0.0.0.0", port=5000)
