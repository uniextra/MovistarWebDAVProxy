# Mapear Movistar / O2 Cloud como Unidad de Red 💻📂

Esta guía detalla paso a paso cómo montar tu almacenamiento de Movistar u O2 Cloud como un disco local o unidad de red en **Windows**, **macOS** y **Linux** a través del proxy WebDAV.

> [!NOTE]
> A partir de la versión **v2.0.5**, el proxy incorpora un sistema de absorción silenciosa de archivos basura (*DevNull Absorber*). Puedes navegar, crear y mover archivos sin preocuparte de que metadatos como `desktop.ini`, `.DS_Store`, `thumbs.db` o carpetas de papelera se suban a tu nube.

---

## 🪟 1. Windows

### Método 1: Montaje Silencioso al Iniciar Sesión con DPAPI (Recomendado)

> [!IMPORTANT]
> **Por qué no usar `/persistent:yes` en WebDAV (Microsoft KB 2673544):**  
> En Windows 10 y Windows 11, el servicio nativo WebClient no puede proporcionar contraseñas guardadas en el Administrador de Credenciales al reconectar unidades WebDAV tras un reinicio ([KB 2673544](https://learn.microsoft.com/en-US/troubleshoot/windows-client/networking/cannot-automatically-reconnect-dav-share)). En su lugar, Windows envía el usuario correcto con la **contraseña vacía**, provocando fallos de autenticación repetidos y bloqueos por rate limit (HTTP 429).
>
> La solución recomendada consiste en montar la unidad con un script al iniciar sesión con la contraseña protegida mediante **DPAPI** (cifrada con la clave de sesión de tu usuario en Windows) y el parámetro `/persistent:no`.

#### Paso 1: Configurar el servicio WebClient (PowerShell como Administrador, una sola vez)
```powershell
# Configurar servicio WebClient en Automático y habilitar HTTP básico y tamaño 4 GB
Set-Service -Name WebClient -StartupType Automatic
Start-Service WebClient
Set-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\WebClient\Parameters" -Name "BasicAuthLevel" -Value 2
Set-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\WebClient\Parameters" -Name "FileSizeLimitInBytes" -Value 4294967295
Restart-Service WebClient
```

#### Paso 2: Limpiar montajes persistentes anteriores (PowerShell normal)
```powershell
net use M: /delete /y
cmdkey /delete:TU_IP_SERVIDOR
```

#### Paso 3: Guardar la contraseña cifrada con DPAPI (PowerShell normal)
Se te solicitará la contraseña una sola vez y quedará almacenada cifrada:
```powershell
Read-Host "Contraseña WebDAV" -AsSecureString | ConvertFrom-SecureString | Out-File "$env:LOCALAPPDATA\montar-m.sec"
```

#### Paso 4: Crear el script `C:\Scripts\montar-m.ps1`
Crea la carpeta `C:\Scripts` y el archivo `montar-m.ps1` con el siguiente contenido (sustituye `TU_IP_SERVIDOR` y `TU_USUARIO`):
```powershell
$letra = 'M:'
$url = 'http://TU_IP_SERVIDOR:5050'
$usuario = 'TU_USUARIO'
$log = "$env:LOCALAPPDATA\montar-m.log"
$sec = Get-Content "$env:LOCALAPPDATA\montar-m.sec" | ConvertTo-SecureString
$pass = [Net.NetworkCredential]::new('', $sec).Password

for ($i = 1; $i -le 20; $i++) {
    if (Test-Path "$letra\") { exit 0 }
    if ((Get-Service WebClient).Status -ne 'Running') {
        Start-Sleep -Seconds 3; continue
    }
    $out = ($null | & net.exe use $letra $url "/user:$usuario" $pass /persistent:no 2>&1) -join ' '
    if ($LASTEXITCODE -eq 0 -or (Test-Path "$letra\")) { exit 0 }
    if ($out -match '(sistema|error) (5|59|86|1244|1326)\.') { exit 1 }
    Start-Sleep -Seconds 5
}
```

#### Paso 5: Programar el montaje automático
Puedes elegir cualquiera de estas dos opciones:
* **Opción A (Tarea programada - Recomendado):** Abre CMD como Administrador:
  ```cmd
  schtasks /create /tn "Montar M WebDAV" /sc onlogon /ru "%USERNAME%" /it /rl limited /tr "conhost.exe --headless powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\Scripts\montar-m.ps1"
  ```
* **Opción B (Carpeta Inicio de Windows - Sin permisos de administrador):**  
  Crea un acceso directo en `shell:startup` (`%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup`) con la línea:
  ```text
  conhost.exe --headless powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\Scripts\montar-m.ps1
  ```

---

### Método 2: Montaje Rápido en la Sesión Actual (Temporal)
Si solo necesitas montar la unidad puntualmente en la sesión activa sin persistencia tras reiniciar:

```powershell
net use M: http://TU_IP_SERVIDOR:5050 /user:TU_USUARIO TU_PASSWORD /persistent:no
```

Para desmontar:
```powershell
net use M: /delete /y
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

> [!WARNING]
> **Límite de 4 GB por archivo en Movistar / O2 Cloud:**  
> La API de Telefónica no admite archivos individuales mayores a 4 GB (4096 MB). Si necesitas almacenar archivos gigantes (por ejemplo imágenes ISO de más de 4 GB), puedes configurar un remoto secundario tipo [`chunker`](https://rclone.org/chunker/) sobre tu remoto `movistar:` con un tamaño de bloque de 2 GB (`chunk_size = 2G`) para fragmentarlos de forma transparente.

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
