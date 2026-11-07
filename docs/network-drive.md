# Mapear Movistar / O2 Cloud como Unidad de Red 💻📂

Esta guía detalla paso a paso cómo montar tu almacenamiento de Movistar u O2 Cloud como un disco local o unidad de red en **Windows**, **macOS** y **Linux** a través del proxy WebDAV.

> [!NOTE]
> A partir de la versión **v2.0.5**, el proxy incorpora un sistema de absorción silenciosa de archivos basura (*DevNull Absorber*). Puedes navegar, crear y mover archivos sin preocuparte de que metadatos como `desktop.ini`, `.DS_Store`, `thumbs.db` o carpetas de papelera se suban a tu nube.

---

## 🪟 1. Windows

> [!CAUTION]
> **Aviso de Compatibilidad: El montaje nativo en Windows NO está soportado al 100% ni se recomienda para uso diario.**  
> El cliente nativo de Windows (`mrxdav.sys` / servicio `WebClient`) fue declarado oficialmente **obsoleto (*deprecated*) por Microsoft en 2023** y sufre de graves limitaciones de diseño:
> * **Descarga forzosa al interactuar:** El Explorador de Windows descarga los archivos íntegros a disco local antes de leer miniaturas o borrarlos.
> * **Reconexión rota tras reinicios ([KB 2673544](https://learn.microsoft.com/en-US/troubleshoot/windows-client/networking/cannot-automatically-reconnect-dav-share)):** Windows olvida la contraseña al reiniciar y envía contraseñas vacías, bloqueando el acceso.
> * **Límite de 4 GB y timeouts:** No soporta transferencias mayores a 4 GB y corta conexiones a los 60 segundos.
>
> Por estos motivos, **el Método 1 (Rclone + WinFsp) es la solución oficial y recomendada** para usuarios de Windows.

---

### Método 1: Montaje de Alto Rendimiento con Rclone + WinFsp (Recomendado en Windows)

Esta solución proporciona una experiencia de disco local 100% nativa, rápida y sin los bloqueos ni bugs de `WebClient`:

1. Instala [WinFsp](https://winfsp.dev/) y [Rclone](https://rclone.org/downloads/) (o en PowerShell: `winget install Rclone.Rclone; winget install WinFsp.WinFsp`).
2. Configura el remoto ejecutando `rclone config` (tipo `webdav`):
   * **URL:** `http://TU_IP_SERVIDOR:5050`
   * **Vendor:** `other`
   * **User:** tu usuario WebDAV
   * **Pass:** tu contraseña WebDAV
3. Monta la unidad como disco local con streaming y caché transparente:
   ```powershell
   rclone mount movistar: M: --vfs-cache-mode full --vfs-cache-max-size 20G
   ```

---

### Método 2: Montaje Nativo de Windows (Experimental / No Soportado Oficialmente)

Si deseas utilizar el cliente nativo de Windows a pesar de sus limitaciones, este procedimiento automatiza el montaje mediante script al inicio de sesión mitigando parte de los fallos de `WebClient`:

#### Paso 1: Configurar el servicio WebClient (PowerShell como Administrador, una sola vez)
```powershell
Set-Service -Name WebClient -StartupType Automatic
Start-Service WebClient
Set-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\WebClient\Parameters" -Name "BasicAuthLevel" -Value 2
Set-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\WebClient\Parameters" -Name "FileSizeLimitInBytes" -Value 4294967295
Set-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\WebClient\Parameters" -Name "SendReceiveTimeoutInSec" -Value 3600
Restart-Service WebClient
```

#### Paso 2: Limpiar montajes persistentes anteriores (PowerShell normal)
```powershell
net use M: /delete /y
cmdkey /delete:TU_IP_SERVIDOR
```

#### Paso 3: Guardar la contraseña cifrada con DPAPI (PowerShell normal)
```powershell
Read-Host "Contraseña WebDAV" -AsSecureString | ConvertFrom-SecureString | Out-File "$env:LOCALAPPDATA\montar-m.sec"
```

#### Paso 4: Crear el script `C:\Scripts\montar-m.ps1`
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
* **Opción A (Tarea programada):** En CMD como Administrador:
  ```cmd
  schtasks /create /tn "Montar M WebDAV" /sc onlogon /ru "%USERNAME%" /it /rl limited /tr "conhost.exe --headless powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\Scripts\montar-m.ps1"
  ```
* **Opción B (Carpeta Inicio):** Acceso directo en `shell:startup` con destino:
  ```text
  conhost.exe --headless powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\Scripts\montar-m.ps1
  ```

---

### Método 3: Montaje Rápido Temporal (Solo sesión activa)
```powershell
net use M: http://TU_IP_SERVIDOR:5050 /user:TU_USUARIO TU_PASSWORD /persistent:no
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
