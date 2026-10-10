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
    register_device
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

    login_url = get_oauth_login_url()
    print("Iniciando navegador virtual para registro de dispositivo Android nativo...", flush=True)
    print("Por favor, introduce tu número de móvil y el código SMS recibido.", flush=True)
    print("Esta ventana se cerrará automáticamente en cuanto se complete la vinculación.", flush=True)
    
    os.environ["DISPLAY"] = ":99"
    
    auth_code = None
    jsessionid = None
    validationkey = None
    tokens_data = {}
    
    with sync_playwright() as p:
        exec_path = "/usr/bin/chromium" if os.path.exists("/usr/bin/chromium") else None

        browser = p.chromium.launch(
            headless=False,
            executable_path=exec_path,
            args=["--start-maximized", "--no-sandbox", "--disable-dev-shm-usage"]
        )
        context = browser.new_context(no_viewport=True)
        page = context.new_page()
        
        # Interceptar navegación y respuestas para detectar el authorization code
        def check_url(url):
            nonlocal auth_code
            if "clientoauth.html" in url and "code=" in url:
                parsed = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
                if "code" in parsed:
                    auth_code = parsed["code"][0]
                    print(f"[OAuth] ¡Authorization Code interceptado con éxito!", flush=True)

        page.on("framenavigated", lambda frame: check_url(frame.url))
        
        # Navegamos a la URL de autorización nativa OAuth
        page.goto(login_url)
        
        start_time = time.time()
        timeout_seconds = 600  # 10 minutos máximo
        
        while True:
            if time.time() - start_time > timeout_seconds:
                print("Tiempo de espera agotado (10 min). Cerrando navegador...", flush=True)
                browser.close()
                return None

            check_url(page.url)

            # Leer cookies como fallback
            cookies = context.cookies()
            for c in cookies:
                c_name = c.get('name', '').lower()
                if c_name == 'jsessionid':
                    jsessionid = c['value']
                elif c_name in ('validationkey', 'validation_key', 'vk'):
                    validationkey = c['value']

            # Si ya tenemos el authorization code, podemos proceder
            if auth_code:
                try:
                    print("[OAuth] Canjeando Authorization Code por Refresh Token de dispositivo Android...", flush=True)
                    tokens_data = exchange_code_for_tokens(auth_code)
                    print("[OAuth] ✔ Tokens de dispositivo obtenidos con éxito (Refresh Token permanente activo).", flush=True)
                    break
                except Exception as e:
                    print(f"[OAuth] Error al canjear code: {e}", flush=True)
                    auth_code = None  # Reintentar si falla

            # Fallback secundario: si se logueó por web y tenemos cookies completas
            if not auth_code and jsessionid and validationkey:
                # Comprobar si además logramos extraer el code de la URL
                current_url = page.url
                check_url(current_url)
                if auth_code:
                    continue
                # Si pasaron más de 5 segundos con cookies web pero sin code, permitir fallback
                if time.time() - start_time > 15:
                    print("[Web] Credenciales de sesión web detectadas (fallback).", flush=True)
                    break

            if len(context.pages) == 0:
                print("Se cerró el navegador antes de completar el login.", flush=True)
                return None
                
            time.sleep(1)
            
        browser.close()

    return {
        "tokens": tokens_data,
        "jsessionid": jsessionid,
        "validationkey": validationkey
    }


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
        if refresh_token:
            print("  Modo: DISPOSITIVO ANDROID NATIVO (Tokens permanentes)", flush=True)
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
