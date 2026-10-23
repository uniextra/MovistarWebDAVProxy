import os
import io
import json
import time
from datetime import datetime, timezone
from pathlib import Path
import requests
from dotenv import load_dotenv
from movistar_device_auth import (
    ANDROID_USER_AGENT,
    build_oauth_auth_header,
    refresh_oauth_token,
    register_device,
    sapi_login_oauth
)

# Base endpoints for Movistar Cloud
BASE_URL = "https://micloud.movistar.es"
UPLOAD_URL = "https://upload.micloud.movistar.es"


def load_account_config(phone: str = None) -> dict:
    """Helper to locate and load full Movistar account configuration including device tokens."""
    candidates = [
        Path("/app/tokens"),
        Path("tokens"),
        Path("../Movistar"),
        Path("../../Movistar"),
        Path("."),
        Path(".."),
        Path("../..")
    ]

    cfg = {
        "phone": os.getenv("MOVISTAR_PHONE", "").strip(),
        "jsessionid": os.getenv("MOVISTAR_JSESSIONID", "").strip(),
        "validationkey": os.getenv("MOVISTAR_VALIDATIONKEY", "").strip(),
        "refresh_token": os.getenv("MOVISTAR_REFRESH_TOKEN", "").strip(),
        "access_token": os.getenv("MOVISTAR_ACCESS_TOKEN", "").strip(),
        "device_id": os.getenv("MOVISTAR_DEVICE_ID", "").strip(),
        "device_name": os.getenv("MOVISTAR_DEVICE_NAME", "").strip(),
        "token_path": None
    }

    if phone:
        clean_phone = "".join(filter(str.isdigit, phone))
        for base in candidates:
            for fname in [f"account_{clean_phone}.env", f"{clean_phone}.env"]:
                p = base / fname
                if p.exists() and p.is_file():
                    load_dotenv(p, override=True)
                    cfg["token_path"] = p
                    cfg["phone"] = clean_phone
                    break
            if cfg["token_path"]:
                break

    if not cfg["token_path"]:
        for base in candidates:
            if not base.exists():
                continue
            env_files = sorted(list(base.glob("account_*.env")) + list(base.glob(".env")))
            for p in env_files:
                if p.is_file():
                    load_dotenv(p, override=True)
                    cfg["token_path"] = p
                    break
            if cfg["token_path"]:
                break

    cfg["phone"] = cfg["phone"] or os.getenv("MOVISTAR_PHONE", "").strip()
    cfg["jsessionid"] = cfg["jsessionid"] or os.getenv("MOVISTAR_JSESSIONID", "").strip()
    cfg["validationkey"] = cfg["validationkey"] or os.getenv("MOVISTAR_VALIDATIONKEY", "").strip()
    cfg["refresh_token"] = cfg["refresh_token"] or os.getenv("MOVISTAR_REFRESH_TOKEN", "").strip()
    cfg["access_token"] = cfg["access_token"] or os.getenv("MOVISTAR_ACCESS_TOKEN", "").strip()
    cfg["device_id"] = cfg["device_id"] or os.getenv("MOVISTAR_DEVICE_ID", "").strip()
    cfg["device_name"] = cfg["device_name"] or os.getenv("MOVISTAR_DEVICE_NAME", "").strip()
    return cfg


def load_credentials(phone: str = None) -> tuple[str, str]:
    """Helper to locate and load Movistar session credentials from various sources."""
    cfg = load_account_config(phone)
    return cfg.get("jsessionid", ""), cfg.get("validationkey", "")


class DownloadStream:
    """
    Wraps requests.Response stream to ensure:
    1. Content-Encoding (gzip/deflate) and Transfer-Encoding (chunked) are properly decoded
       using iter_content() instead of raw socket reads, preventing file corruption.
    2. Connection is properly closed when done.
    3. An internal buffer satisfies arbitrary chunk-sized read() calls seamlessly.
    """
    DEFAULT_CHUNK_SIZE = 64 * 1024  # 64 KB

    def __init__(self, response, chunk_size: int = DEFAULT_CHUNK_SIZE):
        self.response = response
        self.chunk_size = chunk_size
        self._iter = response.iter_content(chunk_size=chunk_size)
        self._buffer = bytearray()
        self._closed = False
        self._bytes_read = 0

    def read(self, size: int = -1) -> bytes:
        if self._closed:
            return b""

        # Read until EOF
        if size is None or size < 0:
            chunks = [bytes(self._buffer)] if self._buffer else []
            self._buffer.clear()
            for chunk in self._iter:
                if chunk:
                    chunks.append(chunk)
            data = b"".join(chunks)
            self._bytes_read += len(data)
            return data

        if size == 0:
            return b""

        # Fill buffer until we have at least `size` bytes or EOF
        while len(self._buffer) < size:
            try:
                chunk = next(self._iter)
                if chunk:
                    self._buffer.extend(chunk)
            except StopIteration:
                break

        if not self._buffer:
            return b""

        chunk_to_return = bytes(self._buffer[:size])
        del self._buffer[:size]
        self._bytes_read += len(chunk_to_return)
        return chunk_to_return

    def tell(self) -> int:
        return self._bytes_read

    def readable(self) -> bool:
        return not self._closed

    def seekable(self) -> bool:
        return False

    def close(self):
        if not self._closed:
            self._closed = True
            self._buffer.clear()
            try:
                self.response.close()
            except Exception:
                pass

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.close()


class MovistarFilesClient:
    def __init__(
        self,
        jsessionid: str = None,
        validationkey: str = None,
        refresh_token: str = None,
        access_token: str = None,
        device_id: str = None,
        device_name: str = None,
        token_path: Path = None,
        phone: str = None
    ):
        self.phone = (phone or "").strip()
        self.jsessionid = (jsessionid or "").strip()
        self.validationkey = (validationkey or "").strip()
        self.refresh_token = (refresh_token or "").strip()
        self.access_token = (access_token or "").strip()
        self.device_id = (device_id or "").strip()
        self.device_name = (device_name or "Samsung SM-S928B (WebDAV Proxy)").strip()
        self.token_path = token_path
        if not self.phone and self.token_path:
            import re
            m = re.search(r"account_(\d+)\.env", str(self.token_path))
            if m:
                self.phone = m.group(1)
        self.session = requests.Session()
        
        # Modo Dispositivo Android Nativo vs Modo Sesión Web
        self.is_native_device = bool(self.refresh_token or self.access_token)
        
        if self.is_native_device:
            headers = {
                "User-Agent": ANDROID_USER_AGENT,
                "Accept": "application/json, text/plain, */*",
            }
            if self.device_id:
                headers["X-deviceid"] = self.device_id
                headers["X-devicename"] = self.device_name
            if self.access_token:
                headers["Authorization"] = build_oauth_auth_header(self.access_token, self.refresh_token)
            if self.jsessionid and self.jsessionid != "None":
                self.session.cookies.set("JSESSIONID", self.jsessionid, domain="micloud.movistar.es", path="/")
            if self.validationkey and self.validationkey != "None":
                self.session.cookies.set("validationkey", self.validationkey, domain="micloud.movistar.es", path="/")
            self.session.headers.update(headers)
            
            # Si no hay access_token pero hay refresh_token, renovar
            if self.refresh_token and not self.access_token:
                self.refresh_device_token()
            elif self.access_token and (not self.validationkey or self.validationkey == "None"):
                # Inicializar sesión SAPI para obtener validationkey
                sapi_res = sapi_login_oauth(self.session, self.access_token, self.refresh_token, self.device_id, self.device_name)
                if sapi_res.get("validationkey"):
                    self.validationkey = sapi_res["validationkey"]
                    self.session.cookies.set("validationkey", self.validationkey, domain="micloud.movistar.es", path="/")
                if sapi_res.get("jsessionid"):
                    self.jsessionid = sapi_res["jsessionid"]
                    self.session.cookies.set("JSESSIONID", self.jsessionid, domain="micloud.movistar.es", path="/")
        else:
            # Modo Sesión Web (fallback)
            if self.jsessionid and self.jsessionid != "None":
                self.session.cookies.set("JSESSIONID", self.jsessionid, domain="micloud.movistar.es", path="/")
            if self.validationkey and self.validationkey != "None":
                self.session.cookies.set("validationkey", self.validationkey, domain="micloud.movistar.es", path="/")
            self.session.headers.update({
                "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
                "Accept": "application/json, text/plain, */*",
            })

        self.upload_url = UPLOAD_URL
        self.max_upload_size_mb = 4096
        self._init_system_info()

    def _init_system_info(self):
        """Fetch system information to resolve dynamic upload endpoint and SAPI limits."""
        try:
            url = f"{BASE_URL}/sapi/system/information?action=get"
            r = self.session.get(url, timeout=5)
            ct = (r.headers.get("content-type") or r.headers.get("Content-Type") or "").lower()
            if r.status_code == 200 and "application/json" in ct:
                data = r.json()
                ep = data.get("sapi.upload.endpoint")
                if ep and str(ep).startswith("http"):
                    self.upload_url = str(ep).rstrip("/")
                max_mb = data.get("sapi.upload.max-size-in-mb")
                if max_mb and str(max_mb).isdigit():
                    self.max_upload_size_mb = int(max_mb)
        except Exception:
            pass

    def get_storage_space(self) -> dict:
        """
        Fetch account storage quota and used space from Movistar Cloud SAPI.
        Endpoint discovered via REA: /sapi/media?action=get-storage-space&softdeleted=true
        Returns dict with keys: used_bytes, total_bytes, free_bytes, raw.
        """
        url = self._url(BASE_URL, "/sapi/media?action=get-storage-space&softdeleted=true")
        try:
            r = self._request("GET", url, timeout=15)
            ct = (r.headers.get("content-type") or r.headers.get("Content-Type") or "").lower()
            if r.status_code == 200 and "application/json" in ct:
                res_data = r.json()
                data = res_data.get("data", {})
                used = data.get("used", 0)
                total = data.get("total", 0)
                try:
                    used_bytes = int(used) if used is not None else 0
                except (ValueError, TypeError):
                    used_bytes = 0
                try:
                    total_bytes = int(total) if total is not None else 0
                except (ValueError, TypeError):
                    total_bytes = 0
                free_bytes = max(0, total_bytes - used_bytes) if total_bytes > 0 else 0
                return {
                    "used_bytes": used_bytes,
                    "total_bytes": total_bytes,
                    "free_bytes": free_bytes,
                    "raw": data
                }
        except Exception as e:
            print(f"[!] get_storage_space error: {e}", flush=True)
        return {"used_bytes": 0, "total_bytes": 0, "free_bytes": 0, "raw": {}}

    def check_session(self) -> bool:
        """
        Ultra-lightweight heartbeat / session validation discovered from official desktop app.
        Calls /sapi/system/information?action=get.
        """
        url = self._url(BASE_URL, "/sapi/system/information?action=get")
        try:
            r = self.session.get(url, timeout=10)
            return r.status_code == 200
        except Exception:
            return False

    def refresh_device_token(self) -> bool:
        """Renueva el access_token usando el refresh_token permanente de Android."""
        if not self.refresh_token:
            return False
        try:
            print("[OAuth] 🔄 Renovando access_token de dispositivo con refresh_token...", flush=True)
            res = refresh_oauth_token(self.refresh_token)
            new_access = res.get("access_token")
            new_refresh = res.get("refresh_token") or self.refresh_token
            if new_access:
                self.access_token = new_access
                self.refresh_token = new_refresh
                self.session.headers["Authorization"] = build_oauth_auth_header(self.access_token, self.refresh_token)

                # Renovar sesión SAPI con los nuevos tokens para sincronizar validationkey y JSESSIONID
                try:
                    sapi_res = sapi_login_oauth(self.session, self.access_token, self.refresh_token, self.device_id, self.device_name)
                    if sapi_res.get("validationkey"):
                        self.validationkey = sapi_res["validationkey"]
                        self.session.cookies.set("validationkey", self.validationkey, domain="micloud.movistar.es", path="/")
                    if sapi_res.get("jsessionid"):
                        self.jsessionid = sapi_res["jsessionid"]
                        self.session.cookies.set("JSESSIONID", self.jsessionid, domain="micloud.movistar.es", path="/")
                except Exception as e:
                    print(f"[SAPI] Aviso renovación sesión SAPI: {e}", flush=True)
                
                # Persistir los tokens renovados en disco en todos los archivos de entorno
                target_paths = set()
                if self.token_path and Path(self.token_path).exists():
                    target_paths.add(Path(self.token_path))
                if Path("/app/tokens/.env").exists():
                    target_paths.add(Path("/app/tokens/.env"))

                for p in target_paths:
                    try:
                        text = p.read_text(encoding="utf-8")
                        lines = []
                        keys_updated = set()
                        for line in text.splitlines():
                            if line.startswith("MOVISTAR_ACCESS_TOKEN="):
                                lines.append(f'MOVISTAR_ACCESS_TOKEN="{self.access_token}"')
                                keys_updated.add("MOVISTAR_ACCESS_TOKEN")
                            elif line.startswith("MOVISTAR_REFRESH_TOKEN="):
                                lines.append(f'MOVISTAR_REFRESH_TOKEN="{self.refresh_token}"')
                                keys_updated.add("MOVISTAR_REFRESH_TOKEN")
                            elif line.startswith("MOVISTAR_VALIDATIONKEY=") and self.validationkey:
                                lines.append(f'MOVISTAR_VALIDATIONKEY="{self.validationkey}"')
                                keys_updated.add("MOVISTAR_VALIDATIONKEY")
                            elif line.startswith("MOVISTAR_JSESSIONID=") and self.jsessionid:
                                lines.append(f'MOVISTAR_JSESSIONID="{self.jsessionid}"')
                                keys_updated.add("MOVISTAR_JSESSIONID")
                            else:
                                lines.append(line)
                        if "MOVISTAR_ACCESS_TOKEN" not in keys_updated:
                            lines.append(f'MOVISTAR_ACCESS_TOKEN="{self.access_token}"')
                        if "MOVISTAR_REFRESH_TOKEN" not in keys_updated:
                            lines.append(f'MOVISTAR_REFRESH_TOKEN="{self.refresh_token}"')
                        if self.validationkey and "MOVISTAR_VALIDATIONKEY" not in keys_updated:
                            lines.append(f'MOVISTAR_VALIDATIONKEY="{self.validationkey}"')
                        if self.jsessionid and "MOVISTAR_JSESSIONID" not in keys_updated:
                            lines.append(f'MOVISTAR_JSESSIONID="{self.jsessionid}"')
                        p.write_text("\n".join(lines) + "\n", encoding="utf-8")
                    except Exception as e:
                        print(f"[OAuth] Error al persistir token renovado en {p}: {e}", flush=True)

                print("[OAuth] ✔ Access token y sesión SAPI renovados con éxito.", flush=True)
                return True
        except Exception as e:
            print(f"[OAuth] ❌ Error al renovar token de dispositivo: {e}", flush=True)
        return False

    def _url(self, base: str, endpoint: str) -> str:
        if self.validationkey:
            sep = "&" if "?" in endpoint else "?"
            return f"{base}{endpoint}{sep}validationkey={self.validationkey}"
        return f"{base}{endpoint}"

    def _request(self, method: str, url: str, **kwargs):
        """Unified request handler with automatic 401 token renewal and retry for native Android devices."""
        req_fn = getattr(self.session, method.lower(), None)
        if callable(req_fn):
            r = req_fn(url, **kwargs)
        else:
            r = self.session.request(method, url, **kwargs)

        if r.status_code == 401:
            if self.refresh_token:
                print(f"[OAuth] 🔄 HTTP 401 recibido en {url.split('?')[0]}. Renovando access_token de dispositivo automáticamente...", flush=True)
                if self.refresh_device_token():
                    if "headers" in kwargs and isinstance(kwargs["headers"], dict) and "Authorization" in kwargs["headers"]:
                        kwargs["headers"]["Authorization"] = self.session.headers.get("Authorization")
                    if "files" in kwargs and isinstance(kwargs["files"], dict):
                        for k, v in kwargs["files"].items():
                            if isinstance(v, tuple) and len(v) >= 2 and hasattr(v[1], "seek"):
                                try:
                                    v[1].seek(0)
                                except Exception:
                                    pass
                    if callable(req_fn):
                        r = req_fn(url, **kwargs)
                    else:
                        r = self.session.request(method, url, **kwargs)
                    print(f"[OAuth] 🔁 Petición reintentada tras renovación de token: HTTP {r.status_code}", flush=True)

            # Si sigue en 401 o no había refresh_token, intentar auto-renovación web persistente sin SMS
            if r.status_code == 401:
                try:
                    from movistar_login import renew_web_session
                    if renew_web_session(phone=self.phone):
                        cfg = load_account_config(phone=self.phone)
                        self.jsessionid = cfg.get("jsessionid", "")
                        self.validationkey = cfg.get("validationkey", "")
                        if self.jsessionid:
                            self.session.cookies.set("JSESSIONID", self.jsessionid, domain="micloud.movistar.es", path="/")
                        if self.validationkey:
                            self.session.cookies.set("validationkey", self.validationkey, domain="micloud.movistar.es", path="/")
                        retry_url = self._url(BASE_URL, url.replace(BASE_URL, "").split("?")[0])
                        if callable(req_fn):
                            r = req_fn(retry_url, **kwargs)
                        else:
                            r = self.session.request(method, retry_url, **kwargs)
                        print(f"[AutoRenew] 🔁 Petición reintentada tras auto-renovación web: HTTP {r.status_code}", flush=True)
                except Exception as e:
                    print(f"[AutoRenew] Intento de auto-renovación web fallido: {e}", flush=True)

            if r.status_code == 401:
                print(f"[!] ⚠️ ERROR 401: La sesión de Movistar Cloud ha caducado. Abre el panel web (puerto 5000) para renovar credenciales.", flush=True)
        return r

    def get_system_info(self):
        """Get system info (public endpoint)."""
        url = self._url(BASE_URL, "/sapi/system/information?action=get")
        r = self.session.get(url, timeout=15)
        print(f"[System Info] Status: {r.status_code}")
        return r.json() if r.status_code == 200 else r.text

    def get_root_folders(self):
        """1. Discover Root Folders (Ficheros root)."""
        url = self._url(BASE_URL, "/sapi/media/folder/root?action=get")
        print(f"[Root Folders] GET {url[:60]}...")
        r = self._request("GET", url, timeout=15)
        print(f"  Response ({r.status_code}): {r.text[:300]}")
        return r

    def list_subfolders(self, parent_id: int, limit: int = 200, offset: int = None):
        """List a single batch of subfolders inside parent folder."""
        url_part = f"/sapi/media/folder?action=list&parentid={parent_id}&limit={limit}"
        if offset is not None and int(offset) > 0:
            url_part += f"&offset={offset}"
        url = self._url(BASE_URL, url_part)
        r = self._request("GET", url, timeout=20)
        return r

    def list_all_subfolders(self, parent_id: int, batch_size: int = 200) -> list:
        """Fetch all subfolders inside parent_id with automatic pagination."""
        all_folders = []
        offset = 0
        while True:
            r = self.list_subfolders(parent_id, limit=batch_size, offset=offset)
            if r.status_code != 200:
                print(f"[!] list_subfolders HTTP {r.status_code}: {r.text[:200]}", flush=True)
                break
            try:
                res_data = r.json()
            except Exception as e:
                print(f"[!] list_subfolders JSON decode error: {e}", flush=True)
                break
            if "error" in res_data:
                print(f"[!] list_subfolders API error: {res_data.get('error')}", flush=True)
                break
            folders = res_data.get("data", {}).get("folders", [])
            if not folders:
                break
            all_folders.extend(folders)
            if len(folders) < batch_size:
                break
            offset += len(folders)
        return all_folders

    def list_media_in_folder(self, folder_id: int, limit: int = 200, offset: int = None):
        """List a single batch of media/files inside a folder."""
        url_part = f"/sapi/media?action=get&folderid={folder_id}&limit={limit}"
        if offset is not None and int(offset) > 0:
            url_part += f"&offset={offset}"
        url = self._url(BASE_URL, url_part)
        payload = {
            "data": {
                "fields": [
                    "name",
                    "modificationdate",
                    "creationdate",
                    "size",
                    "url",
                    "etag",
                    "folderid"
                ]
            }
        }
        r = self._request("POST", url, json=payload, headers={"Content-Type": "application/json;charset=UTF-8"}, timeout=20)
        return r

    def list_all_media_in_folder(self, folder_id: int, batch_size: int = 200) -> list:
        """Fetch all media/files inside folder_id with automatic pagination."""
        all_files = []
        offset = 0
        while True:
            r = self.list_media_in_folder(folder_id, limit=batch_size, offset=offset)
            if r.status_code != 200:
                print(f"[!] list_media_in_folder HTTP {r.status_code}: {r.text[:200]}", flush=True)
                break
            try:
                res_data = r.json()
            except Exception as e:
                print(f"[!] list_media_in_folder JSON decode error: {e}", flush=True)
                break
            if "error" in res_data:
                print(f"[!] list_media_in_folder API error: {res_data.get('error')}", flush=True)
                break
            files = res_data.get("data", {}).get("media", [])
            if not files:
                break
            all_files.extend(files)
            if len(files) < batch_size:
                break
            offset += len(files)
        return all_files

    def create_folder(self, name: str, parent_id: int):
        """2. Create a new folder."""
        url = self._url(BASE_URL, "/sapi/media/folder?action=save")
        payload = {
            "data": {
                "magic": False,
                "offline": False,
                "name": name,
                "parentid": parent_id
            }
        }
        r = self._request("POST", url, json=payload, headers={"Content-Type": "application/json;charset=UTF-8"}, timeout=15)
        return r

    def update_folder(self, folder_id: int, name: str, parent_id: int = None):
        """Update/rename/move an existing folder."""
        url = self._url(BASE_URL, "/sapi/media/folder?action=save")
        data = {
            "id": folder_id,
            "name": name,
        }
        if parent_id is not None:
            data["parentid"] = parent_id
        payload = {"data": data}
        r = self._request("POST", url, json=payload, headers={"Content-Type": "application/json;charset=UTF-8"}, timeout=15)
        return r

    def update_media(self, media_id: str, name: str = None, folder_id: int = None, mediatype: str = "file"):
        """Update/rename an existing media file in Movistar Cloud."""
        url = self._url(BASE_URL, f"/sapi/upload/{mediatype}?action=save-metadata")
        data = {"id": str(media_id)}
        if name is not None:
            data["name"] = name
        if folder_id is not None:
            data["folderid"] = folder_id
        payload = {"data": data}
        r = self._request("POST", url, json=payload, headers={"Content-Type": "application/json;charset=UTF-8"}, timeout=15)
        return r

    def upload_file_to_folder(self, file_name: str, content, folder_id: int, mime: str = "application/octet-stream"):
        """3. Upload a file directly into a folder using dynamic upload endpoint and enforcing max size limit."""
        url = self._url(self.upload_url, "/sapi/upload?action=save&acceptasynchronous=true")
        now_rfc = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        
        if isinstance(content, bytes):
            size = len(content)
            body = content
        else:
            content.seek(0, 2)
            size = content.tell()
            content.seek(0)
            body = content

        max_allowed_bytes = getattr(self, "max_upload_size_mb", 4096) * 1024 * 1024
        if size > max_allowed_bytes:
            raise ValueError(f"File '{file_name}' size ({size / (1024 * 1024):.2f} MB) exceeds maximum allowed size ({self.max_upload_size_mb} MB) for SAPI cluster upload.")
            
        meta = {
            "data": {
                "name": file_name,
                "size": size,
                "modificationdate": now_rfc,
                "creationdate": now_rfc,
                "contenttype": mime,
                "folderid": folder_id
            }
        }
        files = {
            "data": (None, json.dumps(meta), "application/json"),
            "file": (file_name, body, mime)
        }
        r = self._request("POST", url, files=files, timeout=600)
        return r

    def download_file_stream(self, file_info: dict):
        """
        Download a file from Movistar Cloud returning a streaming file-like object.
        Supports file_info with 'url' or 'id'.
        Verifies that the server does not return an error JSON disguised with HTTP 200.
        """
        url = file_info.get("url")
        file_id = file_info.get("id")

        def _is_valid_download(resp):
            if resp.status_code != 200:
                resp.close()
                return False
            ct = (resp.headers.get("Content-Type") or "").lower()
            if "application/json" in ct or "text/javascript" in ct:
                try:
                    err_json = resp.json()
                    err_msg = err_json.get("error", err_json)
                    print(f"[!] download_file_stream: Movistar returned error JSON (Content-Type: {ct}): {err_msg}", flush=True)
                except Exception:
                    print(f"[!] download_file_stream: Movistar returned non-binary response (Content-Type: {ct}): {resp.text[:200]}", flush=True)
                resp.close()
                return False
            return True

        # 1. Try URL if present
        if url:
            if not url.startswith("http"):
                url = f"{BASE_URL}{url}"
            if "validationkey=" not in url:
                sep = "&" if "?" in url else "?"
                url = f"{url}{sep}validationkey={self.validationkey}"
            try:
                r = self._request("GET", url, stream=True, timeout=60)
                if _is_valid_download(r):
                    return DownloadStream(r)
            except Exception as e:
                print(f"[!] download_file_stream URL request failed: {e}", flush=True)

        # 2. Try download endpoints by id
        if file_id:
            # 2a. Request fresh signed URL from media API
            try:
                meta_url = self._url(BASE_URL, f"/sapi/media?action=get&id={file_id}")
                r_meta = self._request(
                    "POST",
                    meta_url,
                    json={"data": {"fields": ["name", "size", "url", "etag"]}},
                    headers={"Content-Type": "application/json;charset=UTF-8"},
                    timeout=15
                )
                if r_meta.status_code == 200:
                    media_items = r_meta.json().get("data", {}).get("media", [])
                    target_media = next((m for m in media_items if str(m.get("id")) == str(file_id)), None)
                    if target_media and target_media.get("url"):
                        fresh_url = target_media["url"]
                        if not fresh_url.startswith("http"):
                            fresh_url = f"{BASE_URL}{fresh_url}"
                        if "validationkey=" not in fresh_url:
                            sep = "&" if "?" in fresh_url else "?"
                            fresh_url = f"{fresh_url}{sep}validationkey={self.validationkey}"
                        r_fresh = self._request("GET", fresh_url, stream=True, timeout=60)
                        if _is_valid_download(r_fresh):
                            return DownloadStream(r_fresh)
            except Exception as e:
                print(f"[!] download_file_stream fresh URL request failed for id {file_id}: {e}", flush=True)

            # 2b. Candidate direct download endpoints
            candidate_endpoints = [
                f"/sapi/media/download?id={file_id}",
                f"/sapi/media?action=download&id={file_id}",
                f"/sapi/media/content?id={file_id}",
            ]
            for ep in candidate_endpoints:
                try:
                    dl_url = self._url(BASE_URL, ep)
                    r = self._request("GET", dl_url, stream=True, timeout=60)
                    if _is_valid_download(r):
                        return DownloadStream(r)
                except Exception as e:
                    print(f"[!] download_file_stream endpoint {ep} request failed: {e}", flush=True)

        return io.BytesIO(b"")

    def delete_folder(self, folder_id: int, permanent: bool = False):
        """Delete a folder (soft delete or permanent)."""
        action = "delete" if permanent else "softdelete"
        url = self._url(BASE_URL, f"/sapi/media/folder?action={action}")
        payload = {
            "data": {
                "folders": [folder_id]
            }
        }
        r = self._request("POST", url, json=payload, headers={"Content-Type": "application/json;charset=UTF-8"}, timeout=15)
        return r

    def delete_file(self, file_id: str):
        """Delete a file (soft delete)."""
        url = self._url(BASE_URL, f"/sapi/media?action=softdelete&id={file_id}")
        r = self._request("POST", url, json={}, timeout=15)
        return r


def main():
    print("Testing Movistar Cloud Files Client...")
    jsid, vk = load_credentials()
    print(f"JSESSIONID: {jsid[:6]}... (len {len(jsid) if jsid else 0})")
    print(f"ValidationKey: {vk[:6]}... (len {len(vk) if vk else 0})")

    if not jsid or not vk:
        print("[!] No credentials found. Please login via the Web UI or set environment variables.")
        return

    client = MovistarFilesClient(jsid, vk)
    
    # 1. System info
    client.get_system_info()
    print("System info received.")

    # 2. Root folder listing
    res_root = client.get_root_folders()

    # 3. If root folder found, test list subfolders and media with pagination
    if res_root.status_code == 200:
        data = res_root.json()
        folders = data.get("data", {}).get("folders", [])
        print(f"Found {len(folders)} root folders.")
        root_folder = next((f for f in folders if f.get("magic")), folders[0] if folders else None)
        if root_folder:
            root_id = root_folder["id"]
            print(f"Active Root Folder: ID={root_id}, Name={root_folder.get('name')}")
            subfolders = client.list_all_subfolders(root_id)
            print(f"Total subfolders (paginated): {len(subfolders)}")
            media_files = client.list_all_media_in_folder(root_id)
            print(f"Total files in root (paginated): {len(media_files)}")
            
            # Test create folder
            test_folder_name = f"Duplicati_Test_{int(time.time())}"
            res_create = client.create_folder(test_folder_name, root_id)
            if res_create.status_code == 200:
                created_folder = res_create.json()
                new_folder_id = created_folder.get("id")
                print(f"Successfully created folder {test_folder_name} with ID: {new_folder_id}")
                
                # Test upload
                test_content = b"Hello from Movistar Duplicati Test!"
                res_upload = client.upload_file_to_folder("test.txt", test_content, new_folder_id)
                print("Upload response:", res_upload.text[:300])


if __name__ == "__main__":
    main()
