# Configuración de Clientes de Copia de Seguridad 💾🛡️

Esta guía detalla la configuración óptima para realizar copias de seguridad automáticas hacia Movistar / O2 Cloud mediante el proxy WebDAV con **Synology HyperBackup**, **Duplicati** y **rclone**.

---

## 🗄️ 1. Synology HyperBackup

HyperBackup de Synology NAS es 100% compatible con el proxy WebDAV de Movistar/O2 Cloud gracias a la implementación de las cuotas virtuales RFC 4331 y el soporte de repositorios `.hbk`.

### Pasos de Configuración:

1. En tu NAS Synology (DSM), abre **Hyper Backup**.
2. Pulsa en el icono `+` para crear una nueva tarea de copia de seguridad de datos.
3. En la lista de destinos, selecciona **WebDAV** dentro de la categoría *Servidor de archivos*.
4. Configura los parámetros de conexión:
   * **Dirección del servidor:** `http://IP_DE_TU_SERVIDOR:5050` (o `https://dominio.com` con proxy inverso).
   * **Nombre de usuario:** El usuario configurado en `WEBDAV_USER` (ej. `admin`).
   * **Contraseña:** La contraseña configurada en `WEBDAV_PASSWORD`.
   * **Carpeta:** Escribe o selecciona una carpeta dedicada (ejemplo: `HyperBackup_NAS`).
   * **Directorio:** Nombre del repositorio `.hbk` que creará HyperBackup (ejemplo: `NAS_Backup.hbk`).
5. Pulsa en **Siguiente** para seleccionar las carpetas compartidas de tu NAS que deseas incluir en la copia.
6. Ajusta el calendario de programación y la rotación de versiones a tu preferencia.
7. Guarda la tarea. El estado reportará **En línea** y podrás ejecutar la copia inmediatamente.

> [!TIP]
> Mantén siempre una carpeta dedicada y exclusiva para HyperBackup (ej: `/HyperBackup_NAS/`) para aislar los índices de DSM de otras copias.

---

## 🔄 2. Duplicati

Duplicati es una solución multiplataforma de copias de seguridad encriptadas e incrementales compatible con Windows, Linux, macOS y Docker.

### Pasos de Configuración:

1. Abre la interfaz web de **Duplicati**.
2. Crea una nueva tarea de copia de seguridad (*Añadir copia de seguridad* > *Configurar una nueva copia*).
3. Asigna un nombre a la copia y una contraseña de encriptación fuerte (AES-256).
4. En el paso **Destino de la copia**:
   * **Tipo de almacenamiento:** `WebDAV`.
   * **Servidor y puerto:** La IP de tu servidor (ej: `192.168.1.46`) y puerto `5050` (o el asignado a `8080` de Docker).
   * **Ruta en el servidor:** Nombre de la carpeta de destino en la nube (ejemplo: `Duplicati_Servidor` o `/Duplicati_Servidor/`).
   * **Conexión integrada (SSL):** Desactivado (a menos que uses proxy inverso HTTPS).
   * **Nombre de usuario:** El usuario definido en `WEBDAV_USER` (ej. `admin`).
   * **Contraseña:** La contraseña definida en `WEBDAV_PASSWORD`.
5. Pulsa el botón **Probar conexión**. La sonda `duplicati-access-privileges-test.tmp` recibe `415 Unsupported Media Type`: SAPI rechaza `.tmp` ([issue #13](https://github.com/uniextra/MovistarWebDAVProxy/issues/13)). El proxy ya no simula éxito en RAM. No ignores este fallo para considerar validado el destino: esta prueba no es compatible con el backend actual. Tampoco se admiten archivos de datos `.tmp` ni `COPY`/`MOVE` hacia esa extensión. No hay un mapeo de nombres alternativo.
6. Selecciona los datos locales a respaldar y finaliza el asistente.

Antes de confiar en cualquier copia, verifica una restauración desde una instancia
nueva del proxy (caché vacía), no solo un `GET` inmediato que puede servirse desde
RAM. Conserva otra copia independiente. El spool local de subidas grandes debe
usar un volumen de disco dimensionado, no tmpfs; consulta el
[apartado de espacio temporal del README](../README.md#espacio-temporal-para-subidas-grandes).

---

## 🚀 3. Rclone (Sync y Backup)

Rclone permite sincronizar, copiar y respaldar directorios locales o de otros proveedores directamente contra Movistar Cloud.

### Configuración en `rclone.conf`:

```ini
[movistar]
type = webdav
url = http://TU_IP_SERVIDOR:5050
vendor = other
user = admin
pass = TuContraseñaOfuscadaConRcloneObscure
```

### Comandos útiles:

* **Listar carpetas:**
  ```bash
  rclone lsd movistar:
  ```
* **Copiar un directorio local a la nube:**
  ```bash
  rclone copy /ruta/a/mis/fotos movistar:Fotos --transfers 3 --progress
  ```
* **Sincronizar espejo:**
  ```bash
  rclone sync /ruta/datos movistar:Backup_Datos --transfers 3 --progress
  ```

> [!NOTE]
> Se recomienda limitar `--transfers 3` para alinearse con el semáforo interno del proxy y aprovechar el máximo ancho de banda sin saturar la API de Telefónica.
