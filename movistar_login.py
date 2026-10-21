import time
import signal
import os
import urllib.parse
from pathlib import Path
from playwright.sync_api import sync_playwright

from movistar_device_auth import (
    get_oauth_login_url,
    generate_device_id,
    exchange_code_for_tokens,
    register_device,
    sapi_login_oauth
)


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


def perform_movistar_login(phone=None):
    try:
        from vnc_helper import ensure_vnc_services
        ensure_vnc_services()
    except Exception:
        pass

    login_url = "https://micloud.movistar.es"
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
    
    with sync_playwright() as p:
        exec_path = "/usr/bin/chromium" if os.path.exists("/usr/bin/chromium") else None

        context = p.chromium.launch_persistent_context(
            user_data_dir=str(profile_dir),
            headless=False,
            executable_path=exec_path,
            args=["--start-maximized", "--no-sandbox", "--disable-dev-shm-usage"]
        )
        page = context.pages[0] if context.pages else context.new_page()
        
        # Interceptar navegación y respuestas para detectar el authorization code
        def check_url(url):
            nonlocal auth_code
            if auth_code:
                return
            if "code=" in url:
                parsed_q = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
                if "code" in parsed_q:
                    auth_code = parsed_q["code"][0]
                    print(f"[OAuth] ¡Authorization Code interceptado con éxito!", flush=True)
                    return
                parsed_f = urllib.parse.parse_qs(urllib.parse.urlparse(url).fragment)
                if "code" in parsed_f:
                    auth_code = parsed_f["code"][0]
                    print(f"[OAuth] ¡Authorization Code interceptado con éxito (fragment)!", flush=True)

        page.on("framenavigated", lambda frame: check_url(frame.url))
        
        # Navegamos a la URL oficial de login OAuth de Telefónica con PKCE
        page.goto(login_url)
        
        start_time = time.time()
        timeout_seconds = 600  # 10 minutos máximo
        
        while True:
            if time.time() - start_time > timeout_seconds:
                print("Tiempo de espera agotado (10 min). Cerrando navegador...", flush=True)
                browser.close()
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
                    if acc_token:
                        print("[SAPI] Inicializando sesión oficial en SAPI...", flush=True)
                        import requests
                        sapi_sess = requests.Session()
                        sapi_res = sapi_login_oauth(sapi_sess, acc_token, ref_token)
                        if sapi_res.get("validationkey"):
                            validationkey = sapi_res["validationkey"]
                            print(f"[SAPI] ✔ ValidationKey obtenida: {validationkey[:6]}...", flush=True)
                        if sapi_res.get("jsessionid"):
                            jsessionid = sapi_res["jsessionid"]
                            print(f"[SAPI] ✔ JSESSIONID obtenida: {jsessionid[:6]}...", flush=True)
                    break
                except Exception as e:
                    print(f"[OAuth] Error al canjear code: {e}", flush=True)
                    auth_code = None

            # Si ya tenemos tokens permanentes, salir con éxito
            if tokens_data and tokens_data.get("refresh_token"):
                break

            # Si ya tenemos ambas credenciales de España (fallback web), login completado
            if jsessionid and validationkey:
                print(f"[Login] ✔ Credenciales de Movistar España detectadas con éxito.", flush=True)
                break

            if len(context.pages) == 0:
                print("Se cerró el navegador antes de completar el login.", flush=True)
                return None
                
            time.sleep(1)
            
        context.close()

    return {
        "tokens": tokens_data,
        "jsessionid": jsessionid,
        "validationkey": validationkey
    }


def renew_web_session(phone=None):
    """
    Renueva la sesión web automáticamente en segundo plano usando el perfil persistente de Chromium.
    No requiere introducir SMS ya que la sesión persistente del navegador mantiene la autenticación.
    """
    profile_dir = Path("/app/tokens/browser_profile")
    if not profile_dir.exists():
        return False

    print("[AutoRenew] 🔄 Comprobando renovación automática de sesión web...", flush=True)
    try:
        with sync_playwright() as p:
            exec_path = "/usr/bin/chromium" if os.path.exists("/usr/bin/chromium") else None
            context = p.chromium.launch_persistent_context(
                user_data_dir=str(profile_dir),
                headless=True,
                executable_path=exec_path,
                args=["--no-sandbox", "--disable-dev-shm-usage"]
            )
            page = context.pages[0] if context.pages else context.new_page()
            page.goto("https://micloud.movistar.es", wait_until="networkidle", timeout=30000)

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

            if jsessionid and validationkey:
                save_account_env({"jsessionid": jsessionid, "validationkey": validationkey}, phone)
                print(f"[AutoRenew] ✔ Sesión web renovada automáticamente sin SMS. JSESSIONID: {jsessionid[:6]}...", flush=True)
                return True
            else:
                print("[AutoRenew] La sesión persistente ha expirado; se requiere login manual.", flush=True)
    except Exception as e:
        print(f"[AutoRenew] Error en renovación automática: {e}", flush=True)
    return False


def save_account_env(login_result: dict, phone: str):
    import re
    if not phone:
        print("Error: No se proporcionó número de teléfono para guardar credenciales.", flush=True)
        return

    phone_clean = re.sub(r"\D", "", str(phone).strip())
    if len(phone_clean) == 11 and phone_clean.startswith("34"):
        phone_clean = phone_clean[2:]

    tokens = login_result.get("tokens", {})
    refresh_token = tokens.get("refresh_token", "")
    access_token = tokens.get("access_token", "")
    jsid = login_result.get("jsessionid", "")
    vk = login_result.get("validationkey", "")

    # Reutilizar device_id si ya existía para este teléfono, o generar uno nuevo
    target = Path(f"/app/tokens/account_{phone_clean}.env")
    device_id = None
    if target.exists():
        try:
            for line in target.read_text(encoding='utf-8').splitlines():
                if "MOVISTAR_DEVICE_ID=" in line:
                    device_id = line.split("=", 1)[1].strip().strip('"').strip("'")
        except Exception:
            pass

    if not device_id:
        device_id = generate_device_id()

    device_name = f"Samsung SM-S928B (Proxy {phone_clean[-4:]})"

    content = f"""# Movistar Cloud Native Device Credentials
MOVISTAR_PHONE="{phone_clean}"
MOVISTAR_DEVICE_ID="{device_id}"
MOVISTAR_DEVICE_NAME="{device_name}"
MOVISTAR_REFRESH_TOKEN="{refresh_token}"
MOVISTAR_ACCESS_TOKEN="{access_token}"
MOVISTAR_JSESSIONID="{jsid}"
MOVISTAR_VALIDATIONKEY="{vk}"
"""
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content, encoding='utf-8')
        print(f"✔ Credenciales de dispositivo guardadas correctamente en: {target}", flush=True)
        print(f"  Device ID: {device_id} ({device_name})", flush=True)
        
        # Sincronizar también con /app/tokens/.env para máxima compatibilidad
        try:
            primary_env = Path("/app/tokens/.env")
            primary_env.write_text(content, encoding='utf-8')
        except Exception:
            pass

        if refresh_token:
            print("  Modo: DISPOSITIVO ANDROID NATIVO (Tokens permanentes)", flush=True)
            if access_token:
                try:
                    import requests
                    from movistar_device_auth import build_oauth_auth_header, register_device
                    reg_session = requests.Session()
                    reg_session.headers["Authorization"] = build_oauth_auth_header(access_token, refresh_token)
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
    else:
        print("\nNo se pudo completar el registro del dispositivo.", flush=True)
