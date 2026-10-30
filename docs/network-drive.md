# Mapear Movistar / O2 Cloud como Unidad de Red 💻📂

Esta guía detalla paso a paso cómo montar tu almacenamiento de Movistar u O2 Cloud como un disco local o unidad de red en **Windows**, **macOS** y **Linux** a través del proxy WebDAV.

> [!NOTE]
> A partir de la versión **v2.0.5**, el proxy incorpora un sistema de absorción silenciosa de archivos basura (*DevNull Absorber*). Puedes navegar, crear y mover archivos sin preocuparte de que metadatos como `desktop.ini`, `.DS_Store`, `thumbs.db` o carpetas de papelera se suban a tu nube.

---

## 🪟 1. Windows

### Método 1: Comando rápido por Terminal (Recomendado)
Abre **PowerShell** o **Símbolo del sistema (CMD)** y ejecuta:

```powershell
net use M: http://TU_IP_SERVIDOR:5050 /user:admin admin /persistent:yes
```

* Sustituye `TU_IP_SERVIDOR` por la IP local o dominio de tu servidor (ej: `192.168.1.46`).
* La letra `M:` será la nueva unidad asignada en "Este equipo". Puedes cambiarla por cualquier letra disponible.
* `/persistent:yes` mantendrá la unidad reconectada tras reiniciar Windows.

Para desconectar la unidad en cualquier momento:
```powershell
net use M: /delete
```

---

### Método 2: Explorador de Windows (Gráfico)

1. Abre el **Explorador de archivos** y haz clic en **Este equipo**.
2. En la barra superior, pulsa en los tres puntos `...` y selecciona **Conectar a unidad de red**.
3. Selecciona una letra de unidad libre (por ejemplo `M:`).
4. En **Carpeta**, escribe:
   ```text
   http://TU_IP_SERVIDOR:5050/
   ```
   *(Si usas HTTPS con un proxy inverso tipo Nginx/Traefik: `https://tudominio.com/`)*
5. Marca la casilla **Conectar con otras credenciales** y haz clic en **Finalizar**.
6. Cuando Windows pida usuario y contraseña, introduce cualquier valor (por ejemplo usuario `admin` y contraseña `admin`) y marca *Recordar mis credenciales*.

---

### ⚠️ Ajustes Importantes del Servicio WebClient de Windows

El cliente WebDAV integrado de Windows (*WebClient*) tiene por defecto dos limitaciones impuestas por Microsoft:
1. **Bloqueo de HTTP básico:** Requiere habilitar autenticación básica sobre conexiones HTTP sin SSL.
2. **Límite de tamaño de archivo (50 MB):** Por defecto Windows no permite descargar o transferir archivos mayores a 50 MB por WebDAV.

#### Solución (Ejecutar en PowerShell como Administrador):

```powershell
# 1. Permitir autenticación básica en redes locales (HTTP)
Set-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\WebClient\Parameters" -Name "BasicAuthLevel" -Value 2

# 2. Aumentar el límite máximo de archivo a 4 GB (4294967295 bytes)
Set-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\WebClient\Parameters" -Name "FileSizeLimitInBytes" -Value 4294967295

# 3. Reiniciar el servicio WebClient para aplicar los cambios
Restart-Service WebClient
```

---

### Método 3: Montaje de Alto Rendimiento con Rclone (Para archivos gigantes)

Si deseas el máximo rendimiento con caché en disco y soporte para archivos de cualquier tamaño sin limitaciones de Windows WebClient:

1. Instala [Rclone](https://rclone.org/downloads/) y [WinFsp](https://winfsp.dev/).
2. Configura el remoto en `rclone config` tipo `webdav`:
   * **URL:** `http://TU_IP_SERVIDOR:5050`
   * **Vendor:** `other`
   * **User:** `admin`
   * **Pass:** `admin`
3. Monta la unidad con caché de lectura y escritura:
   ```powershell
   rclone mount movistar: M: --vfs-cache-mode full --vfs-cache-max-size 20G
   ```

---

## 🍏 2. macOS

### Método 1: Finder (Gráfico)

1. En el escritorio de macOS o en Finder, pulsa las teclas **`Cmd (⌘) + K`** (o en la barra de menú: *Ir > Conectarse al servidor...*).
2. En la casilla **Dirección del servidor**, introduce:
   ```text
   http://TU_IP_SERVIDOR:5050
   ```
3. Pulsa **Conectar**.
4. Selecciona **Usuario registrado**:
   * **Nombre:** `admin`
   * **Contraseña:** `admin`
   * Marca *Guardar esta contraseña en el llavero*.
5. La unidad aparecerá en el Finder y en tu escritorio como un volumen compartido.

---

### Método 2: Terminal de macOS

Para montar la unidad en una carpeta del sistema por línea de comandos:

```bash
# Crear el punto de montaje
mkdir -p ~/Desktop/MovistarCloud

# Montar mediante mount_webdav
mount_webdav -s http://admin:admin@TU_IP_SERVIDOR:5050 ~/Desktop/MovistarCloud
```

Para desmontar:
```bash
umount ~/Desktop/MovistarCloud
```

---

## 🐧 3. Linux

### Método 1: Entorno Gráfico (GNOME Nautilus / KDE Dolphin)

#### En GNOME Files (Nautilus):
1. Abre el explorador de archivos y haz clic en **Otras ubicaciones** en la barra lateral.
2. En la parte inferior (*Conectar al servidor*), escribe:
   ```text
   dav://TU_IP_SERVIDOR:5050/
   ```
   *(Si usas HTTPS, usa `davs://`)*.
3. Haz clic en **Conectar** e introduce cualquier usuario y contraseña (ej: `admin` / `admin`).

#### En KDE Dolphin:
1. En la barra de dirección pulsa `Ctrl + L` e introduce:
   ```text
   webdav://TU_IP_SERVIDOR:5050/
   ```
2. Introduce credenciales cuando las solicite.

---

### Método 2: Montaje persistente en el sistema con `davfs2`

1. Instala el paquete `davfs2`:
   ```bash
   # Debian / Ubuntu / Raspberry Pi OS
   sudo apt update && sudo apt install -y davfs2

   # Arch Linux / Manjaro
   sudo pacman -S davfs2

   # Fedora / RHEL
   sudo dnf install -y davfs2
   ```

2. Crea el directorio de montaje:
   ```bash
   sudo mkdir -p /mnt/movistar_cloud
   ```

3. Monta manualmente:
   ```bash
   sudo mount -t davfs http://TU_IP_SERVIDOR:5050 /mnt/movistar_cloud
   ```
   *(Te solicitará usuario y contraseña; puedes poner `admin` en ambos).*

4. **Montaje automático sin contraseña (`/etc/fstab`):**
   Añade tus credenciales en `/etc/davfs2/secrets`:
   ```text
   http://TU_IP_SERVIDOR:5050 admin admin
   ```
   Y añade la siguiente línea a `/etc/fstab`:
   ```text
   http://TU_IP_SERVIDOR:5050 /mnt/movistar_cloud davfs _netdev,auto,user,rw 0 0
   ```

---

### Método 3: Montaje con Rclone en Linux (Recomendado para servidores)

```bash
rclone mount movistar: /mnt/movistar_cloud \
  --vfs-cache-mode full \
  --vfs-cache-max-size 30G \
  --allow-other \
  --daemon
```

---

## 🧹 Protección Automática contra Archivos Basura

Al trabajar con carpetas compartidas en red, los tres sistemas operativos generan archivos ocultos que pueden ensuciar la nube. El proxy filtra y absorbe automáticamente:

* **Windows:** `desktop.ini`, `autorun.inf`, `thumbs.db`, `$RECYCLE.BIN`, `System Volume Information`.
* **macOS:** `.DS_Store`, archivos AppleDouble `._*`, `.Spotlight-V100`, `.Trashes`, `.fseventsd`.
* **Linux:** Carpetas `.Trash-*` y `.directory`.

Todas las operaciones de creación sobre estos elementos reciben respuesta satisfactoria inmediata sin generar tráfico hacia Movistar/O2 Cloud ni lanzar errores en tu equipo.
