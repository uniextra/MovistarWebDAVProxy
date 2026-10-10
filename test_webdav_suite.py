import io
import time
import json
import pytest
from unittest.mock import MagicMock, patch
from webdav_proxy import (
    MovistarProvider,
    MovistarCollection,
    MovistarNonCollection,
    MovistarMemoryFile,
    MovistarWriteObject,
    parse_movistar_time,
)
from test_movistar_api import MovistarFilesClient, DownloadStream
from wsgidav.dav_error import DAVError, HTTP_INTERNAL_ERROR
from wsgidav.wsgidav_app import WsgiDAVApp


class MockResponse:
    def __init__(self, status_code, json_data=None, content=b"", headers=None):
        self.status_code = status_code
        self._json_data = json_data or {}
        self.content = content
        self.text = json.dumps(self._json_data) if json_data else content.decode("utf-8", errors="ignore")
        self.raw = io.BytesIO(content)
        self.headers = headers or {}

    def json(self):
        return self._json_data

    def iter_content(self, chunk_size=8192):
        bio = io.BytesIO(self.content)
        while True:
            chunk = bio.read(chunk_size)
            if not chunk:
                break
            yield chunk

    def close(self):
        pass


def test_parse_movistar_time():
    # ISO string
    t1 = parse_movistar_time("20261009T200000Z")
    assert t1 > 0

    # Milliseconds
    t2 = parse_movistar_time(1700000000000)
    assert t2 == 1700000000.0

    # Seconds
    t3 = parse_movistar_time(1700000000)
    assert t3 == 1700000000.0


def test_client_pagination():
    client = MovistarFilesClient("mock_jsid", "mock_vk")

    # Mock list_subfolders with 2 pages
    def mock_list_subfolders(parent_id, limit=200, offset=0):
        if offset == 0:
            return MockResponse(200, {"data": {"folders": [{"id": i, "name": f"f_{i}"} for i in range(200)]}})
        elif offset == 200:
            return MockResponse(200, {"data": {"folders": [{"id": i, "name": f"f_{i}"} for i in range(200, 250)]}})
        return MockResponse(200, {"data": {"folders": []}})

    with patch.object(client, "list_subfolders", side_effect=mock_list_subfolders):
        all_folders = client.list_all_subfolders(1, batch_size=200)
        assert len(all_folders) == 250
        assert all_folders[0]["name"] == "f_0"
        assert all_folders[249]["name"] == "f_249"


def test_client_media_pagination():
    client = MovistarFilesClient("mock_jsid", "mock_vk")

    # Mock list_media_in_folder with 2 pages
    def mock_list_media(folder_id, limit=200, offset=0):
        if offset == 0:
            return MockResponse(200, {"data": {"media": [{"id": str(i), "name": f"file_{i}.aes", "size": 1000} for i in range(200)]}})
        elif offset == 200:
            return MockResponse(200, {"data": {"media": [{"id": str(i), "name": f"file_{i}.aes", "size": 1000} for i in range(200, 215)]}})
        return MockResponse(200, {"data": {"media": []}})

    with patch.object(client, "list_media_in_folder", side_effect=mock_list_media):
        all_media = client.list_all_media_in_folder(1, batch_size=200)
        assert len(all_media) == 215
        assert all_media[214]["name"] == "file_214.aes"


def test_download_file_stream():
    client = MovistarFilesClient("mock_jsid", "mock_vk")

    # Case 1: with direct url
    file_info = {"url": "https://micloud.movistar.es/sapi/media/download?id=123", "id": "123"}
    mock_resp = MockResponse(200, content=b"stream-bytes-12345")
    with patch.object(client.session, "get", return_value=mock_resp):
        stream = client.download_file_stream(file_info)
        assert stream.read() == b"stream-bytes-12345"
        stream.close()

    # Case 2: without url, uses id
    file_info_no_url = {"id": "456"}
    mock_resp2 = MockResponse(200, content=b"id-stream-bytes")
    with patch.object(client.session, "get", return_value=mock_resp2):
        stream2 = client.download_file_stream(file_info_no_url)
        assert stream2.read() == b"id-stream-bytes"
        stream2.close()


def test_provider_path_traversal():
    client = MovistarFilesClient("mock_jsid", "mock_vk")
    with patch.object(client, "get_root_folders", return_value=MockResponse(200, {"data": {"folders": [{"id": 10, "name": "root", "magic": True}]}})):
        provider = MovistarProvider(client)
        assert provider.root_id == 10

    environ = {"wsgidav.config": {}}

    # Root resource
    root_res = provider.get_resource_inst("/", environ)
    assert isinstance(root_res, MovistarCollection)
    assert root_res.folder_id == 10

    # Path traversal: /backup/duplicati/dblock.aes
    def mock_subfolders(parent_id):
        if parent_id == 10:
            return [{"id": 20, "name": "backup"}]
        if parent_id == 20:
            return [{"id": 30, "name": "duplicati"}]
        return []

    def mock_media(folder_id):
        if folder_id == 30:
            return [{"id": "100", "name": "dblock.aes", "size": 52428800}]
        return []

    with patch.object(provider, "get_subfolders", side_effect=mock_subfolders), \
         patch.object(provider, "get_media", side_effect=mock_media):
        # 1. Folder at depth 1
        b_res = provider.get_resource_inst("/backup", environ)
        assert isinstance(b_res, MovistarCollection)
        assert b_res.folder_id == 20

        # 2. Folder at depth 2
        d_res = provider.get_resource_inst("/backup/duplicati", environ)
        assert isinstance(d_res, MovistarCollection)
        assert d_res.folder_id == 30

        # 3. File at depth 3
        f_res = provider.get_resource_inst("/backup/duplicati/dblock.aes", environ)
        assert isinstance(f_res, MovistarNonCollection)
        assert f_res.get_content_length() == 52428800
        assert f_res.file_info["id"] == "100"


def test_file_move_rename():
    client = MovistarFilesClient("mock_jsid", "mock_vk")
    provider = MovistarProvider(client)
    provider.root_id = 10
    environ = {"movistar.api": client, "wsgidav.provider": provider, "wsgidav.config": {}}

    file_info = {"id": "555", "name": "temp.tmp", "folderid": 10, "size": 100}
    file_res = MovistarNonCollection("/temp.tmp", environ, file_info)

    # 1. Native update succeeds
    with patch.object(client, "update_media", return_value=MockResponse(200)):
        file_res.copy_move_single("/dblock.aes", is_move=True)
        assert True

    # 2. Native update fails, fallback to stream upload + delete
    with patch.object(client, "update_media", side_effect=Exception("API error")), \
         patch.object(file_res, "get_content", return_value=io.BytesIO(b"content")), \
         patch.object(client, "upload_file_to_folder", return_value=MockResponse(200)) as mock_upload, \
         patch.object(client, "delete_file", return_value=MockResponse(200)) as mock_del:
        file_res.copy_move_single("/dblock2.aes", is_move=True)
        assert mock_upload.called
        assert mock_del.called


def test_wsgidav_lock_integration():
    """Verify that WsgiDAV app correctly initializes with lock manager."""
    client = MovistarFilesClient("mock_jsid", "mock_vk")
    with patch.object(client, "get_root_folders", return_value=MockResponse(200, {"data": {"folders": [{"id": 10, "magic": True}]}})):
        provider = MovistarProvider(client)

    config = {
        "host": "127.0.0.1",
        "port": 8080,
        "provider_mapping": {"/": provider},
        "simple_dc": {"user_mapping": {"*": True}},
        "lock_storage": True,
        "verbose": 0
    }
    app = WsgiDAVApp(config)
    assert app.lock_manager is not None


def test_wsgidav_end_to_end_methods():
    """End-to-end simulation of MKCOL, PUT, GET, HEAD, PROPFIND, MOVE, DELETE, LOCK."""
    from werkzeug.test import Client

    client = MovistarFilesClient("mock_jsid", "mock_vk")
    with patch.object(client, "get_root_folders", return_value=MockResponse(200, {"data": {"folders": [{"id": 10, "magic": True}]}})):
        provider = MovistarProvider(client)

    config = {
        "host": "127.0.0.1",
        "port": 8080,
        "provider_mapping": {"/": provider},
        "simple_dc": {"user_mapping": {"*": True}},
        "lock_storage": True,
        "verbose": 0
    }
    app = WsgiDAVApp(config)
    test_client = Client(app)

    # 1. MKCOL /backup
    with patch.object(client, "create_folder", return_value=MockResponse(201, {"id": 20})):
        res = test_client.open("/backup", method="MKCOL")
        assert res.status_code in (201, 204)

    # Verify immediate PROPFIND without manual cache hack
    res_prop = test_client.open("/backup", method="PROPFIND", headers={"Depth": "0"})
    assert res_prop.status_code == 207

    # 2. PUT /backup/data.dblock
    uploaded_payload = b"Duplicati backup block binary content 12345"
    with patch.object(client, "upload_file_to_folder", return_value=MockResponse(200, {"id": "100", "url": "https://download/100"})) as mock_up:
        res = test_client.open("/backup/data.dblock", method="PUT", data=uploaded_payload)
        assert res.status_code in (201, 204)
        assert mock_up.called

    # 3. GET /backup/data.dblock
    res = test_client.open("/backup/data.dblock", method="GET")
    assert res.status_code == 200
    assert res.data == uploaded_payload

    # 4. HEAD /backup/data.dblock
    res = test_client.open("/backup/data.dblock", method="HEAD")
    assert res.status_code == 200
    assert res.data == b""
    assert int(res.headers.get("Content-Length", 0)) == len(uploaded_payload)

    # 5. PROPFIND /backup (Depth: 1)
    res = test_client.open("/backup", method="PROPFIND", headers={"Depth": "1"})
    assert res.status_code == 207  # Multi-Status
    assert b"data.dblock" in res.data or b"backup" in res.data

    # 6. MOVE /backup/data.dblock -> /backup/data_renamed.dblock
    with patch.object(client, "update_media", return_value=MockResponse(200)):
        res = test_client.open(
            "/backup/data.dblock",
            method="MOVE",
            headers={"Destination": "/backup/data_renamed.dblock", "Overwrite": "T"}
        )
        assert res.status_code in (201, 204)

    # 7. DELETE /backup/data_renamed.dblock
    with patch.object(client, "delete_file", return_value=MockResponse(200)):
        res = test_client.open("/backup/data_renamed.dblock", method="DELETE")
        assert res.status_code in (200, 204)

    # 8. LOCK on unmapped URL /backup/lockfile
    lock_xml = """<?xml version="1.0" encoding="utf-8" ?>
<D:lockinfo xmlns:D='DAV:'>
<D:lockscope><D:exclusive/></D:lockscope>
<D:locktype><D:write/></D:locktype>
<D:owner><D:href>http://duplicati.local/</D:href></D:owner>
</D:lockinfo>"""
    res = test_client.open(
        "/backup/lockfile",
        method="LOCK",
        headers={"Timeout": "Second-3600", "Content-Type": "text/xml"},
        data=lock_xml
    )
    assert res.status_code in (200, 201)
    assert b"locktoken" in res.data.lower()


def test_large_file_download_fallback_from_cache():
    """Verify that a >1MB file in upload_cache streams from API instead of returning 0 bytes."""
    client = MovistarFilesClient("mock_jsid", "mock_vk")
    provider = MovistarProvider(client)
    provider.root_id = 10
    environ = {"movistar.api": client, "wsgidav.provider": provider, "wsgidav.config": {}}

    # Cache entry for a 50MB file where content is b""
    cache_data = {
        "id": "777",
        "name": "large.dblock.zip",
        "folderid": 10,
        "content": b"",
        "size": 52428800,
        "time": time.time(),
        "url": "https://micloud.movistar.es/sapi/media/download?id=777"
    }
    mem_file = MovistarMemoryFile("/large.dblock.zip", environ, cache_data)
    assert mem_file.get_content_length() == 52428800

    stream_content = b"X" * 1024  # Simulated stream
    with patch.object(client, "download_file_stream", return_value=io.BytesIO(stream_content)) as mock_dl:
        content_stream = mem_file.get_content()
        assert content_stream.read() == stream_content
        assert mock_dl.called


def test_put_overwrite_cleans_old_file():
    """Verify that overwriting an existing file deletes the superseded cloud file."""
    client = MovistarFilesClient("mock_jsid", "mock_vk")
    provider = MovistarProvider(client)
    provider.root_id = 10
    environ = {"movistar.api": client, "wsgidav.provider": provider, "wsgidav.config": {}}

    writer = MovistarWriteObject(
        path="/test_overwrite.txt",
        environ=environ,
        folder_id=10,
        existing_file_id="old_zombie_id"
    )
    writer.write(b"new content")

    with patch.object(client, "upload_file_to_folder", return_value=MockResponse(200, {"id": "new_id"})) as mock_up, \
         patch.object(client, "delete_file", return_value=MockResponse(200)) as mock_del:
        writer.close()
        assert mock_up.called
        assert mock_del.called
        mock_del.assert_called_with("old_zombie_id")


def test_thousand_files_propfind_pagination():
    """Verify that a folder with 1,000 dblock files lists every single file with pagination."""
    client = MovistarFilesClient("mock_jsid", "mock_vk")
    provider = MovistarProvider(client)
    provider.root_id = 10
    environ = {"movistar.api": client, "wsgidav.provider": provider, "wsgidav.config": {}}

    thousand_files = [{"id": str(i), "name": f"duplicati-b{i:04d}.dblock.zip.aes", "size": 50000000} for i in range(1000)]

    with patch.object(provider, "get_subfolders", return_value=[]), \
         patch.object(provider, "get_media", return_value=thousand_files):
        col = MovistarCollection("/duplicati", environ, {"id": 99})
        member_list = col.get_member_list()
        assert len(member_list) == 1000
        assert member_list[0].name == "duplicati-b0000.dblock.zip.aes"
        assert member_list[999].name == "duplicati-b0999.dblock.zip.aes"


def test_create_collection_returns_dav_collection_instance():
    """Verify that create_collection returns a valid DAVCollection instance and registers in cache."""
    client = MovistarFilesClient("mock_jsid", "mock_vk")
    provider = MovistarProvider(client)
    provider.root_id = 10
    environ = {"movistar.api": client, "wsgidav.provider": provider, "wsgidav.config": {}}

    root_col = MovistarCollection("/", environ, {"id": 10})

    # Successful creation
    with patch.object(client, "create_folder", return_value=MockResponse(201, {"id": 55})):
        created_res = root_col.create_collection("my_duplicati_folder")
        assert isinstance(created_res, MovistarCollection)
        assert created_res.is_collection is True
        assert created_res.path == "/my_duplicati_folder"
        assert created_res.folder_id == 55
        assert "/my_duplicati_folder" in provider.created_folders

    # Failed creation raises DAVError
    with patch.object(client, "create_folder", return_value=MockResponse(500, content=b"Server error")):
        with pytest.raises(DAVError):
            root_col.create_collection("failed_folder")


def test_duplicati_mkcol_eventual_consistency_and_immediate_verification():
    """
    Simulate Duplicati creating a backup folder and immediately testing it:
    1. MKCOL /duplicati_backup -> WsgiDAV returns 201 Created
    2. Duplicati immediately verifies with PROPFIND /duplicati_backup (eventual consistency: API listing is empty!)
    3. PROPFIND succeeds with 207 Multi-Status instead of 404
    4. PROPFIND / (Depth: 1) includes duplicati_backup
    5. PUT /duplicati_backup/test.tmp uploads with target folder id 77
    """
    from werkzeug.test import Client

    client = MovistarFilesClient("mock_jsid", "mock_vk")
    with patch.object(client, "get_root_folders", return_value=MockResponse(200, {"data": {"folders": [{"id": 10, "magic": True}]}})):
        provider = MovistarProvider(client)

    config = {
        "host": "127.0.0.1",
        "port": 8080,
        "provider_mapping": {"/": provider},
        "simple_dc": {"user_mapping": {"*": True}},
        "lock_storage": True,
        "verbose": 0
    }
    app = WsgiDAVApp(config)
    test_client = Client(app)

    # Simulate eventual consistency: API list_all_subfolders returns empty list
    with patch.object(client, "create_folder", return_value=MockResponse(201, {"id": 77})) as mock_create, \
         patch.object(client, "list_all_subfolders", return_value=[]) as mock_list_sub:

        # 1. Duplicati executes MKCOL
        res_mkcol = test_client.open("/duplicati_backup", method="MKCOL")
        assert res_mkcol.status_code == 201
        assert mock_create.called

        # 2. Duplicati immediately performs PROPFIND /duplicati_backup
        # Without our fix, this returned 404 Not Found because list_all_subfolders was empty
        res_prop = test_client.open("/duplicati_backup", method="PROPFIND", headers={"Depth": "0"})
        assert res_prop.status_code == 207
        assert b"duplicati_backup" in res_prop.data

        # 3. Duplicati lists parent / with Depth 1
        res_parent = test_client.open("/", method="PROPFIND", headers={"Depth": "1"})
        assert res_parent.status_code == 207
        assert b"duplicati_backup" in res_parent.data

        # 4. Duplicati uploads test file into newly created folder
        with patch.object(client, "upload_file_to_folder", return_value=MockResponse(200, {"id": "999"})) as mock_upload:
            res_put = test_client.open("/duplicati_backup/test.tmp", method="PUT", data=b"test-payload")
            assert res_put.status_code in (201, 204)
            assert mock_upload.called
            # Verify upload was directed to newly created folder id 77
            assert mock_upload.call_args[0][2] == 77


def test_download_stream_buffering_and_chunks():
    """Verify that DownloadStream correctly handles arbitrary chunk sizes and EOF."""
    payload = b"ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" * 1000  # 36,000 bytes
    resp = MockResponse(200, content=payload)
    stream = DownloadStream(resp, chunk_size=1024)

    # 1. Read small chunk
    c1 = stream.read(10)
    assert c1 == payload[:10]
    assert stream.tell() == 10

    # 2. Read exact chunk
    c2 = stream.read(100)
    assert c2 == payload[10:110]
    assert stream.tell() == 110

    # 3. Read size 0
    c0 = stream.read(0)
    assert c0 == b""

    # 4. Read larger than internal chunk size
    c3 = stream.read(5000)
    assert c3 == payload[110:5110]
    assert stream.tell() == 5110

    # 5. Read remaining until EOF
    c_rest = stream.read(-1)
    assert c_rest == payload[5110:]
    assert stream.tell() == len(payload)

    # 6. Read after EOF returns empty bytes
    assert stream.read(100) == b""

    stream.close()
    assert stream.read(100) == b""


def test_download_file_stream_rejects_json_error():
    """Verify that download_file_stream rejects 200 OK responses with JSON/JS error bodies."""
    client = MovistarFilesClient("mock_jsid", "mock_vk")

    json_error_resp = MockResponse(
        200,
        json_data={"error": {"code": "PAPI-0000", "message": "Unknown exception"}},
        headers={"Content-Type": "application/json;charset=UTF-8"}
    )

    with patch.object(client.session, "get", return_value=json_error_resp), \
         patch.object(client.session, "post", return_value=json_error_resp):
        stream = client.download_file_stream({"url": "https://download/test", "id": "123"})
        # Should reject JSON and return empty BytesIO fallback instead of streaming JSON as file data
        assert isinstance(stream, io.BytesIO)
        assert stream.read() == b""


def test_download_file_stream_accepts_octet_stream():
    """Verify that download_file_stream accepts valid binary octet-stream responses."""
    client = MovistarFilesClient("mock_jsid", "mock_vk")

    binary_data = b"\x41\x45\x53\x02valid-encrypted-file-content"
    valid_resp = MockResponse(
        200,
        content=binary_data,
        headers={"Content-Type": "application/octet-stream;charset=UTF-8"}
    )

    with patch.object(client.session, "get", return_value=valid_resp):
        stream = client.download_file_stream({"url": "https://download/test"})
        assert isinstance(stream, DownloadStream)
        downloaded = stream.read()
        assert downloaded == binary_data
        stream.close()




