import sys
import time
import signal
import os
import re
import urllib.parse
from pathlib import Path
import requests
from playwright.sync_api import sync_playwright

from movistar_device_auth import (
    get_oauth_login_url,
    generate_device_id,
    exchange_code_for_tokens,
    register_device,
    sapi_login_oauth
)


def atomic_write_text(file_path: Path, content: str, encoding: str = "utf-8"):
    """Escribe un archivo de forma atómica usando un archivo temporal y os.replace."""
    file_path = Path(file_path)
    file_path.parent.mkdir(parents=True, exist_ok=True)
    tmp_path = file_path.with_name(f"{file_path.name}.tmp.{os.getpid()}")
    try:
        tmp_path.write_text(content, encoding=encoding)
        os.replace(tmp_path, file_path)
    except Exception:
        if tmp_path.exists():
            try:
                tmp_path.unlink()
            except Exception:
                pass
        raise


def handle_sigterm(signum, frame):
    os._exit(0)


try:
    signal.signal(signal.SIGTERM, handle_sigterm)
    signal.signal(signal.SIGINT, handle_sigterm)
except Exception:
    pass

try:
    from vnc_helper import ensure_vnc_services
    ensure_vnc_services()
except Exception:
    pass


def cleanup_profile_locks(profile_dir):
    """Elimina ficheros de bloqueo residuales de Chromium si una sesión previa se cerró abruptamente."""
    try:
        p = Path(profile_dir)
        for lock_name in ["SingletonLock", "SingletonSocket", "SingletonCookie"]:
            lock_file = p / lock_name
            try:
                os.remove(str(lock_file))
            except Exception:
                try:
                    if lock_file.exists() or lock_file.is_symlink():
                        lock_file.unlink()
                except Exception:
                    pass
    except Exception:
        pass


def perform_movistar_login(phone=None):
    try:
        from vnc_helper import ensure_vnc_services
        ensure_vnc_services()
    except Exception:
        pass

    code_verifier, login_url = get_oauth_login_url()
    print("Iniciando navegador virtual para vinculación permanente con Movistar Cloud España...", flush=True)
    print("Por favor, introduce tu número de móvil y el código SMS recibido.", flush=True)
    print("Esta ventana se cerrará automáticamente en cuanto se complete la vinculación.", flush=True)
    
    os.environ["DISPLAY"] = ":99"
    
    auth_code = None
    jsessionid = None
    validationkey = None
    tokens_data = {}

    profile_dir = Path("/app/tokens/browser_profile")
    profile_dir.mkdir(parents=True, exist_ok=True)
    cleanup_profile_locks(profile_dir)
    
    with sync_playwright() as p:
        exec_path = "/usr/bin/chromium" if os.path.exists("/usr/bin/chromium") else None

        context = p.chromium.launch_persistent_context(
            user_data_dir=str(profile_dir),
            headless=False,
            executable_path=exec_path,
            args=["--start-maximized", "--no-sandbox", "--disable-dev-shm-usage"]
        )
        # Limpiar cookies residuales caducadas para no confundirlas con la nueva sesión
        try:
            context.clear_cookies()
        except Exception:
            pass

        page = context.pages[0] if context.pages else context.new_page()
        
        # Interceptar navegación y peticiones para detectar el authorization code
        def check_url(url):
            nonlocal auth_code
            if auth_code:
                return
            if not url:
                return
            if "code=" in url:
                parsed_q = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
                if "code" in parsed_q:
                    auth_code = parsed_q["code"][0]
                    print(f"[OAuth] ¡Authorization Code interceptado con éxito en query!", flush=True)
                    return
                parsed_f = urllib.parse.parse_qs(urllib.parse.urlparse(url).fragment)
                if "code" in parsed_f:
                    auth_code = parsed_f["code"][0]
                    print(f"[OAuth] ¡Authorization Code interceptado con éxito en fragment!", flush=True)
                    return

        page.on("framenavigated", lambda frame: check_url(frame.url))
        page.on("request", lambda req: check_url(req.url))
        page.on("response", lambda resp: check_url(resp.url))
        
        # Navegamos a la URL oficial de login OAuth de Telefónica con PKCE
        page.goto(login_url)
        
        start_time = time.time()
        timeout_seconds = 600  # 10 minutos máximo
        
        while True:
            if time.time() - start_time > timeout_seconds:
                print("Tiempo de espera agotado (10 min). Cerrando navegador...", flush=True)
                context.close()
                return None

            check_url(page.url)

            # Leer cookies de sesión
            cookies = context.cookies()
            for c in cookies:
                c_name = c.get('name', '').lower()
                if c_name == 'jsessionid':
                    jsessionid = c['value']
                elif c_name in ('validationkey', 'validation_key', 'vk'):
                    validationkey = c['value']

            # Extraer validationkey de URL y localStorage/sessionStorage
            if not validationkey:
                try:
                    current_url = page.url
                    if "validationkey=" in current_url:
                        parsed = urllib.parse.parse_qs(urllib.parse.urlparse(current_url).query)
                        if "validationkey" in parsed:
                            validationkey = parsed["validationkey"][0]

                    storage_vk = page.evaluate("() => localStorage.getItem('validationkey') || sessionStorage.getItem('validationkey') || ''")
                    if storage_vk:
                        validationkey = storage_vk
                except Exception:
                    pass

            # Si se intercepta authorization code
            if auth_code:
                try:
                    print("[OAuth] Canjeando Authorization Code con PKCE...", flush=True)
                    tokens_data = exchange_code_for_tokens(auth_code, code_verifier)
                    print("[OAuth] ✔ Tokens permanentes obtenidos con éxito.", flush=True)

                    acc_token = tokens_data.get("access_token", "")
                    ref_token = tokens_data.get("refresh_token", "")
                    exp_in = str(tokens_data.get("expires_in") or "3600")
                    if acc_token:
                        print("[SAPI] Inicializando sesión oficial en SAPI...", flush=True)
                        import requests
                        sapi_sess = requests.Session()
                        sapi_res = sapi_login_oauth(
                            sapi_sess,
                            acc_token,
                            ref_token,
                            msisdn=phone,
                            last_refresh_date=0,
                            expires_in=exp_in
                        )
                        if sapi_res.get("refreshed_oauth"):
                            ro = sapi_res["refreshed_oauth"]
                            if ro.get("access_token"):
                                tokens_data["access_token"] = ro["access_token"]
                            if ro.get("refresh_token"):
                                tokens_data["refresh_token"] = ro["refresh_token"]
                            if ro.get("expires_in"):
                                tokens_data["expires_in"] = ro["expires_in"]
                            if ro.get("last_refresh_date") is not None:
                                tokens_data["last_refresh_date"] = ro["last_refresh_date"]
                            print("[SAPI] ✔ Tokens OAuth rotados en tránsito por SAPI capturados.", flush=True)
                        elif sapi_res.get("last_refresh_date"):
                            tokens_data["last_refresh_date"] = sapi_res["last_refresh_date"]
                        if sapi_res.get("validationkey"):
                            validationkey = sapi_res["validationkey"]
                            print(f"[SAPI] ✔ ValidationKey obtenida: {validationkey[:6]}...", flush=True)
                        if sapi_res.get("jsessionid"):
                            jsessionid = sapi_res["jsessionid"]
                            print(f"[SAPI] ✔ JSESSIONID obtenida: {jsessionid[:6]}...", flush=True)

                        # Si SAPI no devolvió validationkey directamente, extraerla también del contexto del navegador
                        if not validationkey or not jsessionid:
                            for c in context.cookies():
                                c_name = c.get('name', '').lower()
                                if c_name == 'jsessionid' and not jsessionid:
                                    jsessionid = c['value']
                                elif c_name in ('validationkey', 'validation_key', 'vk') and not validationkey:
                                    validationkey = c['value']
                    break
                except Exception as e:
                    print(f"[OAuth] Error al canjear code: {e}", flush=True)
                    auth_code = None

            # Si ya tenemos tokens permanentes, salir con éxito
            if tokens_data and tokens_data.get("refresh_token"):
                break

            curr_url = page.url.lower()
            is_on_login = ("segu-loginapp" in curr_url or "t3.movistar.es" in curr_url or "action=login" in curr_url or "/login" in curr_url or "oauth" in curr_url)

            # Si ya tenemos ambas credenciales de España Y no estamos en la pantalla de login, validar en SAPI
            if not is_on_login and jsessionid and validationkey:
                import requests
                test_sess = requests.Session()
                test_sess.cookies.set("JSESSIONID", jsessionid, domain="micloud.movistar.es", path="/")
                test_sess.cookies.set("validationkey", validationkey, domain="micloud.movistar.es", path="/")
                test_url = f"https://micloud.movistar.es/sapi/media/folder/root?action=get&validationkey={validationkey}"
                try:
                    v_res = test_sess.get(test_url, timeout=5)
                    if v_res.status_code == 200:
                        print(f"[Login] ✔ Credenciales de Movistar España verificadas con éxito en SAPI.", flush=True)
                        break
                except Exception:
                    pass

            if len(context.pages) == 0:
                print("Se cerró el navegador antes de completar el login.", flush=True)
                return None
                
            time.sleep(1)
            
        context.close()
    cleanup_profile_locks(profile_dir)

    return {
        "tokens": tokens_data,
        "jsessionid": jsessionid,
        "validationkey": validationkey
    }


def detect_account_phone(phone=None):
    """Detecta el número de teléfono móvil buscando en argumentos, variables de entorno o archivos de tokens."""
    import re
    if phone:
        p_clean = re.sub(r"\D", "", str(phone).strip())
        if len(p_clean) == 11 and p_clean.startswith("34"):
            p_clean = p_clean[2:]
        if p_clean:
            return p_clean

    env_phone = os.environ.get("MOVISTAR_PHONE")
    if env_phone:
        p_clean = re.sub(r"\D", "", str(env_phone).strip())
        if len(p_clean) == 11 and p_clean.startswith("34"):
            p_clean = p_clean[2:]
        if p_clean:
            return p_clean

    for t_dir in [Path("/app/tokens"), Path("tokens"), Path(".")]:
        if t_dir.exists():
            acc_files = sorted(list(t_dir.glob("account_*.env")))
            for f in acc_files:
                m = re.search(r"account_(\d+)\.env", f.name)
                if m:
                    return m.group(1)

    for env_path in [Path("/app/tokens/.env"), Path("tokens/.env"), Path(".env")]:
        if env_path.exists():
            try:
                for line in env_path.read_text(encoding="utf-8").splitlines():
                    if line.startswith("MOVISTAR_PHONE="):
                        val = line.split("=", 1)[1].strip().strip('"').strip("'")
                        p_clean = re.sub(r"\D", "", val)
                        if len(p_clean) == 11 and p_clean.startswith("34"):
                            p_clean = p_clean[2:]
                        if p_clean:
                            return p_clean
            except Exception:
                pass
    return None


def renew_web_session(phone=None):
    """
    Renueva la sesión web automáticamente en segundo plano usando el perfil persistente de Chromium.
    No requiere introducir SMS ya que la sesión persistente del navegador mantiene la autenticación.
    """
    if Path("/tmp/login_in_progress.lock").exists():
        print("[AutoRenew] ⏳ Login interactivo en curso por el usuario. Saltando renovación en segundo plano.", flush=True)
        return False

    profile_dir = Path("/app/tokens/browser_profile")
    if not profile_dir.exists():
        return False

    lock_fd = None
    try:
        import fcntl
        lock_fd = open("/tmp/movistar_renew_web.lock", "w")
        fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        print("[AutoRenew] ⏳ Otra instancia ya está renovando la sesión web. Esperando resultado...", flush=True)
        try:
            if lock_fd:
                fcntl.flock(lock_fd, fcntl.LOCK_EX)
                fcntl.flock(lock_fd, fcntl.LOCK_UN)
                lock_fd.close()
        except Exception:
            pass
        return False
    except Exception:
        lock_fd = None

    try:
        cleanup_profile_locks(profile_dir)
        phone = detect_account_phone(phone)
        print(f"[AutoRenew] 🔄 Comprobando renovación automática de sesión web (móvil: {phone or 'autodetectando'})...", flush=True)
        with sync_playwright() as p:
            exec_path = "/usr/bin/chromium" if os.path.exists("/usr/bin/chromium") else None
            context = p.chromium.launch_persistent_context(
                user_data_dir=str(profile_dir),
                headless=True,
                executable_path=exec_path,
                args=["--no-sandbox", "--disable-dev-shm-usage"]
            )
            page = context.pages[0] if context.pages else context.new_page()
            try:
                page.goto("https://micloud.movistar.es", wait_until="networkidle", timeout=30000)
            except Exception as e:
                print(f"[AutoRenew] Aviso carga inicial: {e}", flush=True)

            curr_url = page.url.lower()
            if "segu-loginapp" in curr_url or "t3.movistar.es" in curr_url or "action=login" in curr_url or "/login" in curr_url:
                print("[AutoRenew] ⚠️ La sesión web en Movistar ha caducado por completo (redirigido a pantalla de login). Se requiere pulsar '🔑 Vincular Dispositivo' en el panel web.", flush=True)
                context.close()
                cleanup_profile_locks(profile_dir)
                return False

            jsessionid = None
            validationkey = None
            for c in context.cookies():
                c_name = c.get('name', '').lower()
                if c_name == 'jsessionid':
                    jsessionid = c['value']
                elif c_name in ('validationkey', 'validation_key', 'vk'):
                    validationkey = c['value']

            if not validationkey:
                try:
                    vk = page.evaluate("() => localStorage.getItem('validationkey') || sessionStorage.getItem('validationkey') || ''")
                    if vk:
                        validationkey = vk
                except Exception:
                    pass

            context.close()
            cleanup_profile_locks(profile_dir)

            if jsessionid and validationkey:
                # Validar credenciales de forma real contra la API SAPI de Movistar antes de darlas por buenas
                import requests
                test_sess = requests.Session()
                test_sess.cookies.set("JSESSIONID", jsessionid, domain="micloud.movistar.es", path="/")
                test_sess.cookies.set("validationkey", validationkey, domain="micloud.movistar.es", path="/")
                test_url = f"https://micloud.movistar.es/sapi/media/folder/root?action=get&validationkey={validationkey}"
                try:
                    v_res = test_sess.get(test_url, timeout=15)
                    if v_res.status_code != 200:
                        print(f"[AutoRenew] ⚠️ Las credenciales obtenidas no son válidas en SAPI (HTTP {v_res.status_code}). Se requiere nuevo login manual.", flush=True)
                        return False
                except Exception as e:
                    print(f"[AutoRenew] Error validando con SAPI: {e}", flush=True)
                    return False

                save_account_env({"jsessionid": jsessionid, "validationkey": validationkey}, phone)
                print(f"[AutoRenew] ✔ Sesión web validada y renovada con éxito. JSESSIONID: {jsessionid[:6]}...", flush=True)
                return True
            else:
                print("[AutoRenew] La sesión persistente ha expirado; se requiere login manual.", flush=True)
    except Exception as e:
        print(f"[AutoRenew] Error en renovación automática: {e}", flush=True)
    finally:
        if lock_fd:
            try:
                import fcntl
                fcntl.flock(lock_fd, fcntl.LOCK_UN)
                lock_fd.close()
            except Exception:
                pass
    return False


def save_account_env(login_result: dict, phone: str = None):
    phone_clean = detect_account_phone(phone)
    if not phone_clean:
        print("Error: No se proporcionó ni se pudo autodetectar el número de teléfono para guardar credenciales.", flush=True)
        return

    tokens = login_result.get("tokens", {})
    refresh_token = tokens.get("refresh_token", "")
    access_token = tokens.get("access_token", "")
    expires_in = str(tokens.get("expires_in") or "3600")
    last_refresh_date = int(tokens.get("last_refresh_date") or 0)
    jsid = login_result.get("jsessionid", "")
    vk = login_result.get("validationkey", "")

    # Reutilizar device_id y tokens existentes si ya existían para este teléfono
    target = Path(f"/app/tokens/account_{phone_clean}.env")
    device_id = None
    existing_ref = ""
    existing_acc = ""
    existing_lrd = 0
    existing_exp = "3600"
    if target.exists():
        try:
            for line in target.read_text(encoding='utf-8').splitlines():
                if "MOVISTAR_DEVICE_ID=" in line:
                    device_id = line.split("=", 1)[1].strip().strip('"').strip("'")
                elif "MOVISTAR_REFRESH_TOKEN=" in line:
                    existing_ref = line.split("=", 1)[1].strip().strip('"').strip("'")
                elif "MOVISTAR_ACCESS_TOKEN=" in line:
                    existing_acc = line.split("=", 1)[1].strip().strip('"').strip("'")
                elif "MOVISTAR_LAST_REFRESH_DATE=" in line:
                    try:
                        existing_lrd = int(line.split("=", 1)[1].strip().strip('"').strip("'"))
                    except (ValueError, TypeError):
                        existing_lrd = 0
                elif "MOVISTAR_EXPIRES_IN=" in line:
                    existing_exp = line.split("=", 1)[1].strip().strip('"').strip("'") or "3600"
        except Exception:
            pass

    if not device_id or device_id.startswith("fac-"):
        device_id = generate_device_id()

    if not refresh_token and existing_ref:
        refresh_token = existing_ref
    if not access_token and existing_acc:
        access_token = existing_acc
    if not last_refresh_date and existing_lrd:
        last_refresh_date = existing_lrd
    if not tokens.get("expires_in") and existing_exp:
        expires_in = existing_exp

    device_name = f"DESKTOP-MOVISTAR-{phone_clean[-4:]}"

    content = f"""# Movistar Cloud Native Device Credentials
MOVISTAR_PHONE="{phone_clean}"
MOVISTAR_DEVICE_ID="{device_id}"
MOVISTAR_DEVICE_NAME="{device_name}"
MOVISTAR_REFRESH_TOKEN="{refresh_token}"
MOVISTAR_ACCESS_TOKEN="{access_token}"
MOVISTAR_LAST_REFRESH_DATE="{last_refresh_date}"
MOVISTAR_EXPIRES_IN="{expires_in}"
MOVISTAR_JSESSIONID="{jsid}"
MOVISTAR_VALIDATIONKEY="{vk}"
"""
    try:
        atomic_write_text(target, content, encoding='utf-8')
        print(f"✔ Credenciales de dispositivo guardadas correctamente en: {target}", flush=True)
        print(f"  Device ID: {device_id} ({device_name})", flush=True)
        
        # Sincronizar también con /app/tokens/.env para máxima compatibilidad
        try:
            primary_env = Path("/app/tokens/.env")
            atomic_write_text(primary_env, content, encoding='utf-8')
        except Exception:
            pass

        if refresh_token:
            print("  Modo: DISPOSITIVO ANDROID NATIVO (Tokens permanentes)", flush=True)
            if access_token:
                try:
                    import requests
                    from movistar_device_auth import build_oauth_auth_header, register_device
                    reg_session = requests.Session()
                    reg_session.headers["Authorization"] = build_oauth_auth_header(
                        access_token,
                        refresh_token,
                        last_refresh_date=last_refresh_date,
                        expires_in=expires_in
                    )
                    reg_session.headers["X-deviceid"] = device_id
                    reg_session.headers["X-devicename"] = device_name
                    reg_ok = register_device(reg_session, device_id, device_name)
                    if reg_ok:
                        print("  ✔ Dispositivo registrado formalmente en Movistar Cloud (SAPI).", flush=True)
                except Exception as e:
                    print(f"  Aviso registro dispositivo SAPI: {e}", flush=True)
        else:
            print("  Modo: Sesión Web (Fallback)", flush=True)
    except Exception as e:
        print(f"Error al guardar credenciales: {e}", flush=True)


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument("--phone", type=str, default=None, help="Número de teléfono móvil asociado")
    args, _ = parser.parse_known_args()

    res = perform_movistar_login(phone=args.phone)
    if res and (res.get("tokens") or (res.get("jsessionid") and res.get("validationkey"))):
        save_account_env(res, phone=args.phone)
        print("\n¡Dispositivo registrado con éxito! El servicio WebDAV se iniciará automáticamente.", flush=True)
        sys.exit(0)
    else:
        print("\nNo se pudo completar el registro del dispositivo.", flush=True)
        sys.exit(1)
