import os
import time
import json
import base64
import secrets
import hashlib
import urllib.parse
import requests

# Endpoints oficiales de Movistar Cloud España (obtenidos del cliente oficial)
OAUTH_AUTHORIZE_URL = "https://apiseg.telefonica.es/openid/connect/cus/col/auth/oauth/v2/authorize"
OAUTH_TOKEN_URL = "https://apiseg.telefonica.es/openid/connect/cus/col/auth/oauth/v2/token"
OAUTH_CLIENT_ID = "b99e5095-7a36-42c1-83b6-6f05d0f13ea1"
OAUTH_CLIENT_SECRET = "e22fdd6b-636a-4bfc-bb4a-aa8ed6d131d8"
OAUTH_REDIRECT_URI = "https://micloud.movistar.es/ui/html/clientoauth.html"

ANDROID_USER_AGENT = "omh android client 31.1.2026011416.0"
SAPI_BASE_URL = "https://micloud.movistar.es/sapi"


def generate_device_id():
    """Genera un device ID nativo Android con prefijo 'fac-' y 16 caracteres hexadecimales."""
    return f"fac-{secrets.token_hex(8)}"


def generate_pkce_pair():
    """Genera el par criptográfico PKCE (code_verifier y code_challenge S256)."""
    code_verifier = secrets.token_urlsafe(64)[:64]
    digest = hashlib.sha256(code_verifier.encode("ascii")).digest()
    code_challenge = base64.urlsafe_b64encode(digest).decode("ascii").rstrip("=")
    return code_verifier, code_challenge


def get_oauth_login_url():
    """
    Devuelve la tupla (code_verifier, auth_url) para iniciar el flujo de autenticación oficial
    con PKCE de Movistar Cloud España.
    """
    code_verifier, code_challenge = generate_pkce_pair()
    state = secrets.token_urlsafe(16)
    nonce = secrets.token_urlsafe(16)
    params = {
        "response_type": "code",
        "client_id": OAUTH_CLIENT_ID,
        "redirect_uri": OAUTH_REDIRECT_URI,
        "scope": "openid",
        "state": state,
        "nonce": nonce,
        "code_challenge": code_challenge,
        "code_challenge_method": "S256",
        "acr_values": "2"
    }
    auth_url = f"{OAUTH_AUTHORIZE_URL}?{urllib.parse.urlencode(params)}"
    return code_verifier, auth_url


def exchange_code_for_tokens(auth_code: str, code_verifier: str = ""):
    """Canjea el authorization code por access_token y refresh_token permanente."""
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
    if code_verifier:
        data["code_verifier"] = code_verifier

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
    utilizada por la app de Android y Windows de Movistar Cloud.
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
    """Registra el dispositivo en Movistar Cloud para persistencia a largo plazo."""
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
