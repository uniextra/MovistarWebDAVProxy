import os
import sys
import io

if hasattr(sys.stdout, "reconfigure"):
    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
        sys.stderr.reconfigure(encoding="utf-8", errors="replace")
    except Exception:
        pass
import re
import time
import threading
from datetime import datetime, timezone
import tempfile
import hashlib
from wsgidav.dav_provider import DAVProvider, DAVCollection, DAVNonCollection
from wsgidav.dav_error import DAVError, HTTP_FORBIDDEN, HTTP_NOT_FOUND, HTTP_CONFLICT, HTTP_INTERNAL_ERROR
from wsgidav import util
from wsgidav.wsgidav_app import WsgiDAVApp
from test_movistar_api import MovistarFilesClient, load_credentials, load_account_config, DownloadStream

# Límite estricto de concurrencia: máximo 3 transferencias simultáneas (subidas o descargas) a la API de Movistar
TRANSFER_SEMAPHORE = threading.BoundedSemaphore(3)


class SemaphoreStream:
    """Envuelve un stream de descarga para liberar el semáforo de concurrencia al cerrarse."""
    def __init__(self, stream, sem):
        self.stream = stream
        self.sem = sem
        self._released = False

    def read(self, size=-1):
        return self.stream.read(size)

    def tell(self):
        return getattr(self.stream, "tell", lambda: 0)()

    def close(self):
        try:
            if hasattr(self.stream, "close"):
                self.stream.close()
        finally:
            if not self._released:
                self._released = True
                self.sem.release()


def parse_movistar_time(val) -> float:
    """Parse Movistar ISO/RFC date strings or unix millisecond timestamps into seconds since epoch."""
    if not val:
        return time.time()
    if isinstance(val, (int, float)):
        return val / 1000.0 if val > 1e11 else float(val)
    if isinstance(val, str):
        val = val.strip()
        if val.isdigit():
            num = int(val)
            return num / 1000.0 if num > 1e11 else float(num)
        for fmt in (
            "%Y%m%dT%H%M%SZ",
            "%Y-%m-%dT%H:%M:%SZ",
            "%Y-%m-%d %H:%M:%S",
            "%Y-%m-%dT%H:%M:%S.%fZ",
            "%Y-%m-%dT%H:%M:%S%z",
        ):
            try:
                dt = datetime.strptime(val, fmt)
                if dt.tzinfo is None:
                    dt = dt.replace(tzinfo=timezone.utc)
                return dt.timestamp()
            except ValueError:
                continue
    return time.time()


class MovistarNonCollection(DAVNonCollection):
    """Represents a file stored on Movistar Cloud."""
    def __init__(self, path, environ, file_info):
        super().__init__(path, environ)
        self.file_info = file_info or {}

    def get_content_length(self):
        try:
            return int(self.file_info.get("size", 0))
        except (ValueError, TypeError):
            return 0

    def get_content_type(self):
        config = self.environ.get("wsgidav.config", {})
        guessed = util.guess_mime_type(self.path, config)
        return guessed or self.file_info.get("contenttype") or "application/octet-stream"

    def get_creation_date(self):
        return parse_movistar_time(
            self.file_info.get("creationdate") or self.file_info.get("modificationdate")
        )

    def get_last_modified(self):
        return parse_movistar_time(
            self.file_info.get("modificationdate") or self.file_info.get("creationdate")
        )

    def support_etag(self):
        return True

    def get_etag(self):
        etag = self.file_info.get("etag")
        if etag:
            return str(etag).strip('"')
        fid = self.file_info.get("id", "0")
        mtime = int(self.get_last_modified())
        size = self.get_content_length()
        return f"{fid}-{mtime}-{size}"

    def support_ranges(self):
        return False

    def get_content(self):
        api = self.environ["movistar.api"]
        now_str = datetime.now().strftime("%H:%M:%S")
        file_name = os.path.basename(self.path)
        size_mb = self.get_content_length() / (1024 * 1024)
        print(f"[{now_str}] [WebDAV] ⬇ Descargando: '{file_name}' ({size_mb:.2f} MB) desde Movistar Cloud...", flush=True)
        TRANSFER_SEMAPHORE.acquire()
        try:
            stream = api.download_file_stream(self.file_info)
            if isinstance(stream, io.BytesIO) and len(stream.getvalue()) == 0 and self.get_content_length() > 0:
                TRANSFER_SEMAPHORE.release()
                raise DAVError(HTTP_INTERNAL_ERROR, f"No se pudo descargar '{file_name}' desde Movistar Cloud")
            return SemaphoreStream(stream, TRANSFER_SEMAPHORE)
        except Exception:
            TRANSFER_SEMAPHORE.release()
            raise

    def begin_write(self, content_type=None):
        return MovistarWriteObject(
            self.path,
            self.environ,
            self.file_info.get("folderid"),
            existing_file_id=self.file_info.get("id")
        )

    def delete(self):
        api = self.environ["movistar.api"]
        file_id = self.file_info.get("id")
        file_name = os.path.basename(self.path)
        now_str = datetime.now().strftime("%H:%M:%S")
        if file_id:
            print(f"[{now_str}] [WebDAV] 🗑 Eliminando archivo: '{file_name}' (ID: {file_id})...", flush=True)
            api.delete_file(file_id)
            print(f"[{now_str}] [WebDAV] ✔ Archivo eliminado: '{file_name}'", flush=True)
        provider = self.environ.get("wsgidav.provider")
        if provider:
            provider.invalidate_cache(self.file_info.get("folderid"))
            if hasattr(provider, "upload_cache") and self.path in provider.upload_cache:
                del provider.upload_cache[self.path]

    def support_recursive_move(self, dest_path):
        return False

    def handle_move(self, dest_path):
        """WebDAV MOVE handled natively to prevent WsgiDAV copy+delete fallback."""
        dest_parent_path = os.path.dirname(dest_path).replace("\\", "/")
        if not dest_parent_path.startswith("/"):
            dest_parent_path = "/" + dest_parent_path
        dest_name = os.path.basename(dest_path)
        now_str = datetime.now().strftime("%H:%M:%S")
        print(f"[{now_str}] [WebDAV] 🔄 Renombrando: '{self.path}' -> '{dest_path}'...", flush=True)

        provider = self.environ["wsgidav.provider"]
        dest_parent_res = provider.get_resource_inst(dest_parent_path, self.environ)
        if not dest_parent_res or not dest_parent_res.is_collection:
            raise DAVError(HTTP_CONFLICT, f"Destination directory {dest_parent_path} does not exist")

        api = self.environ["movistar.api"]
        file_id = self.file_info.get("id")
        src_folder_id = self.file_info.get("folderid")
        dest_folder_id = dest_parent_res.folder_id

        # 1. Attempt native rename/move if cloud id exists
        moved = False
        if file_id:
            try:
                res = api.update_media(file_id, name=dest_name, folder_id=dest_folder_id)
                if res.status_code in (200, 201, 204):
                    moved = True
            except Exception:
                pass

        # 2. Fallback: stream-copy to destination and delete source
        if not moved:
            stream = self.get_content()
            try:
                mime = self.get_content_type()
                res = api.upload_file_to_folder(dest_name, stream, dest_folder_id, mime=mime)
                if res.status_code not in (200, 201, 204):
                    raise DAVError(HTTP_INTERNAL_ERROR, f"Failed to upload to destination during move: {res.text}")
                if file_id:
                    api.delete_file(file_id)
            finally:
                if hasattr(stream, "close"):
                    try:
                        stream.close()
                    except Exception:
                        pass

        # Update cache entries
        if hasattr(provider, "upload_cache"):
            if self.path in provider.upload_cache:
                c = provider.upload_cache.pop(self.path)
                c["name"] = dest_name
                c["folderid"] = dest_folder_id
                provider.upload_cache[dest_path] = c

        provider.invalidate_cache(dest_folder_id)
        if src_folder_id:
            provider.invalidate_cache(src_folder_id)
        return True

    def copy_move_single(self, dest_path, *, is_move):
        """WebDAV MOVE / COPY handler for a single file."""
        if is_move:
            self.handle_move(dest_path)
            return

        dest_parent_path = os.path.dirname(dest_path).replace("\\", "/")
        if not dest_parent_path.startswith("/"):
            dest_parent_path = "/" + dest_parent_path
        dest_name = os.path.basename(dest_path)
        
        provider = self.environ["wsgidav.provider"]
        dest_parent_res = provider.get_resource_inst(dest_parent_path, self.environ)
        if not dest_parent_res or not dest_parent_res.is_collection:
            raise DAVError(HTTP_CONFLICT, f"Destination directory {dest_parent_path} does not exist")
            
        api = self.environ["movistar.api"]
        dest_folder_id = dest_parent_res.folder_id

        stream = self.get_content()
        try:
            mime = self.get_content_type()
            res = api.upload_file_to_folder(dest_name, stream, dest_folder_id, mime=mime)
            if res.status_code not in (200, 201, 204):
                raise DAVError(HTTP_INTERNAL_ERROR, f"Failed to upload to destination during copy: {res.text}")
        finally:
            if hasattr(stream, "close"):
                try:
                    stream.close()
                except Exception:
                    pass

        provider.invalidate_cache(dest_folder_id)


class MovistarMemoryFile(DAVNonCollection):
    """Represents a freshly uploaded or pending file kept in memory cache."""
    def __init__(self, path, environ, cache_data):
        super().__init__(path, environ)
        self.cache_data = cache_data

    def get_content_length(self):
        return int(self.cache_data.get("size", 0))

    def get_content_type(self):
        config = self.environ.get("wsgidav.config", {})
        guessed = util.guess_mime_type(self.path, config)
        return guessed or "application/octet-stream"

    def get_creation_date(self):
        return self.cache_data.get("time", time.time())

    def get_last_modified(self):
        return self.cache_data.get("time", time.time())

    def support_etag(self):
        return True

    def get_etag(self):
        if self.cache_data.get("md5"):
            return self.cache_data["md5"]
        fid = self.cache_data.get("id", "mem")
        mtime = int(self.cache_data.get("time", time.time()))
        size = self.get_content_length()
        return f"{fid}-{mtime}-{size}"

    def support_ranges(self):
        return False

    def get_content(self):
        content = self.cache_data.get("content")
        # If content bytes are present in memory, serve directly
        if content:
            return io.BytesIO(content)
            
        api = self.environ["movistar.api"]
        
        # We don't have the file in RAM. The upload response might not have provided a proper 'url' or 'id'.
        # Let's fetch the REAL metadata from the API using get_media.
        provider = self.environ.get("wsgidav.provider")
        if provider:
            for attempt in range(10):
                # Force cache invalidation for this folder just in case, to get the freshest list
                provider.invalidate_cache(self.cache_data.get("folderid"))
                media_files = provider.get_media(self.cache_data.get("folderid"))
                real_file = next((f for f in media_files if f.get("name") == self.cache_data.get("name")), None)
                if real_file and real_file.get("url"):
                    # Update our cache with the official URL and ID
                    self.cache_data.update(real_file)
                    break
                print(f"[WebDAV] ⏳ Esperando indexación en la nube para '{self.cache_data.get('name')}' (intento {attempt+1}/10)...", flush=True)
                time.sleep(3)
                
        now_str = datetime.now().strftime("%H:%M:%S")
        file_name = self.cache_data.get("name", "unknown")
        size_mb = self.get_content_length() / (1024 * 1024)
        print(f"[{now_str}] [WebDAV] ⬇ Descargando (Caché): '{file_name}' ({size_mb:.2f} MB)...", flush=True)

        TRANSFER_SEMAPHORE.acquire()
        try:
            stream = api.download_file_stream(self.cache_data)
            if isinstance(stream, io.BytesIO) and len(stream.getvalue()) == 0 and self.get_content_length() > 0:
                TRANSFER_SEMAPHORE.release()
                raise DAVError(HTTP_INTERNAL_ERROR, f"No se pudo descargar '{file_name}' desde Movistar Cloud")
            return SemaphoreStream(stream, TRANSFER_SEMAPHORE)
        except Exception:
            TRANSFER_SEMAPHORE.release()
            raise

    def begin_write(self, content_type=None):
        return MovistarWriteObject(
            self.path,
            self.environ,
            self.cache_data.get("folderid"),
            existing_file_id=self.cache_data.get("id")
        )

    def delete(self):
        api = self.environ["movistar.api"]
        file_id = self.cache_data.get("id")
        if file_id:
            api.delete_file(file_id)
        provider = self.environ.get("wsgidav.provider")
        if provider:
            provider.invalidate_cache(self.cache_data.get("folderid"))
            if hasattr(provider, "upload_cache") and self.path in provider.upload_cache:
                del provider.upload_cache[self.path]

    def support_recursive_move(self, dest_path):
        return False

    def handle_move(self, dest_path):
        """WebDAV MOVE handled natively for cached memory files."""
        dest_parent_path = os.path.dirname(dest_path).replace("\\", "/")
        if not dest_parent_path.startswith("/"):
            dest_parent_path = "/" + dest_parent_path
        dest_name = os.path.basename(dest_path)

        provider = self.environ["wsgidav.provider"]
        dest_parent_res = provider.get_resource_inst(dest_parent_path, self.environ)
        if not dest_parent_res or not dest_parent_res.is_collection:
            raise DAVError(HTTP_CONFLICT, f"Destination directory {dest_parent_path} does not exist")

        api = self.environ["movistar.api"]
        file_id = self.cache_data.get("id")
        src_folder_id = self.cache_data.get("folderid")
        dest_folder_id = dest_parent_res.folder_id

        # 1. Attempt native rename/move if cloud id exists
        moved = False
        if file_id:
            try:
                res = api.update_media(file_id, name=dest_name, folder_id=dest_folder_id)
                if res.status_code in (200, 201, 204):
                    moved = True
            except Exception:
                pass

        # 2. Fallback: upload to destination and remove source
        if not moved:
            content = self.cache_data.get("content")
            if content:
                res = api.upload_file_to_folder(dest_name, io.BytesIO(content), dest_folder_id)
            else:
                stream = self.get_content()
                try:
                    res = api.upload_file_to_folder(dest_name, stream, dest_folder_id)
                finally:
                    if hasattr(stream, "close"):
                        try:
                            stream.close()
                        except Exception:
                            pass
            if res.status_code not in (200, 201, 204):
                raise DAVError(HTTP_INTERNAL_ERROR, f"Failed to upload to destination during move: {res.text}")
            if file_id:
                try:
                    api.delete_file(file_id)
                except Exception:
                    pass

        # Update cache entries
        if hasattr(provider, "upload_cache"):
            if self.path in provider.upload_cache:
                c = provider.upload_cache.pop(self.path)
                c["name"] = dest_name
                c["folderid"] = dest_folder_id
                provider.upload_cache[dest_path] = c

        provider.invalidate_cache(dest_folder_id)
        if src_folder_id:
            provider.invalidate_cache(src_folder_id)
        return True

    def copy_move_single(self, dest_path, *, is_move):
        if is_move:
            self.handle_move(dest_path)
            return

        dest_parent_path = os.path.dirname(dest_path).replace("\\", "/")
        if not dest_parent_path.startswith("/"):
            dest_parent_path = "/" + dest_parent_path
        dest_name = os.path.basename(dest_path)

        provider = self.environ["wsgidav.provider"]
        dest_parent_res = provider.get_resource_inst(dest_parent_path, self.environ)
        if not dest_parent_res or not dest_parent_res.is_collection:
            raise DAVError(HTTP_CONFLICT, f"Destination directory {dest_parent_path} does not exist")

        api = self.environ["movistar.api"]
        dest_folder_id = dest_parent_res.folder_id
        content = self.cache_data.get("content")

        if content:
            res = api.upload_file_to_folder(dest_name, io.BytesIO(content), dest_folder_id)
        else:
            stream = self.get_content()
            try:
                res = api.upload_file_to_folder(dest_name, stream, dest_folder_id)
            finally:
                if hasattr(stream, "close"):
                    try:
                        stream.close()
                    except Exception:
                        pass

        if res.status_code not in (200, 201, 204):
            raise DAVError(HTTP_INTERNAL_ERROR, f"Failed to upload to destination during copy: {res.text}")

        provider.invalidate_cache(dest_folder_id)


class MovistarWriteObject:
    """Buffers incoming WebDAV PUT stream and commits upload to Movistar Cloud on close()."""
    def __init__(self, path, environ, folder_id, existing_file_id=None):
        self.path = path
        self.environ = environ
        self.folder_id = folder_id
        self.existing_file_id = existing_file_id
        # Buffer up to 10MB in memory, then spill to temporary disk file
        self.buffer = tempfile.SpooledTemporaryFile(max_size=10 * 1024 * 1024, mode="w+b")

    def write(self, data):
        if self.buffer is not None:
            self.buffer.write(data)

    def close(self):
        if self.buffer is None:
            return

        self.buffer.seek(0, 2)
        size = self.buffer.tell()
        self.buffer.seek(0)

        api = self.environ["movistar.api"]
        file_name = os.path.basename(self.path)
        config = self.environ.get("wsgidav.config", {})
        mime = util.guess_mime_type(self.path, config) or "application/octet-stream"

        if not self.folder_id:
            self.buffer.close()
            self.buffer = None
            raise DAVError(HTTP_INTERNAL_ERROR, "Cannot upload file: target folder ID is missing")

        # Clean up superseded file BEFORE uploading to prevent Movistar Cloud from adding ' (1)' suffix
        if self.existing_file_id:
            try:
                api.delete_file(self.existing_file_id)
            except Exception:
                pass

        size_mb = size / (1024 * 1024)
        max_mb = getattr(api, "max_upload_size_mb", 4096)
        if size > max_mb * 1024 * 1024:
            self.buffer.close()
            self.buffer = None
            raise DAVError(413, f"File '{file_name}' size ({size_mb:.2f} MB) exceeds maximum allowed size ({max_mb} MB) for Movistar Cloud")

        # Calculate fast MD5 block checksum (identical to official Movistar desktop sync engine)
        hasher = hashlib.md5()
        self.buffer.seek(0)
        while True:
            chunk = self.buffer.read(1024 * 1024)
            if not chunk:
                break
            hasher.update(chunk)
        file_md5 = hasher.hexdigest()
        self.buffer.seek(0)

        now_str = datetime.now().strftime("%H:%M:%S")
        print(f"[{now_str}] [WebDAV] ⬆ Subiendo: '{file_name}' ({size_mb:.2f} MB, MD5: {file_md5[:8]}...) a Movistar Cloud...", flush=True)

        with TRANSFER_SEMAPHORE:
            res = api.upload_file_to_folder(file_name, self.buffer, self.folder_id, mime=mime)
        
        # Read content for fast memory cache only for small files (< 1MB)
        content_to_cache = b""
        if size < 1024 * 1024:
            self.buffer.seek(0)
            content_to_cache = self.buffer.read()

        self.buffer.close()
        self.buffer = None

        if res.status_code not in (200, 201, 204):
            print(f"[{now_str}] [WebDAV] ❌ Error al subir '{file_name}': {res.status_code} {res.text}", flush=True)
            raise DAVError(HTTP_INTERNAL_ERROR, f"Upload to Movistar Cloud failed ({res.status_code}): {res.text}")

        data_json = {}
        try:
            data_json = res.json()
        except Exception:
            pass
            
        # Extract metadata from {"data": {"id": ...}} structure
        real_id = data_json.get("id")
        real_url = data_json.get("url")
        cloud_name = data_json.get("name")
        if "data" in data_json and isinstance(data_json["data"], dict):
            d = data_json["data"]
            if not real_id:
                real_id = d.get("id")
            if not real_url:
                real_url = d.get("url")
            if not cloud_name:
                cloud_name = d.get("name")

        # If Movistar still appended a ' (1)' suffix despite cleanup, rename it back immediately
        if real_id and cloud_name and cloud_name != file_name and " (" in cloud_name:
            try:
                api.update_media(real_id, name=file_name)
            except Exception:
                pass

        fid = real_id or "OK"
        print(f"[{now_str}] [WebDAV] ✔ Subida completada: '{file_name}' ({size_mb:.2f} MB, ID: {fid})", flush=True)

        now = time.time()
        provider = self.environ.get("wsgidav.provider")
        if provider:
            if not hasattr(provider, "upload_cache"):
                provider.upload_cache = {}
            provider.upload_cache[self.path] = {
                "id": real_id,
                "name": file_name,
                "folderid": self.folder_id,
                "content": content_to_cache,
                "size": size,
                "time": now,
                "url": real_url,
                "md5": file_md5
            }
            provider.invalidate_cache(self.folder_id)


class MovistarCollection(DAVCollection):
    """Represents a folder/directory on Movistar Cloud."""
    def __init__(self, path, environ, folder_info=None):
        super().__init__(path, environ)
        self.folder_info = folder_info or {}
        self.folder_id = self.folder_info.get("id")

    def get_available_bytes(self):
        """RFC 4331 quota-available-bytes for WebDAV clients (e.g. Synology HyperBackup)."""
        provider = self.environ.get("wsgidav.provider")
        if provider and hasattr(provider, "get_storage_space"):
            space = provider.get_storage_space()
            free_bytes = space.get("free_bytes", 0)
            if free_bytes > 0:
                return free_bytes
            # For unlimited Movistar accounts (total_bytes == 0), report 10 TB virtual capacity minus used
            used = space.get("used_bytes", 0)
            return max(0, 10 * 1024 * 1024 * 1024 * 1024 - used)
        return 10 * 1024 * 1024 * 1024 * 1024  # 10 TB virtual fallback

    def get_used_bytes(self):
        """RFC 4331 quota-used-bytes querying real storage from Movistar Cloud."""
        provider = self.environ.get("wsgidav.provider")
        if provider and hasattr(provider, "get_storage_space"):
            space = provider.get_storage_space()
            used = space.get("used_bytes")
            if used is not None and used > 0:
                return used
        return 1024 * 1024 * 1024  # 1 GB fallback

    def get_creation_date(self):
        return parse_movistar_time(
            self.folder_info.get("creationdate") or self.folder_info.get("modificationdate")
        )

    def get_last_modified(self):
        return parse_movistar_time(
            self.folder_info.get("modificationdate") or self.folder_info.get("creationdate")
        )

    def get_member_names(self):
        return [res.name for res in self.get_member_list()]

    def create_empty_resource(self, name):
        """
        Invoked by WsgiDAV before PUT or during LOCK on an unmapped URL.
        Registers a placeholder in upload_cache without wasting an empty 0-byte upload to the cloud.
        """
        path = os.path.join(self.path, name).replace("\\", "/")
        file_info = {
            "name": name,
            "id": None,
            "size": 0,
            "url": None,
            "folderid": self.folder_id,
            "creationdate": datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ"),
            "modificationdate": datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ"),
        }
        provider = self.environ.get("wsgidav.provider")
        if provider:
            if not hasattr(provider, "upload_cache"):
                provider.upload_cache = {}
            provider.upload_cache[path] = {
                "id": None,
                "name": name,
                "folderid": self.folder_id,
                "content": b"",
                "size": 0,
                "time": time.time(),
                "url": None
            }
        return MovistarNonCollection(path, self.environ, file_info)

    def create_collection(self, name):
        """WebDAV MKCOL: Create subfolder."""
        name = name.strip("/")
        api = self.environ["movistar.api"]
        provider = self.environ.get("wsgidav.provider")

        if not self.folder_id and provider and self.path == "/":
            self.folder_id = provider._ensure_root_id()

        if not self.folder_id:
            raise DAVError(HTTP_INTERNAL_ERROR, "Cannot create folder: parent folder ID is missing")

        now_str = datetime.now().strftime("%H:%M:%S")

        # 1. Comprobar si la carpeta ya existe en Movistar Cloud para no duplicar con '(1)'
        if provider:
            existing = [f for f in provider.get_subfolders(self.folder_id) if f.get("name", "").lower() == name.lower()]
            if existing:
                folder_info = existing[0]
                created_id = folder_info.get("id")
                child_path = os.path.join(self.path, name).replace("\\", "/")
                if not child_path.startswith("/"):
                    child_path = "/" + child_path
                print(f"[{now_str}] [WebDAV] ℹ Carpeta '{name}' ya existía en Movistar Cloud (ID: {created_id}). Reutilizando sin duplicar.", flush=True)
                provider.add_created_folder(child_path, folder_info)
                return MovistarCollection(child_path, self.environ, folder_info)

        print(f"[{now_str}] [WebDAV] 📁 Creando carpeta: '{name}' en carpeta ID {self.folder_id}...", flush=True)

        res = api.create_folder(name, self.folder_id)
        if res.status_code not in (200, 201, 204):
            print(f"[{now_str}] [WebDAV] ❌ Error al crear carpeta '{name}': {res.status_code} {res.text}", flush=True)
            raise DAVError(HTTP_INTERNAL_ERROR, f"Failed to create folder in Movistar Cloud ({res.status_code}): {res.text}")

        # Extract created folder ID from API response
        created_id = None
        try:
            c_json = res.json()
            if isinstance(c_json, dict):
                if "id" in c_json:
                    created_id = c_json["id"]
                elif "data" in c_json:
                    d = c_json["data"]
                    if isinstance(d, dict):
                        created_id = d.get("id")
                        if not created_id and "folders" in d and isinstance(d["folders"], list) and d["folders"]:
                            created_id = d["folders"][0].get("id")
                    elif isinstance(d, list) and d:
                        created_id = d[0].get("id")
        except Exception:
            pass

        # If ID wasn't in the response body, actively resolve it from parent folder listing
        if not created_id and provider:
            for attempt in range(5):
                provider.invalidate_cache(self.folder_id)
                subs = provider.get_subfolders(self.folder_id)
                found = next((f for f in subs if f.get("name", "").lower() == name.lower()), None)
                if found and found.get("id"):
                    created_id = found["id"]
                    break
                time.sleep(1)

        child_path = os.path.join(self.path, name).replace("\\", "/")
        if not child_path.startswith("/"):
            child_path = "/" + child_path

        now_rfc = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        folder_info = {
            "id": created_id,
            "name": name,
            "parentid": self.folder_id,
            "creationdate": now_rfc,
            "modificationdate": now_rfc,
        }

        print(f"[{now_str}] [WebDAV] ✔ Carpeta creada: '{name}' (ID: {created_id}) en '{child_path}'", flush=True)

        if provider:
            provider.add_created_folder(child_path, folder_info)

        return MovistarCollection(child_path, self.environ, folder_info)

    def delete(self):
        """WebDAV DELETE on collection."""
        api = self.environ["movistar.api"]
        folder_name = os.path.basename(self.path)
        now_str = datetime.now().strftime("%H:%M:%S")
        if self.folder_id:
            print(f"[{now_str}] [WebDAV] 🗑 Eliminando carpeta: '{folder_name}' (ID: {self.folder_id})...", flush=True)
            api.delete_folder(self.folder_id, permanent=False)
            print(f"[{now_str}] [WebDAV] ✔ Carpeta eliminada: '{folder_name}'", flush=True)
        provider = self.environ.get("wsgidav.provider")
        if provider:
            provider.invalidate_cache()
            if hasattr(provider, "created_folders"):
                provider.created_folders.pop(self.path.rstrip("/"), None)

    def support_recursive_delete(self):
        """Allow WsgiDAV to delete collection in a single call."""
        return True

    def support_recursive_move(self, dest_path):
        """Allow moving entire folder hierarchy."""
        return True

    def handle_move(self, dest_path):
        """Native collection move handler."""
        self.copy_move_single(dest_path, is_move=True)
        return True

    def move_recursive(self, dest_path):
        """Native recursive move of collection."""
        self.copy_move_single(dest_path, is_move=True)
        return []

    def copy_move_single(self, dest_path, *, is_move):
        """WebDAV MOVE / COPY for collection."""
        dest_parent_path = os.path.dirname(dest_path.rstrip("/")).replace("\\", "/")
        if not dest_parent_path.startswith("/"):
            dest_parent_path = "/" + dest_parent_path
        dest_name = os.path.basename(dest_path.rstrip("/"))

        provider = self.environ["wsgidav.provider"]
        dest_parent_res = provider.get_resource_inst(dest_parent_path, self.environ)
        if not dest_parent_res or not dest_parent_res.is_collection:
            raise DAVError(HTTP_CONFLICT, f"Destination directory {dest_parent_path} does not exist")

        api = self.environ["movistar.api"]
        dest_folder_id = dest_parent_res.folder_id

        if is_move:
            res = api.update_folder(self.folder_id, dest_name, dest_folder_id)
            if res.status_code not in (200, 201, 204):
                raise DAVError(HTTP_INTERNAL_ERROR, f"Failed to move collection: {res.text}")
            if hasattr(provider, "created_folders"):
                src_key = self.path.rstrip("/")
                dest_key = dest_path.rstrip("/")
                if src_key in provider.created_folders:
                    entry = provider.created_folders.pop(src_key)
                    entry["info"]["name"] = dest_name
                    entry["info"]["parentid"] = dest_folder_id
                    provider.created_folders[dest_key] = entry
        else:
            res = api.create_folder(dest_name, dest_folder_id)
            if res.status_code not in (200, 201, 204):
                raise DAVError(HTTP_INTERNAL_ERROR, f"Failed to copy collection: {res.text}")

        provider.invalidate_cache()

    def get_member_list(self):
        """Returns list of children (subfolders and files) with pagination."""
        member_list = []
        if not self.folder_id:
            return member_list

        provider = self.environ.get("wsgidav.provider")
        if not provider:
            return member_list

        # 1. Subfolders (paginated)
        subfolders = provider.get_subfolders(self.folder_id)
        seen_folder_names = set()
        for f in subfolders:
            f_name = f.get("name")
            if f_name and f_name not in seen_folder_names:
                seen_folder_names.add(f_name)
                child_path = os.path.join(self.path, f_name).replace("\\", "/")
                member_list.append(MovistarCollection(child_path, self.environ, f))

        # 2. Files (paginated)
        media_files = provider.get_media(self.folder_id)
        seen_names = set()
        for f in media_files:
            f["folderid"] = self.folder_id
            raw_name = f.get("name", "")
            # Normalize collision suffixes like ' (1)' so WebDAV clients (HyperBackup) always see canonical names
            clean_name = re.sub(r' \(\d+\)(\.[^.]+)?$', lambda m: m.group(1) or '', raw_name)
            name_to_use = clean_name if (clean_name and clean_name not in seen_names) else raw_name
            seen_names.add(name_to_use)
            child_path = os.path.join(self.path, name_to_use).replace("\\", "/")
            f_res = dict(f)
            f_res["name"] = name_to_use
            member_list.append(MovistarNonCollection(child_path, self.environ, f_res))

        # 3. Add fresh items from upload_cache for this folder that might not yet appear in API
        if hasattr(provider, "upload_cache"):
            for c_path, c_data in list(provider.upload_cache.items()):
                if c_data.get("folderid") == self.folder_id:
                    c_name = c_data.get("name")
                    if c_name and c_name not in seen_names:
                        member_list.append(MovistarMemoryFile(c_path, self.environ, c_data))

        return member_list


class MovistarProvider(DAVProvider):
    """WsgiDAV provider translating WebDAV operations to Movistar Cloud SAPI."""
    def __init__(self, api):
        super().__init__()
        self.api = api
        self.upload_cache = {}
        self.created_folders = {} # {norm_path: {"info": folder_info, "time": timestamp}}
        self.folder_cache = {}    # {parent_id: (timestamp, [subfolders])}
        self.media_cache = {}     # {folder_id: (timestamp, [media_files])}
        self.path_cache = {}      # {path: (timestamp, folder_id)}
        self.CACHE_TTL = 15.0     # seconds

        # Discover root folder ID once
        self.root_id = None
        self._ensure_root_id()

    def _ensure_root_id(self):
        """Discover or refresh root folder ID lazily if not set."""
        if not self.root_id:
            try:
                res = self.api.get_root_folders()
                if res.status_code == 200:
                    folders = res.json().get("data", {}).get("folders", [])
                    root_folder = next((f for f in folders if f.get("magic")), folders[0] if folders else None)
                    if root_folder:
                        self.root_id = root_folder["id"]
            except Exception:
                pass
        return self.root_id

    def add_created_folder(self, path, folder_info):
        """Register a freshly created folder to guarantee immediate WebDAV consistency."""
        norm_path = path.rstrip("/")
        if not norm_path:
            norm_path = "/"
        if not hasattr(self, "created_folders"):
            self.created_folders = {}
        self.created_folders[norm_path] = {
            "info": folder_info,
            "time": time.time()
        }
        parent_id = folder_info.get("parentid")
        if parent_id is not None and parent_id in self.folder_cache:
            ts, folders = self.folder_cache[parent_id]
            if not any(f.get("name") == folder_info.get("name") for f in folders):
                folders = list(folders)
                folders.append(folder_info)
                self.folder_cache[parent_id] = (ts, folders)

    def invalidate_cache(self, folder_id=None):
        """Invalidate in-memory listings when changes occur."""
        self._storage_space_cache = None
        if folder_id:
            self.folder_cache.pop(folder_id, None)
            self.media_cache.pop(folder_id, None)
        else:
            self.folder_cache.clear()
            self.media_cache.clear()
            self.path_cache.clear()

    def get_storage_space(self):
        """Cached account storage quota and used bytes (Discovery #1)."""
        now = time.time()
        if hasattr(self, "_storage_space_cache") and self._storage_space_cache:
            cached_time, cached_val = self._storage_space_cache
            if now - cached_time < 60.0:
                return cached_val
        res = self.api.get_storage_space()
        self._storage_space_cache = (now, res)
        return res

    def get_subfolders(self, parent_id):
        """Cached paginated listing of subfolders with immediate consistency for newly created folders."""
        now = time.time()
        cached = self.folder_cache.get(parent_id)
        if cached and (now - cached[0] < self.CACHE_TTL):
            folders = list(cached[1])
        else:
            folders = self.api.list_all_subfolders(parent_id)
            self.folder_cache[parent_id] = (now, folders)
            folders = list(folders)

        # Merge freshly created folders that might not yet be returned due to eventual consistency
        if hasattr(self, "created_folders"):
            seen_names = {f.get("name") for f in folders if f.get("name")}
            for p, entry in list(self.created_folders.items()):
                if now - entry.get("time", 0) > 60:
                    self.created_folders.pop(p, None)
                    continue
                f_info = entry.get("info", {})
                if f_info.get("parentid") == parent_id:
                    f_name = f_info.get("name")
                    if f_name and f_name not in seen_names:
                        folders.append(f_info)
                        seen_names.add(f_name)

        return folders

    def get_media(self, folder_id):
        """Cached paginated listing of media files."""
        now = time.time()
        cached = self.media_cache.get(folder_id)
        if cached and (now - cached[0] < self.CACHE_TTL):
            return cached[1]
        media = self.api.list_all_media_in_folder(folder_id)
        self.media_cache[folder_id] = (now, media)
        return media

    def get_resource_inst(self, path, environ):
        """Resolve any WebDAV path to its corresponding DAVResource instance."""
        environ["movistar.api"] = self.api
        environ["wsgidav.provider"] = self

        # Normalize path
        norm_path = path.rstrip("/")
        if not norm_path:
            norm_path = "/"

        # Root Collection
        if norm_path == "/":
            self._ensure_root_id()
            return MovistarCollection("/", environ, {"id": self.root_id})

        # Garbage collect upload cache entries older than 60 seconds
        now = time.time()
        expired = [p for p, c in self.upload_cache.items() if now - c.get("time", 0) > 900]
        for p in expired:
            del self.upload_cache[p]

        # Check upload_cache first for immediate consistency
        if norm_path in self.upload_cache:
            return MovistarMemoryFile(norm_path, environ, self.upload_cache[norm_path])

        # Check created_folders for immediate collection consistency
        if hasattr(self, "created_folders") and norm_path in self.created_folders:
            c_entry = self.created_folders[norm_path]
            if now - c_entry.get("time", 0) <= 60:
                c_info = c_entry.get("info", {})
                if not c_info.get("id") and c_info.get("parentid"):
                    try:
                        for f in self.get_subfolders(c_info["parentid"]):
                            if f.get("name") == c_info.get("name") and f.get("id"):
                                c_info["id"] = f["id"]
                                break
                    except Exception:
                        pass
                return MovistarCollection(norm_path, environ, c_info)
            else:
                self.created_folders.pop(norm_path, None)

        # Walk down folder tree
        parts = [p for p in norm_path.split("/") if p]
        current_id = self.root_id
        target_info = None
        is_folder = True

        for idx, part in enumerate(parts):
            if not current_id:
                return None

            is_last = (idx == len(parts) - 1)

            # Check subfolders
            subfolders = self.get_subfolders(current_id)
            found_folder = next((f for f in subfolders if f.get("name") == part), None)
            if not found_folder:
                found_folder = next((f for f in subfolders if f.get("name", "").lower() == part.lower()), None)
            # Synology HyperBackup .hbk extension tolerance
            if not found_folder:
                found_folder = next((f for f in subfolders if f.get("name", "").lower() == f"{part}.hbk".lower()), None)
            if not found_folder and part.lower().endswith(".hbk"):
                base_part = part[:-4]
                found_folder = next((f for f in subfolders if f.get("name", "").lower() == base_part.lower()), None)
            
            if found_folder:
                current_id = found_folder["id"]
                target_info = found_folder
                is_folder = True
                continue

            # If not found in subfolders and this is the final path component, check files
            if is_last:
                media_files = self.get_media(current_id)
                found_file = next((f for f in media_files if f.get("name") == part), None)
                if not found_file:
                    found_file = next((f for f in media_files if f.get("name", "").lower() == part.lower()), None)
                # Tolerance for Movistar cloud duplicate suffix ' (1)', ' (2)', etc.
                if not found_file:
                    stem, ext = os.path.splitext(part)
                    for mf in media_files:
                        m_name = mf.get("name", "")
                        if m_name.startswith(f"{stem} (") or m_name.startswith(f"{part} ("):
                            found_file = mf
                            break
                if found_file:
                    found_file["folderid"] = current_id
                    target_info = found_file
                    is_folder = False
                    break

            # Path not found
            return None

        if target_info:
            if is_folder:
                return MovistarCollection(norm_path, environ, target_info)
            else:
                return MovistarNonCollection(norm_path, environ, target_info)

        return None


class WebDAVTrafficLogger:
    """Middleware WSGI para registrar conexiones y métodos WebDAV en tiempo real."""
    def __init__(self, wsgi_app):
        self.wsgi_app = wsgi_app

    def __call__(self, environ, start_response):
        method = environ.get("REQUEST_METHOD", "")
        path = environ.get("PATH_INFO", "")
        client_ip = environ.get("REMOTE_ADDR", "-")
        now_str = datetime.now().strftime("%H:%M:%S")

        # Eliminar el header de autorización enviado por el cliente.
        # Esto engaña a WsgiDAV para que procese la petición como "anónima"
        # (permitida vía simple_dc {"*": True}), aceptando así cualquier usuario/password.
        environ.pop("HTTP_AUTHORIZATION", None)

        # Registrar peticiones activas de clientes como Duplicati
        if method in ("PUT", "GET", "DELETE", "MOVE", "MKCOL", "PROPFIND", "HEAD", "LOCK", "UNLOCK"):
            print(f"[{now_str}] [WebDAV] ⚡ {client_ip} -> {method} {path}", flush=True)

        return self.wsgi_app(environ, start_response)


def main():
    cfg = load_account_config()
    api = MovistarFilesClient(
        jsessionid=cfg.get("jsessionid"),
        validationkey=cfg.get("validationkey"),
        refresh_token=cfg.get("refresh_token"),
        access_token=cfg.get("access_token"),
        device_id=cfg.get("device_id"),
        device_name=cfg.get("device_name"),
        token_path=cfg.get("token_path"),
        phone=cfg.get("phone")
    )
    PROXY_VERSION = "v1.0.5"
    if api.is_native_device:
        print(f"[WebDAV] 📱 Modo Autenticación: DISPOSITIVO ANDROID NATIVO ({cfg.get('device_id')})", flush=True)
        print("[WebDAV] 🔒 Inmune a desconexiones web. Refresh Token permanente activo.", flush=True)
    elif cfg.get("jsessionid"):
        print("[WebDAV] 🌐 Modo Autenticación: Sesión Web (Fallback)", flush=True)
        print("[WebDAV] ℹ️ Recomendado: Para sesiones 24/7 sin caducidad, pulsa '🔑 Vincular Dispositivo (Login SMS)' en el panel web (puerto 5000).", flush=True)
    else:
        print("[!] Warning: Movistar credentials not found. Inicia sesión en el panel Web (puerto 5000).", flush=True)

    provider = MovistarProvider(api)

    config = {
        "host": "0.0.0.0",
        "port": 8080,
        "provider_mapping": {"/": provider},
        "verbose": 1,
        "simple_dc": {"user_mapping": {"*": True}},
        "dir_browser": {"enabled": True},
        "hotfixes": {"win_accept_unquoted_boundary": True},
        "lock_storage": True
    }

    raw_app = WsgiDAVApp(config)
    app = WebDAVTrafficLogger(raw_app)
    
    now_str = datetime.now().strftime("%H:%M:%S")
    print(f"[{now_str}] [WebDAV] ✔ Proxy WebDAV {PROXY_VERSION} iniciado y escuchando en http://{config['host']}:{config['port']}", flush=True)
    try:
        from cheroot import wsgi
        server = wsgi.Server(bind_addr=(config["host"], config["port"]), wsgi_app=app, numthreads=8)
        server.start()
    except ImportError:
        from wsgiref.simple_server import make_server
        print(f"[{now_str}] [WebDAV] Cheroot no disponible; sirviendo con wsgiref en {config['host']}:{config['port']}", flush=True)
        httpd = make_server(config["host"], config["port"], app)
        httpd.serve_forever()


if __name__ == "__main__":
    main()
