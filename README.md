# Movistar Cloud WebDAV Proxy ☁️🔐 (Go Edition v2.0.1)

[![Docker](https://img.shields.io/badge/Docker-Ready-blue?logo=docker)](https://hub.docker.com/r/uniextra/movistar-webdav-proxy)
[![Go Report Card](https://goreportcard.com/badge/github.com/uniextra/MovistarWebDAVProxy)](https://goreportcard.com/report/github.com/uniextra/MovistarWebDAVProxy)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![WebDAV](https://img.shields.io/badge/WebDAV-RFC%204918%20%7C%20RFC%204331-green)](https://tools.ietf.org/html/rfc4918)
[![Image Size](https://img.shields.io/badge/Image%20Size-~20%20MB-success)](https://hub.docker.com/r/uniextra/movistar-webdav-proxy)
[![RAM Usage](https://img.shields.io/badge/RAM%20Usage-~6.5%20MB-success)](https://hub.docker.com/r/uniextra/movistar-webdav-proxy)

A high-performance, ultra-lightweight standalone WebDAV proxy written entirely in **Go** that seamlessly translates standard WebDAV backup requests (**Synology HyperBackup**, **Duplicati**, **rclone**, etc.) into the proprietary **Movistar Cloud Spain (Files)** API in real-time.

Turn your unlimited Movistar Cloud storage into a transparent, secure, and remote destination for your server, NAS, and personal backups.

> [!NOTE]
> **Migrating from Python (v1.x)?**  
> The original Python + Chromium/noVNC implementation is preserved in the [`legacy-python`](https://github.com/uniextra/MovistarWebDAVProxy/tree/legacy-python) branch.  
> Version 2.0.0 is a complete ground-up rewrite in native Go, reducing container image size from **~1.34 GB down to ~20 MB** and RAM usage from **~150 MB down to ~6.5 MB**.

---

## 🌟 Key Features

* 🚀 **Ultra-Lightweight & Efficient (Native Go v2.0.0):**
  * Standalone compiled binary running inside an Alpine scratch container (`~20 MB` image size).
  * Consumes only **~6.5 MB of RAM** under normal operation (down from 150 MB in Python).
  * Completely removes Chromium, Xvfb, Fluxbox, and noVNC dependencies.
* 🔄 **Full WebDAV Method Translation (RFC 4918):**
  * `PROPFIND` (automatic server-side pagination overcoming Movistar's default 200-item limit).
  * `PUT` (disk-buffered streaming upload supporting files of any size without RAM exhaustion).
  * `GET` & `HEAD` (streaming downloads with chunk decoding, HTTP `Range` request support, and partial content `206 Partial Content`).
  * `MKCOL` (folder creation with instant ID resolution and nested directory support for `.hbk` archives).
  * `DELETE` (clean pruning of files and collections).
  * `MOVE` & `COPY` (atomic server-side renaming/moving with pre-delete overwrite protection to avoid ` (1)` duplicates).
  * `LOCK` & `UNLOCK` (in-memory lock manager).
* 📊 **RFC 4331 Storage Quota Support:**
  * Reports virtual quota (`quota-available-bytes`) and usage so that Synology DSM and backup wizards pass pre-flight checks without *"Insufficient storage"* errors.
* 🛡️ **Eventual Consistency & Collision Resilience:**
  * Hides Movistar CDN indexing propagation delays via an intelligent transient metadata cache.
  * Deletes superseded cloud files prior to overwriting to prevent Movistar from generating ` (1)` duplicate suffix collisions.
  * On-the-fly filename normalization ensuring backup software always receives canonical file paths.
* 🚦 **Concurrency Semaphore (Max 3 Transfers):**
  * Strictly bounds heavy concurrent uploads and downloads to 3 parallel streams to operate in Movistar Cloud's optimal throughput band without API rate-limiting or socket resets.
* 🔑 **Permissive Client Authentication:**
  * Accepts any username and password in your WebDAV client (Synology DSM and Duplicati require these fields); the proxy authorizes requests internally using your active account credentials.
* 💻 **Official Windows Desktop Client OAuth 2.0 Protocol:**
  * Emulates the official Movistar Cloud Windows desktop client (`omh windows client 32.0.7.1`, `platform="windows"`, `scope="openid"`, and `fol-<base64_md5>` `X-deviceid` matching the `brand.dll` OAuth2 client `b99e5095-7a36-42c1-83b6-6f05d0f13ea1`).
  * **64-bit Millisecond `lastrefreshdate` Synchronization:** Preserves the exact 13-digit server timestamp (`responsetime` / `lastrefreshdate`) returned by `GET /sapi/login/oauth?action=login&responsetime=true`.
  * **In-Band Token Rotation (`processAuthenticationResponse`):** Inspects SAPI HTTP response headers for updated `Authorization: oauth <base64>` tokens and atomically persists rotated credentials.
  * **Zero session conflicts:** You can log into Movistar Cloud via web browsers or mobile apps without kicking the WebDAV proxy offline.
* 📱 **Native Browser-Based OAuth2 PKCE Linking:**
  * When linking a new device, the web panel generates an official Telefónica authorization URL with PKCE security. Open it directly in your own browser, complete the SMS verification, and paste the resulting redirect URL into the modal. The proxy exchanges tokens and initializes SAPI without requiring a browser inside the container!
* 💓 **Automatic Keep-Alive Worker:**
  * Periodically validates session health via `/sapi/system/information?action=get` and automatically refreshes tokens and Tomcat sessions (`JSESSIONID` + `validationkey`) upon `HTTP 401`.

---

## 🐳 Docker Deployment

### Option 1: Docker Compose (Recommended)

Create a `docker-compose.yml` file:

```yaml
version: '3.8'

services:
  movistar-webdav-proxy:
    image: uniextra/movistar-webdav-proxy:latest
    build: .
    container_name: movistar-webdav-proxy
    restart: unless-stopped
    ports:
      - "5000:5000"      # Web Management UI & logs
      - "5050:8080"      # WebDAV Endpoint (map to 5050 on host)
    volumes:
      - movistar_tokens:/app/tokens
      - movistar_data:/data

volumes:
  movistar_tokens:
    name: movistar_webdav_tokens
  movistar_data:
    name: movistar_webdav_data
```

Start the service:
```bash
docker compose up -d
```

### Option 2: Docker CLI

```bash
docker run -d \
  --name movistar-webdav-proxy \
  --restart unless-stopped \
  -p 5000:5000 \
  -p 5050:8080 \
  -v movistar_webdav_tokens:/app/tokens \
  -v movistar_webdav_data:/data \
  uniextra/movistar-webdav-proxy:latest
```

---

## 🔑 Initial Setup & Authentication (Device Pairing)

1. Open your web browser and navigate to the management dashboard: `http://YOUR_SERVER_IP:5000`.
2. Click on **"Vincular Dispositivo (Login SMS Oficial)"**.
3. A modal will display the official Telefónica login URL. Click **"Abrir Página de Login en Nueva Pestaña"** to open it in your browser.
4. Enter your Movistar mobile number and the SMS code you receive.
5. After login, your browser will redirect to a Movistar URL (e.g. `https://micloud.movistar.es/ui/html/clientoauth.html?code=...`).
6. Copy that URL and paste it into the field in the proxy modal, then click **"Completar Vinculación"**.
7. The proxy will exchange the OAuth2 authorization code with PKCE, register your device, obtain initial session tokens, and start the WebDAV server on port `8080` (or `5050` based on your host port mapping).

---

## ⚙️ Backup Client Configuration

### 1. Synology HyperBackup Setup

In DSM HyperBackup wizard:

1. Select **WebDAV** as the backup destination.
2. Fill in the connection settings:
   * **Server address:** `http://YOUR_DOCKER_HOST_IP:5050`
   * **Username:** `admin` (or any string)
   * **Password:** `admin` (or any string)
   * **Folder:** Select or enter a dedicated folder for HyperBackup (e.g. `HyperBackup_NAS`).
   * **Directory:** Repository name (e.g. `Server_Backup.hbk`).
3. Complete the wizard. The backup task will report **Online** and is ready for scheduled or on-demand backups.

> [!IMPORTANT]
> **Best Practice:** Keep separate folders in Movistar Cloud for Duplicati and HyperBackup (e.g. `/Duplicati_Server/` and `/HyperBackup_NAS/`) so that each application manages its indices in complete isolation.

---

### 2. Duplicati Setup

In the Duplicati destination setup wizard:

| Setting | Value |
| :--- | :--- |
| **Storage Type** | `WebDAV` |
| **Server** | Your server IP (e.g. `192.168.1.100`) |
| **Port** | `5050` (or the host port mapped to `8080`) |
| **Path on server** | Target folder name (e.g. `Backup_Duplicati` or `/Backup_Duplicati/`) |
| **Integrated connection (SSL)** | Disabled (unless using an external HTTPS reverse proxy) |
| **Username** | Any value (e.g. `admin`) |
| **Password** | Any value (e.g. `admin`) |

---

## 📄 License

This project is licensed under the [MIT License](LICENSE).
