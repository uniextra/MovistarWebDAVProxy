package webdav

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
)

const (
	ProxyInternalDir = ".movistar-webdav-proxy"
	ManifestFileName = "empty-files.json"
)

var (
	manifestDebounceDur = 30 * time.Second
)

// EmptyFilesManifest represents the cloud-persisted manifest of 0-byte files.
type EmptyFilesManifest struct {
	Version int                      `json:"version"`
	Files   []VirtualEmptyFileExport `json:"files"`
}

type VirtualEmptyFileExport struct {
	Path    string `json:"path"`
	ModTime string `json:"mod_time"`
}

// isInternalProxyPath returns true if the path belongs to internal proxy storage.
func isInternalProxyPath(cleanPath string) bool {
	p := strings.ToLower(cleanPath)
	return p == "/"+ProxyInternalDir || strings.HasPrefix(p, "/"+ProxyInternalDir+"/")
}

// EmptyFileManager manages debounced cloud persistence and bootstrap of virtual 0-byte files.
type EmptyFileManager struct {
	mfs         *MovistarFS
	mu          sync.Mutex
	bootstrapMu sync.Mutex
	timer       *time.Timer
	saving      bool
	pending     bool
}

func NewEmptyFileManager(mfs *MovistarFS) *EmptyFileManager {
	return &EmptyFileManager{
		mfs: mfs,
	}
}

// emptyFilesDirtyKey marca en SQLite que hay cambios en los archivos vacíos emulados
// que aún no están en el manifiesto de la nube. Sobrevive a reinicios y
// actualizaciones del contenedor: al arrancar, si está puesta, se sube el estado
// local en lugar de cargar el manifiesto (que es más antiguo). Antes se cargaba
// igualmente y podía resucitar .cinfo viejos de Hyper Backup sobre los nuevos
// («cloud-seq-id [1], local-seq-id [24]»).
const emptyFilesDirtyKey = "empty_files_manifest_dirty"

// emptyFilesCloudKey guarda «ID:sha256» del manifiesto que hay en la nube (el último
// subido o el cargado al arrancar). Si el que se va a subir es idéntico, no se sube:
// cada subida, renombrado o borrado programa un guardado, pero solo cambia el
// manifiesto cuando cambian los archivos vacíos emulados. La app oficial no sube nada
// que no haya cambiado.
const emptyFilesCloudKey = "empty_files_manifest_cloud"

func manifestFingerprint(id string, data []byte) string {
	sum := sha256.Sum256(data)
	return id + ":" + hex.EncodeToString(sum[:])
}

// ScheduleSave triggers a debounced save of the virtual empty files manifest to SAPI.
func (efm *EmptyFileManager) ScheduleSave() {
	if efm == nil || efm.mfs == nil {
		return
	}
	if ms := efm.mfs.MetadataStore(); ms != nil {
		_ = ms.SetSyncState(emptyFilesDirtyKey, "1")
	}
	efm.mu.Lock()
	defer efm.mu.Unlock()

	efm.pending = true
	if efm.timer != nil {
		efm.timer.Stop()
	}
	efm.timer = time.AfterFunc(manifestDebounceDur, func() {
		efm.SaveManifest()
	})
}

// SaveManifest serializes all virtual empty files and uploads empty-files.json to SAPI.
func (efm *EmptyFileManager) SaveManifest() {
	if efm == nil || efm.mfs == nil {
		return
	}
	efm.mu.Lock()
	if efm.saving {
		efm.pending = true
		efm.mu.Unlock()
		return
	}
	efm.saving = true
	efm.pending = false
	efm.mu.Unlock()

	defer func() {
		efm.mu.Lock()
		efm.saving = false
		if efm.pending {
			efm.pending = false
			efm.timer = time.AfterFunc(manifestDebounceDur, func() {
				efm.SaveManifest()
			})
		}
		efm.mu.Unlock()
	}()

	ms := efm.mfs.MetadataStore()
	if ms == nil || efm.mfs.client == nil {
		return
	}

	files, err := ms.ListAllVirtualEmptyFiles()
	if err != nil {
		logger.Log("[EmptyFiles] ⚠️ Error listando archivos vacíos para manifiesto: %v", err)
		return
	}

	manifest := EmptyFilesManifest{
		Version: 1,
		Files:   make([]VirtualEmptyFileExport, 0, len(files)),
	}
	for _, f := range files {
		if isEphemeralEmptyFile(f.Path) {
			continue
		}
		manifest.Files = append(manifest.Files, VirtualEmptyFileExport{
			Path:    f.Path,
			ModTime: f.ModTime,
		})
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		logger.Log("[EmptyFiles] ⚠️ Error codificando manifiesto JSON: %v", err)
		return
	}

	// 1. Locate or create root folder
	root, err := ms.GetRootFolder()
	if err != nil || root == nil {
		logger.Log("[EmptyFiles] ⚠️ No se encontró carpeta raíz para persistir manifiesto")
		return
	}

	// 2. Locate or create .movistar-webdav-proxy folder
	folder, err := ms.GetFolderByParentAndName(root.ID, ProxyInternalDir)
	var folderID int64
	if folder != nil {
		folderID = folder.ID
	} else {
		// Create via SAPI
		createdFolder, err := efm.mfs.client.CreateFolder(ProxyInternalDir, root.ID)
		if err != nil {
			logger.Log("[EmptyFiles] ⚠️ Error creando carpeta '%s' en SAPI: %v", ProxyInternalDir, err)
			return
		}
		folderID = createdFolder.ID
		_ = ms.UpsertFolder(*createdFolder)
	}

	// 3. Check if empty-files.json already exists in folder
	existingMedia, _ := ms.GetMediaByFolderAndName(folderID, ManifestFileName)
	var existingID string
	if existingMedia != nil {
		existingID = existingMedia.ID
	}

	// 4. Si en la nube ya está este mismo manifiesto, no hay nada que subir.
	if existingID != "" {
		if prev, _ := ms.GetSyncState(emptyFilesCloudKey); prev == manifestFingerprint(existingID, data) {
			efm.mu.Lock()
			clean := !efm.pending
			efm.mu.Unlock()
			if clean {
				_ = ms.SetSyncState(emptyFilesDirtyKey, "0")
			}
			return
		}
	}

	// 5. Upload manifest to SAPI
	now := time.Now()
	uploaded, err := efm.mfs.client.UploadFileToFolder(
		ManifestFileName,
		bytes.NewReader(data),
		int64(len(data)),
		folderID,
		"application/json",
		existingID,
		now,
	)
	if err != nil {
		// sesión caída, 401, WAF…: la próxima subida del cliente podría tardar horas
		retry := config.EmptyManifestRetryWait()
		logger.Log("[EmptyFiles] ⚠️ Error subiendo manifiesto a SAPI: %v (se reintenta en %s)", err, retry)
		time.AfterFunc(retry, efm.ScheduleSave)
		return
	}
	if uploaded != nil {
		_ = ms.UpsertMedia(*uploaded)
		_ = ms.SetSyncState(emptyFilesCloudKey, manifestFingerprint(uploaded.ID, data))
	}
	efm.mu.Lock()
	clean := !efm.pending
	efm.mu.Unlock()
	if clean {
		_ = ms.SetSyncState(emptyFilesDirtyKey, "0")
	}
	logger.Log("[EmptyFiles] ☁️ Manifiesto persistido en la nube (%d archivos vacíos emulados)", len(manifest.Files))
}

// BootstrapLoad loads the empty-files manifest from SAPI/cloud into the local SQLite store if missing.
func (efm *EmptyFileManager) BootstrapLoad() {
	if efm == nil || efm.mfs == nil {
		return
	}
	efm.bootstrapMu.Lock()
	defer efm.bootstrapMu.Unlock()

	ms := efm.mfs.MetadataStore()
	if ms == nil || efm.mfs.client == nil {
		return
	}
	efm.purgeLocalEphemeralFiles()
	if efm.mfs.client.IsSessionExpired() {
		return
	}
	if dirty, _ := ms.GetSyncState(emptyFilesDirtyKey); dirty == "1" {
		logger.Log("[EmptyFiles] ⬆️ Hay cambios locales de archivos vacíos sin subir: se sube el estado local en lugar de cargar el manifiesto de la nube (más antiguo).")
		efm.ScheduleSave()
		return
	}

	root, err := ms.GetRootFolder()
	if err != nil || root == nil {
		return
	}

	folder, _ := ms.GetFolderByParentAndName(root.ID, ProxyInternalDir)
	if folder == nil {
		return
	}

	media, _ := ms.GetMediaByFolderAndName(folder.ID, ManifestFileName)
	if media == nil || media.ID == "" {
		return
	}

	// Download content
	rc, err := efm.mfs.client.DownloadFileStream(*media)
	if err != nil {
		logger.Log("[EmptyFiles] ⚠️ Error descargando manifiesto: %v", err)
		return
	}
	defer rc.Close()

	body, err := io.ReadAll(io.LimitReader(rc, 10<<20)) // 10 MB limit
	if err != nil {
		return
	}

	var manifest EmptyFilesManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		logger.Log("[EmptyFiles] ⚠️ Manifiesto JSON inválido: %v", err)
		return
	}
	_ = ms.SetSyncState(emptyFilesCloudKey, manifestFingerprint(media.ID, body))

	loadedCount := 0
	skippedEphemeral := false
	for _, f := range manifest.Files {
		cleanPath := path.Clean("/" + strings.TrimSpace(f.Path))
		if isEphemeralEmptyFile(cleanPath) {
			skippedEphemeral = true
			continue
		}
		dir := path.Dir(cleanPath)
		baseName := path.Base(cleanPath)

		// Ensure folder chain in SQLite
		parentFolder, err := efm.mfs.ensureFolderChain(dir)
		if err != nil || parentFolder == nil {
			continue
		}
		modTime := f.ModTime
		if modTime == "" {
			modTime = time.Now().UTC().Format("20060102T150405Z")
		}
		if err := ms.UpsertVirtualEmptyFile(parentFolder.ID, baseName, modTime); err == nil {
			loadedCount++
		}
	}
	logger.Log("[EmptyFiles] 📥 Cargados %d archivos vacíos emulados desde el manifiesto en la nube", loadedCount)
	if skippedEphemeral {
		logger.Log("[EmptyFiles] 🧹 Descartados archivos efímeros (locks/estados transitorios) del manifiesto; reescribiendo limpio...")
		efm.ScheduleSave()
	}
}

// isEphemeralEmptyFile identifica archivos vacíos transitorios de bloqueo
// (locks y sesiones @reader de Hyper Backup) que solo deben existir durante una
// operación activa y nunca persistirse en el manifiesto ni sobrevivir a reinicios.
// Los archivos dentro de /Control/@writer (incluidos todos los .cinfo de progreso/rollback)
// son estado persistente de Hyper Backup y NO deben purgarse.
func isEphemeralEmptyFile(rawPath string) bool {
	clean := strings.ToLower(path.Clean("/" + rawPath))
	return isControlOrLockFile(clean) || strings.Contains(clean, "/control/lock/")
}

func (efm *EmptyFileManager) purgeLocalEphemeralFiles() {
	ms := efm.mfs.MetadataStore()
	if ms == nil {
		return
	}
	files, err := ms.ListAllVirtualEmptyFiles()
	if err != nil {
		return
	}
	purged := 0
	for _, f := range files {
		if !isEphemeralEmptyFile(f.Path) {
			continue
		}
		cleanPath := path.Clean("/" + f.Path)
		parentFolder, _, isFolder, err := efm.mfs.resolvePathOpts(path.Dir(cleanPath), false)
		if err == nil && isFolder && parentFolder != nil {
			if err := ms.DeleteVirtualEmptyFile(parentFolder.ID, path.Base(cleanPath)); err == nil {
				purged++
			}
		}
	}
	if purged > 0 {
		logger.Log("[EmptyFiles] 🧹 Purgados %d archivos vacíos efímeros huérfanos de la caché local al arrancar", purged)
	}
}
