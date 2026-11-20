package webdav

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"movistar-webdav-proxy/internal/cache"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"
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
	// bootstrapped: en este arranque ya se decidió entre el manifiesto de la nube y el
	// estado local (BootstrapLoad terminó). Hasta entonces, una base que nunca ha
	// cargado ni subido un manifiesto no puede subir el suyo: lo pisaría con un estado
	// casi vacío (contenedor nuevo, base local borrada…).
	bootstrapped atomic.Bool
	// migrating: hay en marcha una subida a la nube de los vacíos que siguen emulados
	// (ver startMigration).
	migrating atomic.Bool
	// migrateAgain: se pidió otra migración mientras había una en marcha (el manifiesto
	// se carga más de una vez al arrancar); se repite al terminar.
	migrateAgain atomic.Bool
}

func NewEmptyFileManager(mfs *MovistarFS) *EmptyFileManager {
	return &EmptyFileManager{
		mfs: mfs,
	}
}

// emptyFilesDirtyKey marca en SQLite que hay cambios en los archivos vacíos emulados
// que aún no están en el manifiesto de la nube. Sobrevive a reinicios y
// actualizaciones del contenedor: al arrancar, si está puesta y esta base ya conocía
// el manifiesto de la nube (emptyFilesCloudKey), se sube el estado local en lugar de
// cargar el manifiesto, que es más antiguo. Antes se cargaba igualmente y podía
// resucitar .cinfo viejos de Hyper Backup sobre los nuevos («cloud-seq-id [1],
// local-seq-id [24]»). Una base que nunca lo ha conocido (nueva) siempre lo carga.
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
	if !efm.mayUpload(ms) {
		// Base que aún no ha cargado el manifiesto de la nube: subir ahora lo pisaría.
		// BootstrapLoad programa el guardado en cuanto lo haya combinado.
		efm.mu.Lock()
		efm.pending = false
		efm.mu.Unlock()
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

// retryBootstrap: no se pudo leer el manifiesto de la nube. Hasta leerlo, una base nueva
// no sube el suyo, así que se vuelve a intentar en lugar de esperar a otro arranque.
func (efm *EmptyFileManager) retryBootstrap(motivo string) {
	retry := config.EmptyManifestRetryWait()
	logger.Log("[EmptyFiles] ⚠️ %s (se reintenta en %s)", motivo, retry)
	time.AfterFunc(retry, efm.BootstrapLoad)
}

// mayUpload: se puede subir el manifiesto si en este arranque ya se resolvió el de la
// nube, o si esta base lo conocía de antes (lo cargó o lo subió alguna vez).
func (efm *EmptyFileManager) mayUpload(ms *cache.MetadataStore) bool {
	if efm.bootstrapped.Load() {
		return true
	}
	known, _ := ms.GetSyncState(emptyFilesCloudKey)
	return known != ""
}

// BootstrapLoad loads the empty-files manifest from SAPI/cloud into the local SQLite store if missing.
func (efm *EmptyFileManager) BootstrapLoad() {
	if efm == nil || efm.mfs == nil {
		return
	}
	efm.bootstrapMu.Lock()
	defer efm.bootstrapMu.Unlock()
	defer efm.startMigration()

	ms := efm.mfs.MetadataStore()
	if ms == nil || efm.mfs.client == nil {
		return
	}
	efm.purgeLocalEphemeralFiles()
	if efm.mfs.client.IsSessionExpired() {
		return
	}
	dirty, _ := ms.GetSyncState(emptyFilesDirtyKey)
	known, _ := ms.GetSyncState(emptyFilesCloudKey)
	if dirty == "1" && known != "" {
		logger.Log("[EmptyFiles] ⬆️ Hay cambios locales de archivos vacíos sin subir: se sube el estado local en lugar de cargar el manifiesto de la nube (más antiguo).")
		efm.bootstrapped.Store(true)
		efm.ScheduleSave()
		return
	}

	root, err := ms.GetRootFolder()
	if err != nil || root == nil {
		return // metadatos aún sin sincronizar: se reintenta al terminar la sincronización
	}

	folder, _ := ms.GetFolderByParentAndName(root.ID, ProxyInternalDir)
	var media *sapi.MediaItem
	if folder != nil {
		media, _ = ms.GetMediaByFolderAndName(folder.ID, ManifestFileName)
	}
	if media == nil || media.ID == "" {
		// No hay manifiesto en la nube: no hay nada que proteger.
		efm.bootstrapped.Store(true)
		if d, _ := ms.GetSyncState(emptyFilesDirtyKey); d == "1" {
			efm.ScheduleSave() // cambios hechos mientras se esperaba (guardados aplazados)
		}
		return
	}

	// Download content
	rc, err := efm.mfs.client.DownloadFileStream(*media)
	if err != nil {
		efm.retryBootstrap(fmt.Sprintf("Error descargando manifiesto: %v", err))
		return
	}
	defer rc.Close()

	body, err := io.ReadAll(io.LimitReader(rc, 10<<20)) // 10 MB limit
	if err != nil {
		efm.retryBootstrap(fmt.Sprintf("Error leyendo manifiesto: %v", err))
		return
	}

	var manifest EmptyFilesManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		// Ilegible: no hay nada que proteger; el próximo guardado lo reescribe.
		logger.Log("[EmptyFiles] ⚠️ Manifiesto JSON inválido: %v", err)
		efm.bootstrapped.Store(true)
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
		if m, _ := ms.GetMediaByFolderAndName(parentFolder.ID, baseName); m != nil && m.ID != "" {
			continue // ya está en la nube como archivo real: no se emula otra vez
		}
		if err := ms.UpsertVirtualEmptyFile(parentFolder.ID, baseName, modTime); err == nil {
			loadedCount++
		}
	}
	logger.Log("[EmptyFiles] 📥 Cargados %d archivos vacíos emulados desde el manifiesto en la nube", loadedCount)
	efm.bootstrapped.Store(true)
	// Se relee: algún guardado pudo aplazarse mientras se cargaba.
	dirty, _ = ms.GetSyncState(emptyFilesDirtyKey)
	switch {
	case dirty == "1":
		// Base nueva con cambios hechos antes de cargar el manifiesto (un cliente subió
		// algo nada más arrancar): ya está combinado con el de la nube y se sube todo.
		logger.Log("[EmptyFiles] ⬆️ Manifiesto de la nube combinado con los cambios locales de este arranque; se sube.")
		efm.ScheduleSave()
	case skippedEphemeral:
		logger.Log("[EmptyFiles] 🧹 Descartados archivos efímeros (locks/estados transitorios) del manifiesto; reescribiendo limpio...")
		efm.ScheduleSave()
	}
}

// startMigration: con los vacíos de verdad (MovistarFS.realEmptyFiles), los que siguen
// emulados (creados antes de activarlo, o cargados del manifiesto de la nube) se suben
// a la nube una vez resuelto el manifiesto en este arranque. Va en segundo plano.
func (efm *EmptyFileManager) startMigration() {
	if !efm.bootstrapped.Load() || !efm.mfs.realEmptyFiles() {
		return
	}
	if !efm.migrating.CompareAndSwap(false, true) {
		efm.migrateAgain.Store(true)
		return
	}
	go func() {
		for {
			efm.migrateToCloud()
			efm.migrating.Store(false)
			if !efm.migrateAgain.Swap(false) || !efm.migrating.CompareAndSwap(false, true) {
				return
			}
		}
	}()
}

// migrateToCloud sube como archivos reales de 0 bytes los vacíos emulados que no sean
// de bloqueo o control. Si en la nube ya hay un archivo con ese nombre (subido de
// verdad en un arranque anterior), solo se quita el emulado. Los que O2 no admite o
// fallan se quedan emulados y en el manifiesto, que tras la migración solo contiene
// esos; se vuelven a intentar en el próximo arranque.
func (efm *EmptyFileManager) migrateToCloud() {
	ms := efm.mfs.MetadataStore()
	if ms == nil || efm.mfs.client == nil {
		return
	}
	files, err := ms.ListAllVirtualEmptyFiles()
	if err != nil {
		logger.Log("[EmptyFiles] ⚠️ No se pudieron listar los vacíos emulados para subirlos: %v", err)
		return
	}
	subidos, yaEstaban, fallidos := 0, 0, 0
	enNube := map[int64]map[string]bool{} // carpeta → nombres que hay en la nube (listado real)
	for _, f := range files {
		clean := path.Clean("/" + f.Path)
		if isEphemeralEmptyFile(clean) || isInternalProxyPath(clean) {
			continue
		}
		switch efm.migrateOne(ms, clean, f.ModTime, enNube) {
		case migratedUploaded:
			subidos++
		case migratedAlreadyReal:
			yaEstaban++
		default:
			fallidos++
		}
	}
	if subidos+yaEstaban+fallidos == 0 {
		return
	}
	logger.Log("[EmptyFiles] ☁️ Vacíos emulados pasados a la nube: %d subidos, %d ya estaban, %d siguen emulados", subidos, yaEstaban, fallidos)
	efm.ScheduleSave() // el manifiesto queda solo con los que siguen emulados
}

type migrateResult int

const (
	migratedFailed migrateResult = iota
	migratedUploaded
	migratedAlreadyReal
)

func (efm *EmptyFileManager) migrateOne(ms *cache.MetadataStore, clean, modTime string, enNube map[int64]map[string]bool) migrateResult {
	unlock := acquireFolderLock(clean) // como una subida: no se cruza con un PUT, MOVE o DELETE
	defer unlock()
	parent, _, isFolder, err := efm.mfs.resolvePathOpts(path.Dir(clean), false)
	if err != nil || !isFolder || parent == nil || parent.ID <= 0 {
		return migratedFailed
	}
	name := path.Base(clean)
	if vf, _ := ms.GetVirtualEmptyFile(parent.ID, name); vf == nil {
		return migratedAlreadyReal // borrado o subido mientras tanto
	}
	defer func() {
		efm.mfs.cacheMu.Lock()
		delete(efm.mfs.uploadCache, cacheKey(clean))
		efm.mfs.cacheMu.Unlock()
		efm.mfs.InvalidateCache(parent.ID)
	}()
	// Se mira el listado real de la carpeta en la nube, no solo la base local: con una
	// base nueva la sincronización inicial puede no haber terminado y se subiría un
	// duplicado. Un listado por carpeta y migración.
	nombres, ok := enNube[parent.ID]
	if !ok {
		items, err := efm.mfs.client.ListAllMediaInFolder(parent.ID)
		if err != nil {
			logger.Log("[EmptyFiles] ⚠️ No se pudo listar la carpeta de '%s' para pasarlo a la nube: %v (sigue emulado)", clean, err)
			return migratedFailed
		}
		nombres = make(map[string]bool, len(items))
		for _, it := range items {
			nombres[it.Name] = true
		}
		enNube[parent.ID] = nombres
	}
	if nombres[name] {
		logMetaErr("DeleteVirtualEmptyFile", ms.DeleteVirtualEmptyFile(parent.ID, name))
		return migratedAlreadyReal
	}
	mod := time.Now()
	if t, err := time.Parse("20060102T150405Z", modTime); err == nil {
		mod = t
	}
	item, err := efm.mfs.client.UploadFileToFolder(name, bytes.NewReader(nil), 0, parent.ID, "", "", mod)
	if err != nil || item == nil {
		logger.Log("[EmptyFiles] ⚠️ No se pudo subir a la nube el vacío emulado '%s': %v (sigue emulado)", clean, err)
		return migratedFailed
	}
	logMetaErr("UpsertMedia", ms.UpsertMedia(*item))
	logMetaErr("DeleteVirtualEmptyFile", ms.DeleteVirtualEmptyFile(parent.ID, name))
	return migratedUploaded
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
