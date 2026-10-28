import os
import time
import json
import uuid
import base64
import secrets
import hashlib
import urllib.parse
import requests

# Endpoints y credenciales oficiales de Movistar Cloud España (extraídos de brand/brand.dll v32.0.7.1)
OAUTH_AUTHORIZE_URL = "https://apiseg.telefonica.es/openid/connect/cus/col/auth/oauth/v2/authorize"
OAUTH_TOKEN_URL = "https://apiseg.telefonica.es/openid/connect/cus/col/auth/oauth/v2/token"
OAUTH_CLIENT_ID = "b99e5095-7a36-42c1-83b6-6f05d0f13ea1"
OAUTH_CLIENT_SECRET = "e22fdd6b-636a-4bfc-bb4a-aa8ed6d131d8"
OAUTH_REDIRECT_URI = "https://micloud.movistar.es/ui/html/clientoauth.html"

# CRÍTICO: brand.dll (ClientId b99e5095-...) pertenece a la plataforma "windows" (Movistar Cloud.exe VA 0x004047c0)
CLIENT_PLATFORM = "windows"
ANDROID_USER_AGENT = "omh windows client 32.0.7.1"
SAPI_BASE_URL = "https://micloud.movistar.es/sapi"


def generate_device_id():
    """
    Genera un device ID con el formato exacto de Movistar Cloud Windows (VA 0x00489d52 - 0x00489e38):
    'fol-' + Base64(MD5(username + '+' + MachineGuid)).
    """
    seed = f"PC+{uuid.uuid4()}".encode("utf-8")
    b64_md5 = base64.b64encode(hashlib.md5(seed).digest()).decode("ascii")
    return f"fol-{b64_md5}"


def generate_pkce_pair():
    """Genera el par criptográfico PKCE (code_verifier y code_challenge S256)."""
    code_verifier = secrets.token_urlsafe(64)[:64]
    digest = hashlib.sha256(code_verifier.encode("ascii")).digest()
    code_challenge = base64.urlsafe_b64encode(digest).decode("ascii").rstrip("=")
    return code_verifier, code_challenge


def get_oauth_login_url():
    """
    Devuelve la tupla (code_verifier, auth_url) idéntica a OAuth2WebViewController::getLoginUrl
    (VA 0x00465540) usando los parámetros de brand/brand.dll (scope="openid", access_type="offline").
    """
    code_verifier, code_challenge = generate_pkce_pair()
    state = secrets.token_urlsafe(16)
    nonce = secrets.token_urlsafe(16)
    params = {
        "response_type": "code",
        "client_id": OAUTH_CLIENT_ID,
        "redirect_uri": OAUTH_REDIRECT_URI,
        "scope": "openid",
        "nonce": nonce,
        "state": state,
        "code_challenge_method": "S256",
        "code_challenge": code_challenge,
        "access_type": "offline",
        "lang": "es",
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


def build_oauth_auth_header(
    access_token: str,
    refresh_token: str = "",
    msisdn: str = "",
    last_refresh_date: int = 0,
    expires_in: str = "3600",
    platform: str = CLIENT_PLATFORM
) -> str:
    """
    Construye la cabecera 'Authorization: oauth <base64>' propietaria de Synchronoss/Funambol
    idéntica a Funambol::OAuth2JsonParser::formatOAuth2CredentialData (VA 0x004ba830) con includeMsisdn=false
    y platform="windows" (VA 0x004047c0), que corresponde al ClientId b99e5095-... de brand/brand.dll.
    """
    payload = {
        "data": {
            "accesstoken": access_token,
            "lastrefreshdate": int(last_refresh_date or 0),
            "expiresin": str(expires_in or "3600"),
            "refreshtoken": refresh_token,
            "platform": platform or CLIENT_PLATFORM
        }
    }
    raw_json = json.dumps(payload, separators=(',', ':')).encode("utf-8")
    b64_val = base64.b64encode(raw_json).decode("utf-8")
    return f"oauth {b64_val}"


def parse_oauth_response_header(auth_header: str) -> dict | None:
    """
    Replica Funambol::OAuth2HttpAuthentication::processAuthenticationResponse (VA 0x004e5d80)
    y Funambol::OAuth2JsonParser::parseOAuth2Credentials (VA 0x004bc1b0).
    Inspecciona la cabecera HTTP 'Authorization' devuelta por SAPI; si comienza por 'oauth ',
    decodifica el Base64 y extrae accesstoken, refreshtoken, expiresin y lastrefreshdate.
    """
    if not auth_header or not isinstance(auth_header, str):
        return None
    auth_header = auth_header.strip()
    if len(auth_header) < 6 or auth_header[:5].lower() != "oauth" or auth_header[5] != " ":
        return None

    b64_part = auth_header[6:].strip()
    if not b64_part:
        return None

    try:
        padded = b64_part + "=" * ((4 - len(b64_part) % 4) % 4)
        decoded_bytes = base64.b64decode(padded)
        root = json.loads(decoded_bytes.decode("utf-8"))
        if not isinstance(root, dict) or "error" in root:
            return None
        inner = root.get("data") if isinstance(root.get("data"), dict) else root
        acc = str(inner.get("accesstoken") or inner.get("access_token") or "").strip()
        if not acc:
            return None
        ref = str(inner.get("refreshtoken") or inner.get("refresh_token") or "").strip()
        exp = str(inner.get("expiresin") or inner.get("expires_in") or "3600").strip()
        lrd_raw = inner.get("lastrefreshdate") or root.get("responsetime")
        try:
            lrd = int(lrd_raw) if lrd_raw is not None else int(time.time() * 1000)
            if 0 < lrd < 100000000000:
                lrd = lrd * 1000
        except (ValueError, TypeError):
            lrd = int(time.time() * 1000)
        return {
            "access_token": acc,
            "refresh_token": ref,
            "expires_in": exp,
            "last_refresh_date": lrd
        }
    except Exception:
        return None


def register_device(session: requests.Session, device_id: str, device_name: str, user_id: str = ""):
    """
    En el cliente oficial de Windows (Movistar Cloud.exe / brand.dll), no se invoca /profile/device?action=add
    (que es exclusivo de terminales Android con carrierid=10019/modelid=3482); el dispositivo queda registrado
    mediante las cabeceras X-deviceid y X-devicename en SAPI login.
    """
    return True


def sapi_login_oauth(
    session: requests.Session,
    access_token: str,
    refresh_token: str = "",
    device_id: str = "",
    device_name: str = "",
    msisdn: str = "",
    last_refresh_date: int = 0,
    expires_in: str = "3600"
) -> dict:
    """
    Ejecuta el login nativo en Movistar Cloud SAPI según Funambol::OAuth2LoginHandler::performLogin
    (VA 0x004e3630) para inicializar o renovar la sesión en el backend y obtener
    validationkey, JSESSIONID, responsetime/lastrefreshdate (en ms) y posibles tokens OAuth rotados.
    """
    candidate_requests = [
        ("GET", f"{SAPI_BASE_URL}/login/oauth?action=login&responsetime=true"),
        ("POST", f"{SAPI_BASE_URL}/login/oauth?action=login&responsetime=true"),
        ("POST", f"{SAPI_BASE_URL}/login?action=login&responsetime=true"),
    ]
    auth_header = build_oauth_auth_header(
        access_token,
        refresh_token,
        last_refresh_date=last_refresh_date,
        expires_in=expires_in,
        platform=CLIENT_PLATFORM
    )
    headers = {
        "Authorization": auth_header,
        "User-Agent": ANDROID_USER_AGENT,
        "Accept": "application/json, text/plain, */*"
    }
    if device_id:
        headers["X-deviceid"] = device_id
    if device_name:
        headers["X-devicename"] = device_name

    res = None
    try:
        for method, url in candidate_requests:
            if method == "POST":
                r_try = session.post(url, headers=headers, data=b"", timeout=20)
            else:
                r_try = session.get(url, headers=headers, timeout=20)
            res = r_try
            if r_try.status_code == 200:
                break
    except Exception as e:
        return {"status_code": 0, "validationkey": None, "jsessionid": None, "refreshed_oauth": None, "last_refresh_date": last_refresh_date, "error": str(e)}

    refreshed_oauth = parse_oauth_response_header(res.headers.get("Authorization", ""))
    validationkey = None
    jsessionid = None
    resolved_lrd = int(last_refresh_date or 0)

    for c in session.cookies:
        c_name = c.name.lower()
        if c_name == 'jsessionid':
            jsessionid = c.value
        elif c_name in ('validationkey', 'validation_key', 'vk'):
            validationkey = c.value

    if res.status_code == 200:
        try:
            data = res.json()
            if isinstance(data, dict):
                resp_time = data.get("responsetime")
                inner = data.get("data", {})
                if isinstance(inner, dict):
                    validationkey = inner.get("validationkey") or inner.get("validationKey") or validationkey
                    jsessionid = inner.get("jsessionid") or inner.get("jsessionId") or inner.get("sessionid") or jsessionid
                    body_lrd = inner.get("lastrefreshdate") or resp_time
                    if body_lrd is not None:
                        try:
                            resolved_lrd = int(body_lrd)
                            if 0 < resolved_lrd < 100000000000:
                                resolved_lrd *= 1000
                        except (ValueError, TypeError):
                            pass
                    body_acc = inner.get("access_token") or inner.get("accesstoken")
                    if body_acc and not refreshed_oauth:
                        refreshed_oauth = {
                            "access_token": str(body_acc).strip(),
                            "refresh_token": str(inner.get("refresh_token") or inner.get("refreshtoken") or refresh_token).strip(),
                            "expires_in": str(inner.get("expires_in") or inner.get("expiresin") or expires_in or "3600").strip(),
                            "last_refresh_date": resolved_lrd or int(time.time() * 1000)
                        }
                elif resp_time is not None:
                    try:
                        resolved_lrd = int(resp_time)
                        if 0 < resolved_lrd < 100000000000:
                            resolved_lrd *= 1000
                    except (ValueError, TypeError):
                        pass
        except Exception:
            pass

    if refreshed_oauth and refreshed_oauth.get("last_refresh_date"):
        resolved_lrd = int(refreshed_oauth["last_refresh_date"])
    elif resolved_lrd == 0 and res.status_code == 200:
        resolved_lrd = int(time.time() * 1000)

    return {
        "status_code": res.status_code,
        "validationkey": validationkey,
        "jsessionid": jsessionid,
        "refreshed_oauth": refreshed_oauth,
        "last_refresh_date": resolved_lrd,
        "raw": res.text
    }
