import os
import time
import json
import base64
import secrets
import requests

OAUTH_AUTHORIZE_URL = "https://miportal.proteccion.movistar.com/oauth/authorize"
OAUTH_TOKEN_URL = "https://miportal.proteccion.movistar.com/oauth/token"
OAUTH_CLIENT_ID = "e525465e-598d-4a5f-b53c-5171285df751"
OAUTH_CLIENT_SECRET = "d23cee48-b60c-4184-8395-52f590975e86"
OAUTH_REDIRECT_URI = "https://micloud.proteccion.movistar.com/ui/html/clientoauth.html"

ANDROID_USER_AGENT = "omh android client 31.1.2026011416.0"
SAPI_BASE_URL = "https://micloud.movistar.es/sapi"


def get_oauth_login_url():
    """Devuelve la URL de login OAuth oficial emulando la app Android de Movistar Cloud."""
    import urllib.parse
    params = {
        "response_type": "code",
        "client_id": OAUTH_CLIENT_ID,
        "redirect_uri": OAUTH_REDIRECT_URI,
        "scope": "public"
    }
    return f"{OAUTH_AUTHORIZE_URL}?{urllib.parse.urlencode(params)}"


def generate_device_id():
    """Genera un device ID nativo Android con prefijo 'fac-' y 16 caracteres hexadecimales."""
    return f"fac-{secrets.token_hex(8)}"


def exchange_code_for_tokens(auth_code: str):
    """Canjea el authorization code por access_token y refresh_token."""
    auth_header = "Basic " + base64.b64encode(f"{OAUTH_CLIENT_ID}:{OAUTH_CLIENT_SECRET}".encode("utf-8")).decode("utf-8")
    headers = {
        "Authorization": auth_header,
        "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8",
        "User-Agent": ANDROID_USER_AGENT
    }
    data = {
        "grant_type": "authorization_code",
        "code": auth_code,
        "redirect_uri": OAUTH_REDIRECT_URI,
        "client_id": OAUTH_CLIENT_ID,
        "client_secret": OAUTH_CLIENT_SECRET
    }
    
    res = requests.post(OAUTH_TOKEN_URL, headers=headers, data=data, timeout=30)
    if res.status_code != 200:
        raise Exception(f"OAuth token exchange failed ({res.status_code}): {res.text}")
        
    return res.json()


def refresh_oauth_token(refresh_token: str):
    """Renueva el access_token usando el refresh_token permanente."""
    auth_header = "Basic " + base64.b64encode(f"{OAUTH_CLIENT_ID}:{OAUTH_CLIENT_SECRET}".encode("utf-8")).decode("utf-8")
    headers = {
        "Authorization": auth_header,
        "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8",
        "User-Agent": ANDROID_USER_AGENT
    }
    data = {
        "grant_type": "refresh_token",
        "refresh_token": refresh_token,
        "client_id": OAUTH_CLIENT_ID,
        "client_secret": OAUTH_CLIENT_SECRET
    }
    
    res = requests.post(OAUTH_TOKEN_URL, headers=headers, data=data, timeout=30)
    if res.status_code != 200:
        raise Exception(f"OAuth token refresh failed ({res.status_code}): {res.text}")
        
    return res.json()


def build_oauth_auth_header(access_token: str, refresh_token: str = "") -> str:
    """
    Construye la cabecera 'Authorization: oauth <base64>' propietaria de Synchronoss/Funambol
    utilizada por la app de Android.
    """
    payload = {
        "data": {
            "accesstoken": access_token,
            "refreshtoken": refresh_token,
            "platform": "android",
            "expiresin": "3600",
            "lastrefreshdate": int(time.time() * 1000),
            "msisdn": ""
        }
    }
    raw_json = json.dumps(payload, separators=(',', ':')).encode("utf-8")
    b64_val = base64.b64encode(raw_json).decode("utf-8")
    return f"oauth {b64_val}"


def register_device(session: requests.Session, device_id: str, device_name: str, user_id: str = ""):
    """Registra el dispositivo Android en Movistar Cloud para persistencia a largo plazo."""
    url = f"{SAPI_BASE_URL}/profile/device?action=add&responsetime=true"
    if user_id:
        url += f"&userid={user_id}"
        
    payload = {
        "data": {
            "carrierid": "10019",
            "modelid": "3482",
            "countrya2": "ES",
            "platform": "android",
            "name": device_name,
            "deviceid": device_id
        }
    }
    try:
        res = session.post(url, json=payload, headers={"Content-Type": "application/json;charset=UTF-8"}, timeout=20)
        return res.status_code in (200, 201)
    except Exception:
        return False
