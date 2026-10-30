# Movistar / O2 Cloud WebDAV Proxy ☁️🔐

[![Docker](https://img.shields.io/badge/Docker-Ready-blue?logo=docker)](https://hub.docker.com/r/uniextra/movistar-webdav-proxy)
[![Go Report Card](https://goreportcard.com/badge/github.com/uniextra/MovistarWebDAVProxy)](https://goreportcard.com/report/github.com/uniextra/MovistarWebDAVProxy)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![WebDAV](https://img.shields.io/badge/WebDAV-RFC%204918%20%7C%20RFC%204331-green)](https://tools.ietf.org/html/rfc4918)
[![Image Size](https://img.shields.io/badge/Image%20Size-~20%20MB-success)](https://hub.docker.com/r/uniextra/movistar-webdav-proxy)
[![RAM Usage](https://img.shields.io/badge/RAM%20Usage-~6.5%20MB-success)](https://hub.docker.com/r/uniextra/movistar-webdav-proxy)

Proxy WebDAV nativo de alto rendimiento escrito íntegramente en **Go** que traduce peticiones estándar WebDAV (**Synology HyperBackup**, **Duplicati**, **Rclone**, mapeo de discos en **Windows / macOS / Linux**) a la API oficial de **Movistar Cloud España** y **O2 Cloud** en tiempo real y sin intermediarios.

Transforma tu almacenamiento ilimitado en una unidad remota rápida, transparente y segura para tus servidores, ordenadores y copias de seguridad de NAS.

<p align="center">
  <img src="screenshot.png" alt="Movistar & O2 Cloud WebDAV Proxy Web Dashboard" width="850">
</p>

---

## 📑 Índice de Contenidos

1. [🌟 Características Principales](#-características-principales)
2. [🐳 Despliegue con Docker](#-despliegue-con-docker)
3. [🔑 Vinculación Inicial (Emparejamiento SMS)](#-vinculación-inicial-emparejamiento-sms)
4. [💻 Mapear como Unidad de Red (Windows, macOS, Linux)](#-mapear-como-unidad-de-red-windows-macos-linux)
5. [⚙️ Clientes de Copia de Seguridad (HyperBackup, Duplicati)](#️-clientes-de-copia-de-seguridad-hyperbackup-duplicati)
6. [🧹 Filtro Inteligente de Basura del SO (v2.0.5)](#-filtro-inteligente-de-basura-del-so-v205)
7. [📚 Documentación Detallada](#-documentación-detallada)
8. [📄 Licencia](#-licencia)

---

## 🌟 Características Principales

* 🚀 **Extremadamente Ligero y Eficiente (Go Nativo):** Binario estático compilado en contenedor Docker de solo **~20 MB** con consumo de memoria de apenas **~6.5 MB de RAM**.
* 🔄 **Traducción Completa de WebDAV (RFC 4918):** Soporta `PROPFIND` (paginación automática de miles de archivos), `PUT` (streaming directo a disco sin saturar RAM), `GET`/`HEAD` (con soporte para peticiones `Range` y streaming por chunks), `MKCOL`, `DELETE`, `MOVE`, `COPY` y bloqueos `LOCK`/`UNLOCK`.
* 📊 **Cuotas Virtuales RFC 4331:** Informa del espacio disponible para que asistentes como Synology DSM o Duplicati no fallen por chequeos de espacio insuficiente.
* 🚦 **Semáforo de Concurrencia (3 Transfers Máximo):** Optimiza las subidas y descargas masivas al ancho de banda óptimo de Telefónica previniendo bloqueos por *rate-limit*.
* 📱 **Autenticación Oficial Windows + OAuth2 PKCE:** Emula el cliente de escritorio oficial (`omh windows client`) con sincronización milimétrica de timestamps y soporte para verificación SMS oficial desde el navegador.
* 🛡️ **Protección contra Colisiones:** Previene la creación de duplicados automáticos como ` (1)` al actualizar o mover archivos.
* 🧹 **Absorbedor de Basura de SO (*DevNull Absorber*):** Filtra y neutraliza automáticamente archivos innecesarios de Windows, macOS y Linux.

---

## 🐳 Despliegue con Docker

### Opción 1: Docker Compose (Recomendado)

Crea un archivo `docker-compose.yml`:

```yaml
version: '3.8'

services:
  movistar-webdav-proxy:
    image: uniextra/movistar-webdav-proxy:latest
    container_name: movistar-webdav-proxy
    restart: unless-stopped
    ports:
      - "5000:5000"      # Panel Web de Gestión y Logs
      - "5050:8080"      # Endpoint WebDAV (puerto 5050 en el host)
    volumes:
      - movistar_tokens:/app/tokens

volumes:
  movistar_tokens:
    name: movistar_webdav_tokens
```

Inicia el servicio:
```bash
docker compose up -d
```

### Opción 2: Docker CLI

```bash
docker run -d \
  --name movistar-webdav-proxy \
  --restart unless-stopped \
  -p 5000:5000 \
  -p 5050:8080 \
  -v movistar_webdav_tokens:/app/tokens \
  uniextra/movistar-webdav-proxy:latest
```

---

## 🔑 Vinculación Inicial (Emparejamiento SMS)

1. Abre tu navegador y entra al panel de control: `http://IP_DE_TU_SERVIDOR:5000`.
2. Haz clic en **"Vincular Dispositivo (OAuth2 SMS)"**.
3. Selecciona tu proveedor (**Movistar Cloud** u **O2 Cloud**) e introduce tu número de teléfono móvil.
4. Pulsa en **"Abrir Página de Login en Nueva Pestaña"** para acceder al portal oficial de Telefónica y escribe el código SMS recibido.
5. Copia la URL final redirigida en tu navegador (ej: `https://micloud.movistar.es/...code=...`), pégala en el panel y pulsa **"Completar Vinculación"**.
6. ¡Listo! El servidor WebDAV quedará activo y escuchando en el puerto `5050`.

---

## 💻 Mapear como Unidad de Red (Windows, macOS, Linux)

Puedes montar la nube como si fuera un disco duro o carpeta compartida local:

| Sistema Operativo | Método Rápido | Ruta / Comando |
| :--- | :--- | :--- |
| **🪟 Windows** | PowerShell / CMD | `net use M: http://TU_IP:5050 /user:admin admin /persistent:yes` |
| **🍏 macOS** | Finder (`Cmd + K`) | `http://TU_IP:5050` (Usuario: `admin` / Clave: `admin`) |
| **🐧 Linux** | GNOME Nautilus / `davfs2` | `dav://TU_IP:5050/` o `mount -t davfs http://TU_IP:5050 /mnt/cloud` |

👉 **Para instrucciones paso a paso, ajustes del registro de Windows (límite de 50MB de Microsoft) y montaje con Rclone, consulta la [Guía Completa de Mapeo de Red](docs/network-drive.md).**

---

## ⚙️ Clientes de Copia de Seguridad (HyperBackup, Duplicati)

El proxy está optimizado para actuar como repositorio remoto de copias automáticas:

* **Synology HyperBackup:** Destino `WebDAV` > `http://TU_IP:5050` > Usuario/Clave `admin` > Carpeta `HyperBackup_NAS` > Repositorio `Copia.hbk`.
* **Duplicati:** Tipo de almacenamiento `WebDAV` > Servidor `TU_IP`, puerto `5050` > Carpeta `Duplicati_Server`.
* **Rclone:** Remoto tipo `webdav` con soporte de `--transfers 3` y comandos `sync` / `copy`.

👉 **Instrucciones detalladas y capturas en la [Guía de Clientes de Backup](docs/backup-clients.md).**

---

## 🧹 Filtro Inteligente de Basura del SO (v2.0.5)

Al conectar la unidad en red, los sistemas operativos intentan generar archivos de indexación y metadatos que ensucian la nube. A partir de la versión **v2.0.5**, el proxy integra un absorbedor en memoria (*DevNull Absorber*) que responde con éxito al SO pero evita que se suban a la nube:

* **Windows:** `desktop.ini`, `autorun.inf`, `thumbs.db`, carpetas `$RECYCLE.BIN` y `System Volume Information`.
* **macOS:** `.DS_Store`, AppleDouble `._*`, `.Spotlight-V100`, `.Trashes`, `.fseventsd`, `.TemporaryItems`.
* **Linux:** Carpetas `.Trash-*` y `.directory` de KDE.

---

## 📚 Documentación Detallada

* [📖 Mapear como Unidad de Red en Windows, macOS y Linux](docs/network-drive.md)
* [🛡️ Configurar Synology HyperBackup, Duplicati y Rclone](docs/backup-clients.md)

---

## 📄 Licencia

Este proyecto está bajo licencia [MIT](LICENSE).
