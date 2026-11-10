package webdav

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/webdav"

	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"
)

var (
	uploadSem           = make(chan struct{}, 4) // Hasta 4 subidas simultáneas (óptimo Duplicati)
	downloadSem         = make(chan struct{}, 4) // Hasta 4 descargas simultáneas (óptimo restore)
	globalTransferSem   = make(chan struct{}, 5) // Techo conjunto para no sobrepasar la cuota por token de SAPI
	uploadBufferSem     = make(chan struct{}, 4) // Límite de ingestión PUT local
	folderLocks         sync.Map
	ErrUploadTooLarge   = errors.New("file exceeds maximum allowed upload size")
	ErrUploadIncomplete = errors.New("upload incomplete: connection cut or missing bytes")
)

type uploadGuardKey struct{}

type uploadGuard struct {
	expected int64 // Content-Length anunciado (-1 si no se sabe)
	broken   atomic.Bool
}

type guardedBody struct {
	rc    io.ReadCloser
	guard *uploadGuard
}

func (g *guardedBody) Read(p []byte) (int, error) {
	n, err := g.rc.Read(p)
	if err != nil && err != io.EOF {
		g.guard.broken.Store(true)
	}
	return n, err
}

func (g *guardedBody) Close() error { return g.rc.Close() }

// incomplete dice si la subida está incompleta y por qué.
func (u *uploadGuard) incomplete(size int64) (bool, string) {
	if u == nil {
		return false, ""
	}
	if u.broken.Load() {
		return true, "la conexión se cortó"
	}
	if u.expected >= 0 && size != u.expected {
		return true, fmt.Sprintf("llegaron %d de %d bytes", size, u.expected)
	}
	return false, ""
}

// GetMaxUploadMB devuelve el límite máximo de subida en MB (por defecto 4096 MB).
func GetMaxUploadMB() int64 {
	if env := strings.TrimSpace(os.Getenv("MAX_UPLOAD_MB")); env != "" {
		if val, err := strconv.ParseInt(env, 10, 64); err == nil && val > 0 {
			return val
		}
	}
	return 4096
}

// GetMaxUploadBytes devuelve el límite máximo de subida en bytes.
func GetMaxUploadBytes() int64 {
	return GetMaxUploadMB() * 1024 * 1024
}

// GetUploadRAMBufferMaxBytes devuelve el tamaño máximo de buffer en RAM antes de hacer spool a disco (por defecto 32 MB).
func GetUploadRAMBufferMaxBytes() int {
	if env := strings.TrimSpace(os.Getenv("WEBDAV_BUFFER_MB")); env != "" {
		if val, err := strconv.Atoi(env); err == nil && val > 0 {
			return val * 1024 * 1024
		}
	}
	return 32 * 1024 * 1024 // 32 MB por defecto (cubre el 99% de bloques de Duplicati y restic en RAM sin tocar disco)
}

func getUploadTempDir() string {
	if dir := strings.TrimSpace(os.Getenv("WEBDAV_TMPDIR")); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("TMPDIR")); dir != "" {
		return dir
	}
	return ""
}

// isOSJunk comprueba si el nombre de archivo o carpeta corresponde a metadatos automáticos
// generados por Windows, macOS o Linux al mapear unidades de red o carpetas compartidas.
func isOSJunk(name string) bool {
	base := strings.ToLower(path.Base(name))
	base = strings.TrimSpace(base)

	// Windows: metadatos de carpeta, miniaturas, papelera ($RECYCLE.BIN) y volumen
	switch base {
	case "desktop.ini",
		"autorun.inf",
		"autorun.ini",
		"thumbs.db",
		"ehthumbs.db",
		"ehthumbs_vista.db",
		"$recycle.bin",
		"system volume information":
		return true
	}

	// macOS: configuración de vista, indexación Spotlight, papelera, eventos y AppleShare
	switch base {
	case ".ds_store",
		".spotlight-v100",
		".trashes",
		".fseventsd",
		".temporaryitems",
		".documentrevisions-v100",
		".apdisk",
		".volumeicon.icns":
		return true
	}

	// macOS AppleDouble: bifurcaciones de recursos y atributos extendidos (._*)
	if strings.HasPrefix(base, "._") {
		return true
	}

	// Linux: papelera (.Trash-1000) y configuración de directorios KDE (.directory)
	if strings.HasPrefix(base, ".trash-") || base == ".directory" {
		return true
	}

	return false
}

// isOSJunkPath comprueba si la ruta completa o alguno de sus componentes coincide con archivos o carpetas basura de SO.
func isOSJunkPath(rawPath string) bool {
	clean := path.Clean("/" + rawPath)
	parts := strings.Split(strings.Trim(clean, "/"), "/")
	for _, part := range parts {
		if part != "" && isOSJunk(part) {
			return true
		}
	}
	return false
}

func acquireFolderLock(cleanPath string) func() {
	val, _ := folderLocks.LoadOrStore(cleanPath, &sync.Mutex{})
	mu := val.(*sync.Mutex)
	mu.Lock()
	return func() {
		mu.Unlock()
	}
}

type MemoryFile struct {
	ID        string
	Name      string
	FolderID  int64
	Content   []byte
	Size      int64
	ModTime   time.Time // fecha de modificación que se muestra al cliente
	CachedAt  time.Time // momento en que entró en la caché (caducidad)
	URL       string
	// URLConfirmed indica que URL ya se ha obtenido de SAPI consultando el archivo por su ID.
	URLConfirmed bool
	// URLLookupFailedAt es el momento del último error al consultar la URL (cero si no hubo).
	URLLookupFailedAt time.Time
	MD5       string
}

type CreatedFolder struct {
	Info sapi.FolderItem
	Time time.Time
}

type sfCall struct {
	wg  sync.WaitGroup
	val interface{}
	err error
}

type singleflightGroup struct {
	mu sync.Mutex
	m  map[string]*sfCall
}

func (g *singleflightGroup) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*sfCall)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := new(sfCall)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err
}

type MovistarFS struct {
	client            *sapi.Client
	uploadCache       map[string]*MemoryFile
	createdFolders    map[string]*CreatedFolder
	deletedTombstones map[string]time.Time
	folderCache       map[int64]*cachedFolders
	mediaCache        map[int64]*cachedMedia
	storageCache      *cachedStorage
	cacheMu           sync.RWMutex
	cacheTTL          time.Duration
	duplicatiStore    *DuplicatiProbeStore
	sf                singleflightGroup
}

type cachedFolders struct {
	folders []sapi.FolderItem
	byLower map[string]sapi.FolderItem
	time    time.Time
}

func newCachedFolders(folders []sapi.FolderItem) *cachedFolders {
	byLower := make(map[string]sapi.FolderItem, len(folders))
	for _, f := range folders {
		byLower[strings.ToLower(f.Name)] = f
	}
	return &cachedFolders{
		folders: folders,
		byLower: byLower,
		time:    time.Now(),
	}
}

func (cf *cachedFolders) find(name string) (*sapi.FolderItem, bool) {
	lower := strings.ToLower(name)
	if item, ok := cf.byLower[lower]; ok {
		itemCopy := item
		return &itemCopy, true
	}
	// Tolerancia Synology HyperBackup .hbk
	if strings.HasSuffix(lower, ".hbk") && len(lower) > 4 {
		base := lower[:len(lower)-4]
		if item, ok := cf.byLower[base]; ok {
			itemCopy := item
			return &itemCopy, true
		}
	} else {
		hbk := lower + ".hbk"
		if item, ok := cf.byLower[hbk]; ok {
			itemCopy := item
			return &itemCopy, true
		}
	}
	return nil, false
}

type cachedMedia struct {
	media   []sapi.MediaItem
	byExact map[string]sapi.MediaItem
	byLower map[string]sapi.MediaItem
	time    time.Time
}

func newCachedMedia(media []sapi.MediaItem) *cachedMedia {
	byExact := make(map[string]sapi.MediaItem, len(media))
	byLower := make(map[string]sapi.MediaItem, len(media))
	for _, m := range media {
		byExact[m.Name] = m
		lower := strings.ToLower(m.Name)
		if _, exists := byLower[lower]; !exists {
			byLower[lower] = m
		}
	}
	return &cachedMedia{
		media:   media,
		byExact: byExact,
		byLower: byLower,
		time:    time.Now(),
	}
}

func (cm *cachedMedia) find(name string) (*sapi.MediaItem, bool) {
	if item, ok := cm.byExact[name]; ok {
		itemCopy := item
		return &itemCopy, true
	}
	if item, ok := cm.byLower[strings.ToLower(name)]; ok {
		itemCopy := item
		return &itemCopy, true
	}
	return nil, false
}

type cachedStorage struct {
	space *sapi.StorageSpace
	time  time.Time
}

func NewMovistarFS(client *sapi.Client) *MovistarFS {
	return &MovistarFS{
		client:            client,
		uploadCache:       make(map[string]*MemoryFile),
		createdFolders:    make(map[string]*CreatedFolder),
		deletedTombstones: make(map[string]time.Time),
		folderCache:       make(map[int64]*cachedFolders),
		mediaCache:        make(map[int64]*cachedMedia),
		cacheTTL:          120 * time.Second,
		duplicatiStore:    NewDuplicatiProbeStore(),
	}
}

func (mfs *MovistarFS) getDuplicatiStore() *DuplicatiProbeStore {
	if mfs != nil && mfs.duplicatiStore != nil {
		return mfs.duplicatiStore
	}
	return globalDuplicatiStore
}

func (mfs *MovistarFS) InvalidateCache(folderID int64) {
	mfs.cacheMu.Lock()
	defer mfs.cacheMu.Unlock()
	if folderID != 0 {
		delete(mfs.folderCache, folderID)
		delete(mfs.mediaCache, folderID)
	} else {
		mfs.folderCache = make(map[int64]*cachedFolders)
		mfs.mediaCache = make(map[int64]*cachedMedia)
	}
}

func (mfs *MovistarFS) GetStorageSpace() *sapi.StorageSpace {
	mfs.cacheMu.RLock()
	if mfs.storageCache != nil && time.Since(mfs.storageCache.time) < 5*time.Minute {
		sp := mfs.storageCache.space
		mfs.cacheMu.RUnlock()
		return sp
	}
	mfs.cacheMu.RUnlock()

	val, err := mfs.sf.Do("storage_space", func() (interface{}, error) {
		mfs.cacheMu.RLock()
		if mfs.storageCache != nil && time.Since(mfs.storageCache.time) < 5*time.Minute {
			sp := mfs.storageCache.space
			mfs.cacheMu.RUnlock()
			return sp, nil
		}
		mfs.cacheMu.RUnlock()
		if mfs.client == nil {
			return nil, errors.New("sapi client is nil")
		}
		return mfs.client.GetStorageSpace()
	})

	if err == nil && val != nil {
		sp, _ := val.(*sapi.StorageSpace)
		mfs.cacheMu.Lock()
		mfs.storageCache = &cachedStorage{space: sp, time: time.Now()}
		mfs.cacheMu.Unlock()
		return sp
	}

	// Si falló SAPI (por ejemplo WAF 403 o error de red), reusar la cuota previa si existe y renovar su TTL
	mfs.cacheMu.Lock()
	if mfs.storageCache != nil && mfs.storageCache.space != nil {
		mfs.storageCache.time = time.Now()
		sp := mfs.storageCache.space
		mfs.cacheMu.Unlock()
		return sp
	}

	// Si no hay cuota previa, guardar un fallback durante 5 minutos para evitar martillear SAPI
	fallback := &sapi.StorageSpace{TotalBytes: 0, UsedBytes: 0, NoLimit: true}
	mfs.storageCache = &cachedStorage{space: fallback, time: time.Now()}
	mfs.cacheMu.Unlock()
	return fallback
}

func (mfs *MovistarFS) getSubfolders(parentID int64) ([]sapi.FolderItem, error) {
	mfs.cacheMu.RLock()
	cached, ok := mfs.folderCache[parentID]
	if ok && time.Since(cached.time) < mfs.cacheTTL {
		folders := cached.folders
		mfs.cacheMu.RUnlock()
		return mfs.mergeCreatedFolders(parentID, folders), nil
	}
	mfs.cacheMu.RUnlock()

	key := fmt.Sprintf("subfolders:%d", parentID)
	val, err := mfs.sf.Do(key, func() (interface{}, error) {
		mfs.cacheMu.RLock()
		if c, ok := mfs.folderCache[parentID]; ok && time.Since(c.time) < mfs.cacheTTL {
			folders := c.folders
			mfs.cacheMu.RUnlock()
			return folders, nil
		}
		mfs.cacheMu.RUnlock()
		if mfs.client == nil {
			return nil, errors.New("sapi client is nil")
		}
		return mfs.client.ListAllSubfolders(parentID)
	})

	if err != nil {
		logger.Log("[WebDAV] ⚠️ Error listando subcarpetas de %d: %v", parentID, err)
		mfs.cacheMu.Lock()
		if c, exists := mfs.folderCache[parentID]; exists && c != nil {
			c.time = time.Now()
			folders := c.folders
			mfs.cacheMu.Unlock()
			logger.Log("[WebDAV] ℹ Sirviendo %d subcarpetas en caché previa para carpeta %d", len(folders), parentID)
			return mfs.mergeCreatedFolders(parentID, folders), nil
		}
		mfs.cacheMu.Unlock()
		return nil, err
	}

	subs, _ := val.([]sapi.FolderItem)
	cf := newCachedFolders(subs)
	mfs.cacheMu.Lock()
	mfs.folderCache[parentID] = cf
	mfs.cacheMu.Unlock()

	return mfs.mergeCreatedFolders(parentID, subs), nil
}

func (mfs *MovistarFS) mergeCreatedFolders(parentID int64, folders []sapi.FolderItem) []sapi.FolderItem {
	mfs.cacheMu.Lock()
	defer mfs.cacheMu.Unlock()

	if len(mfs.createdFolders) == 0 {
		return folders
	}

	now := time.Now()
	seen := make(map[string]bool, len(folders))
	for _, f := range folders {
		seen[f.Name] = true
	}

	merged := make([]sapi.FolderItem, len(folders), len(folders)+len(mfs.createdFolders))
	copy(merged, folders)

	for p, cf := range mfs.createdFolders {
		if now.Sub(cf.Time) > 60*time.Second {
			delete(mfs.createdFolders, p)
			continue
		}
		if cf.Info.ParentID == parentID && !seen[cf.Info.Name] {
			merged = append(merged, cf.Info)
			seen[cf.Info.Name] = true
		}
	}
	return merged
}

func (mfs *MovistarFS) findSubfolder(parentID int64, name string) (*sapi.FolderItem, bool, error) {
	mfs.cacheMu.RLock()
	now := time.Now()
	for _, cf := range mfs.createdFolders {
		if now.Sub(cf.Time) <= 60*time.Second && cf.Info.ParentID == parentID {
			if strings.EqualFold(cf.Info.Name, name) || (strings.HasSuffix(strings.ToLower(name), ".hbk") && strings.EqualFold(cf.Info.Name, name[:len(name)-4])) {
				f := cf.Info
				mfs.cacheMu.RUnlock()
				return &f, true, nil
			}
		}
	}

	cached, ok := mfs.folderCache[parentID]
	if ok && time.Since(cached.time) < mfs.cacheTTL {
		item, found := cached.find(name)
		mfs.cacheMu.RUnlock()
		return item, found, nil
	}
	mfs.cacheMu.RUnlock()

	subs, err := mfs.getSubfolders(parentID)
	if err != nil {
		return nil, false, err
	}
	if len(subs) == 0 {
		return nil, false, nil
	}
	mfs.cacheMu.RLock()
	if c, ok := mfs.folderCache[parentID]; ok && c != nil {
		item, found := c.find(name)
		mfs.cacheMu.RUnlock()
		return item, found, nil
	}
	mfs.cacheMu.RUnlock()

	for i := range subs {
		if strings.EqualFold(subs[i].Name, name) || (strings.HasSuffix(strings.ToLower(name), ".hbk") && strings.EqualFold(subs[i].Name, name[:len(name)-4])) {
			return &subs[i], true, nil
		}
	}
	return nil, false, nil
}

func (mfs *MovistarFS) getMedia(folderID int64) ([]sapi.MediaItem, error) {
	mfs.cacheMu.RLock()
	cached, ok := mfs.mediaCache[folderID]
	if ok && time.Since(cached.time) < mfs.cacheTTL {
		items := cached.media
		mfs.cacheMu.RUnlock()
		return items, nil
	}
	mfs.cacheMu.RUnlock()

	key := fmt.Sprintf("media:%d", folderID)
	val, err := mfs.sf.Do(key, func() (interface{}, error) {
		mfs.cacheMu.RLock()
		if c, ok := mfs.mediaCache[folderID]; ok && time.Since(c.time) < mfs.cacheTTL {
			items := c.media
			mfs.cacheMu.RUnlock()
			return items, nil
		}
		mfs.cacheMu.RUnlock()
		if mfs.client == nil {
			return nil, errors.New("sapi client is nil")
		}
		return mfs.client.ListAllMediaInFolder(folderID)
	})

	if err != nil {
		logger.Log("[WebDAV] ⚠️ Error listando media de %d: %v", folderID, err)
		mfs.cacheMu.Lock()
		if c, exists := mfs.mediaCache[folderID]; exists && c != nil {
			c.time = time.Now()
			res := c.media
			mfs.cacheMu.Unlock()
			logger.Log("[WebDAV] ℹ Sirviendo %d elementos en caché previa para carpeta %d", len(res), folderID)
			return res, nil
		}
		mfs.cacheMu.Unlock()
		return nil, err
	}

	items, _ := val.([]sapi.MediaItem)
	cm := newCachedMedia(items)
	mfs.cacheMu.Lock()
	mfs.mediaCache[folderID] = cm
	mfs.cacheMu.Unlock()

	return items, nil
}

func (mfs *MovistarFS) findMedia(folderID int64, name string) (*sapi.MediaItem, bool, error) {
	mfs.cacheMu.RLock()
	cached, ok := mfs.mediaCache[folderID]
	if ok && time.Since(cached.time) < mfs.cacheTTL {
		item, found := cached.find(name)
		mfs.cacheMu.RUnlock()
		return item, found, nil
	}
	mfs.cacheMu.RUnlock()

	items, err := mfs.getMedia(folderID)
	if err != nil {
		return nil, false, err
	}
	if len(items) == 0 {
		return nil, false, nil
	}
	mfs.cacheMu.RLock()
	if c, ok := mfs.mediaCache[folderID]; ok && c != nil {
		item, found := c.find(name)
		mfs.cacheMu.RUnlock()
		return item, found, nil
	}
	mfs.cacheMu.RUnlock()

	for i := range items {
		if strings.EqualFold(items[i].Name, name) {
			return &items[i], true, nil
		}
	}
	return nil, false, nil
}

func (mfs *MovistarFS) resolvePath(rawPath string) (folderInfo *sapi.FolderItem, mediaInfo *sapi.MediaItem, isFolder bool, err error) {
	cleanPath := path.Clean("/" + rawPath)

	rootID, err := mfs.client.GetRootFolderID()
	if err != nil {
		return nil, nil, false, err
	}

	if cleanPath == "/" || cleanPath == "." {
		return &sapi.FolderItem{ID: rootID, Name: "/", Magic: true}, nil, true, nil
	}

	// Archivos y rutas basura de SO (desktop.ini, .DS_Store, etc.)
	if isOSJunkPath(cleanPath) {
		return nil, nil, false, os.ErrNotExist
	}

	// Sondas de prueba de Duplicati en memoria
	if isDuplicatiTestPath(cleanPath) {
		if content, modTime, _, ok := mfs.getDuplicatiStore().Get(cleanPath); ok {
			mi := &sapi.MediaItem{
				ID:               "duplicati-probe",
				Name:             path.Base(cleanPath),
				Size:             int64(len(content)),
				ModificationDate: sapi.FlexDate(modTime.UTC().Format("20060102T150405Z")),
			}
			return nil, mi, false, nil
		}
		return nil, nil, false, os.ErrNotExist
	}

	// Check if path is currently a tombstone (deleted within 5m)
	mfs.cacheMu.Lock()
	if exp, ok := mfs.deletedTombstones[cleanPath]; ok {
		if time.Now().Before(exp) {
			mfs.cacheMu.Unlock()
			return nil, nil, false, os.ErrNotExist
		}
		delete(mfs.deletedTombstones, cleanPath)
	}
	mfs.cacheMu.Unlock()

	// 1. Check createdFolders con verificación de TTL (60s)
	mfs.cacheMu.RLock()
	if cf, ok := mfs.createdFolders[cleanPath]; ok {
		if time.Since(cf.Time) < 60*time.Second {
			info := cf.Info
			mfs.cacheMu.RUnlock()
			return &info, nil, true, nil
		}
	}

	// 2. Check uploadCache
	if mem, ok := mfs.uploadCache[cleanPath]; ok {
		mfs.cacheMu.RUnlock()
		mi := &sapi.MediaItem{
			ID:               mem.ID,
			Name:             mem.Name,
			Size:             mem.Size,
			URL:              mem.URL,
			FolderID:         mem.FolderID,
			ModificationDate: sapi.FlexDate(mem.ModTime.UTC().Format("20060102T150405Z")),
		}
		return nil, mi, false, nil
	}
	mfs.cacheMu.RUnlock()

	parts := strings.Split(strings.Trim(cleanPath, "/"), "/")
	currentID := rootID
	var currentFolder *sapi.FolderItem

	for idx, part := range parts {
		isLast := (idx == len(parts)-1)

		foundSub, ok, err := mfs.findSubfolder(currentID, part)
		if err != nil {
			return nil, nil, false, err
		}
		if ok {
			currentID = foundSub.ID
			currentFolder = foundSub
			if isLast {
				return currentFolder, nil, true, nil
			}
			continue
		}

		// Si no se encontró en subcarpetas y es el último elemento, buscar en archivos
		if isLast {
			foundMedia, okMedia, errMedia := mfs.findMedia(currentID, part)
			if errMedia != nil {
				return nil, nil, false, errMedia
			}
			if okMedia {
				return nil, foundMedia, false, nil
			}
		}

		return nil, nil, false, os.ErrNotExist
	}

	return nil, nil, false, os.ErrNotExist
}

// Implementación webdav.FileSystem

func (mfs *MovistarFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	cleanPath := path.Clean("/" + name)

	// Si es una carpeta de sistema o metadatos de SO ($RECYCLE.BIN, .Trashes, etc.), absorber silenciosamente
	if isOSJunkPath(cleanPath) {
		logger.Log("[WebDAV] 🧹 Ignorada creación de carpeta del sistema: '%s'", cleanPath)
		return nil
	}

	unlock := acquireFolderLock(cleanPath)
	defer unlock()

	// Clear tombstone AND clear any conflicting uploadCache entry if re-creating
	mfs.cacheMu.Lock()
	delete(mfs.deletedTombstones, cleanPath)
	delete(mfs.uploadCache, cleanPath)
	mfs.cacheMu.Unlock()

	parentDir := path.Dir(cleanPath)
	folderName := path.Base(cleanPath)

	parentFolder, _, isFolder, err := mfs.resolvePath(parentDir)
	if err != nil {
		return err
	}
	if !isFolder || parentFolder == nil {
		return os.ErrNotExist
	}

	// Comprobar si ya existe
	subs, err := mfs.getSubfolders(parentFolder.ID)
	if err != nil {
		return err
	}
	for _, s := range subs {
		if strings.EqualFold(s.Name, folderName) {
			logger.Log("[WebDAV] ℹ Carpeta '%s' ya existía en Movistar/O2 Cloud (ID: %d). Reutilizando.", folderName, s.ID)
			mfs.cacheMu.Lock()
			delete(mfs.uploadCache, cleanPath)
			mfs.createdFolders[cleanPath] = &CreatedFolder{Info: s, Time: time.Now()}
			mfs.cacheMu.Unlock()
			return nil
		}
	}

	logger.Log("[WebDAV] 📁 Creando carpeta: '%s' en ID %d...", folderName, parentFolder.ID)
	created, err := mfs.client.CreateFolder(folderName, parentFolder.ID)
	if err != nil {
		logger.Log("[WebDAV] ❌ Error creando carpeta '%s': %v", folderName, err)
		return err
	}

	logger.Log("[WebDAV] ✔ Carpeta creada: '%s' (ID: %d)", folderName, created.ID)
	mfs.cacheMu.Lock()
	delete(mfs.uploadCache, cleanPath)
	mfs.createdFolders[cleanPath] = &CreatedFolder{Info: *created, Time: time.Now()}
	mfs.cacheMu.Unlock()
	mfs.InvalidateCache(parentFolder.ID)

	return nil
}

func (mfs *MovistarFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	cleanPath := path.Clean("/" + name)

	// Absorber escrituras o PROPPATCH sobre archivos de sistema (desktop.ini, .DS_Store, etc.)
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) != 0 && isOSJunkPath(cleanPath) {
		return NewDevNullFileWriter(cleanPath, path.Base(cleanPath)), nil
	}

	// Manejo de escritura (solo cuando se solicita escritura/creación/truncado explícito;
	// PROPPATCH en x/net/webdav abre con os.O_RDWR sin O_CREATE ni O_TRUNC solo para consultar DeadPropsHolder,
	// por lo que NO debe instanciar WebDAVFileWriter ni sobrescribir el archivo con 0 bytes al cerrarse).
	if flag&(os.O_WRONLY|os.O_CREATE|os.O_TRUNC) != 0 {
		fileName := path.Base(cleanPath)

		// Absorber sondas de prueba de Duplicati en memoria sin subir a Telefónica SAPI
		if isDuplicatiTestPath(cleanPath) {
			return NewDuplicatiProbeFileWriter(mfs.getDuplicatiStore(), cleanPath, fileName), nil
		}

		parentDir := path.Dir(cleanPath)

		parentFolder, _, isFolder, err := mfs.resolvePath(parentDir)
		if err != nil {
			return nil, err
		}
		if !isFolder || parentFolder == nil {
			return nil, os.ErrNotExist
		}

		// En WebDAV RFC 4918 (Sección 9.7.1), un PUT sobre una colección existente
		// DEBE ser rechazado con 405 Method Not Allowed (retornando os.ErrInvalid).
		// Esto es crítico para Windows Explorer: al arrastrar carpetas, hace MKCOL y luego un PUT de 0 bytes;
		// si se acepta el PUT, crea un archivo plano en lugar de mantener la carpeta, rompiendo la copia de sus hijos.
		existingFolder, mediaInfo, isF, err := mfs.resolvePath(cleanPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil && isF && existingFolder != nil {
			logger.Log("[WebDAV] ℹ️ Rechazado PUT sobre carpeta existente: '%s' (RFC 4918 405 Method Not Allowed)", cleanPath)
			return nil, os.ErrInvalid
		}

		// Chequear si ya existe un archivo con ese nombre para borrarlo antes y no crear sufijo ' (1)'
		var existingID string
		if err == nil && !isF && mediaInfo != nil {
			existingID = mediaInfo.ID
		}

		fw := NewWebDAVFileWriter(mfs, cleanPath, fileName, parentFolder.ID, existingID)
		fw.modTime = ocMtimeFromContext(ctx)
		fw.guard, _ = ctx.Value(uploadGuardKey{}).(*uploadGuard)
		return fw, nil
	}

	// Modo lectura
	if isDuplicatiTestPath(cleanPath) {
		content, modTime, _, ok := mfs.getDuplicatiStore().Get(cleanPath)
		if !ok {
			return nil, os.ErrNotExist
		}
		return NewWebDAVMemoryReader(cleanPath, &MemoryFile{
			Name:     path.Base(cleanPath),
			Content:  content,
			Size:     int64(len(content)),
			ModTime:  modTime,
			CachedAt: time.Now(),
		}), nil
	}

	folderInfo, mediaInfo, isFolder, err := mfs.resolvePath(cleanPath)
	if err != nil {
		return nil, err
	}

	if isFolder {
		return NewWebDAVDirReader(mfs, cleanPath, folderInfo.ID), nil
	}

	// Comprobar memoria primero
	mfs.cacheMu.RLock()
	if mem, ok := mfs.uploadCache[cleanPath]; ok {
		mfs.cacheMu.RUnlock()
		if mem.Size == 0 || len(mem.Content) > 0 {
			return NewWebDAVMemoryReader(cleanPath, mem), nil
		}
		item := sapi.MediaItem{
			ID:               mem.ID,
			Name:             mem.Name,
			Size:             mem.Size,
			URL:              mem.downloadURL(mfs),
			FolderID:         mem.FolderID,
			ModificationDate: sapi.FlexDate(mem.ModTime.UTC().Format("20060102T150405Z")),
		}
		return NewWebDAVFileReader(mfs, cleanPath, item), nil
	}
	mfs.cacheMu.RUnlock()

	return NewWebDAVFileReader(mfs, cleanPath, *mediaInfo), nil
}

// urlLookupErrorBackoff es el tiempo durante el que no se vuelve a consultar la URL de
// un archivo tras un error de SAPI, para no insistir contra un servicio que está fallando.
var urlLookupErrorBackoff = 10 * time.Second

// downloadURL devuelve la URL de descarga de un archivo recién subido.
//
// La respuesta de subida de SAPI no siempre incluye la URL de descarga; en ese caso
// el cliente construye una por defecto que el servidor no sirve, y las lecturas
// inmediatas tras subir (p. ej. "restic check" justo después de un backup) devuelven
// un cuerpo vacío durante toda la vida de la caché de subidas.
//
// Se consulta a SAPI ese archivo por su ID (una sola petición, sin esperas):
//   - si devuelve la URL, se usa y se recuerda para las lecturas siguientes;
//   - si aún no lo conoce (indexándose), se usa la URL conocida y se volverá a
//     consultar en la siguiente lectura;
//   - si la consulta falla, se usa la URL conocida, se registra el error y no se
//     reintenta para ese archivo hasta pasado urlLookupErrorBackoff.
//
// En ningún caso es peor que el comportamiento anterior (usar siempre la URL conocida).
func (mem *MemoryFile) downloadURL(mfs *MovistarFS) string {
	mfs.cacheMu.RLock()
	known, confirmed, failedAt := mem.URL, mem.URLConfirmed, mem.URLLookupFailedAt
	mfs.cacheMu.RUnlock()
	if confirmed || mem.ID == "" {
		return known
	}
	if !failedAt.IsZero() && time.Since(failedAt) < urlLookupErrorBackoff {
		return known
	}

	item, err := mfs.client.GetMediaByID(mem.ID)
	switch {
	case err != nil:
		logger.Log("[WebDAV] ⚠️ No se pudo consultar la URL de descarga de '%s' (ID: %s): %v. Se usa la conocida; nuevo intento en %s.", mem.Name, mem.ID, err, urlLookupErrorBackoff)
		mfs.cacheMu.Lock()
		mem.URLLookupFailedAt = time.Now()
		mfs.cacheMu.Unlock()
		return known
	case item == nil || item.URL == "":
		return known
	default:
		mfs.cacheMu.Lock()
		mem.URL = item.URL
		mem.URLConfirmed = true
		mem.URLLookupFailedAt = time.Time{}
		mfs.cacheMu.Unlock()
		return item.URL
	}
}

func (mfs *MovistarFS) RemoveAll(ctx context.Context, name string) error {
	cleanPath := path.Clean("/" + name)
	if cleanPath == "/" || cleanPath == "." {
		return fs.ErrPermission
	}
	if isOSJunkPath(cleanPath) {
		return nil
	}
	if isDuplicatiTestPath(cleanPath) {
		mfs.getDuplicatiStore().Delete(cleanPath)
		return nil
	}

	// Lock por path: contiende con un MOVE/PUT en curso sobre el mismo fichero.
	unlock := acquireFolderLock(cleanPath)
	defer unlock()

	folderInfo, mediaInfo, isFolder, err := mfs.resolvePath(cleanPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // WebDAV idempotencia
		}
		return err
	}

	if isFolder && folderInfo != nil {
		logger.Log("[WebDAV] 🗑 Eliminando carpeta: '%s' (ID: %d)...", folderInfo.Name, folderInfo.ID)
		if err := mfs.client.DeleteFolder(folderInfo.ID, false); err != nil {
			logger.Log("[WebDAV] ❌ Error eliminando carpeta '%s' (ID: %d): %v", folderInfo.Name, folderInfo.ID, err)
			return err
		}
		mfs.cacheMu.Lock()
		delete(mfs.createdFolders, cleanPath)
		delete(mfs.uploadCache, cleanPath)
		mfs.deletedTombstones[cleanPath] = time.Now().Add(5 * time.Minute)
		mfs.cacheMu.Unlock()
		mfs.InvalidateCache(0)
		return nil
	}

	if !isFolder && mediaInfo != nil {
		logger.Log("[WebDAV] 🗑 Eliminando archivo: '%s' (ID: %s)...", mediaInfo.Name, mediaInfo.ID)
		if mediaInfo.ID != "" {
			if err := mfs.client.DeleteFile(mediaInfo.ID); err != nil {
				logger.Log("[WebDAV] ❌ Error eliminando archivo '%s' (ID: %s): %v", mediaInfo.Name, mediaInfo.ID, err)
				return err
			}
		}
		mfs.cacheMu.Lock()
		delete(mfs.uploadCache, cleanPath)
		mfs.deletedTombstones[cleanPath] = time.Now().Add(5 * time.Minute)
		mfs.cacheMu.Unlock()
		mfs.InvalidateCache(mediaInfo.FolderID)
		return nil
	}

	return nil
}

func (mfs *MovistarFS) Rename(ctx context.Context, oldName, newName string) error {
	oldClean := path.Clean("/" + oldName)
	newClean := path.Clean("/" + newName)

	if oldClean == "/" || oldClean == "." || newClean == "/" || newClean == "." {
		return fs.ErrPermission
	}

	if isOSJunkPath(oldClean) && isOSJunkPath(newClean) {
		return nil
	}
	// Si se mueve un archivo real hacia una papelera de SO (.Trashes, .Trash-1000, $RECYCLE.BIN),
	// ejecutar borrado real del archivo origen para que macOS Finder / Linux lo eliminen de la nube.
	if !isOSJunkPath(oldClean) && isOSJunkPath(newClean) {
		return mfs.RemoveAll(ctx, oldClean)
	}
	if isOSJunkPath(oldClean) && !isOSJunkPath(newClean) {
		return nil
	}

	// Lock por path (dentro de este proceso) para serializar MOVE/PUT/DELETE del
	// mismo fichero y evitar que otro proceso borre o sobrescriba el origen a mitad
	// del move. Se toman origen y destino en orden total (orden alfabético) para no
	// generar interbloqueos entre dos renombrados cruzados.
	// Nota: solo protege dentro de una instancia del proxy, no entre varias.
	lkA, lkB := oldClean, newClean
	if lkA > lkB {
		lkA, lkB = lkB, lkA
	}
	unlockA := acquireFolderLock(lkA)
	defer unlockA()
	if lkB != lkA {
		unlockB := acquireFolderLock(lkB)
		defer unlockB()
	}

	newParentDir := path.Dir(newClean)
	newBaseName := path.Base(newClean)

	parentFolder, _, isFolder, err := mfs.resolvePath(newParentDir)
	if err != nil {
		return err
	}
	if !isFolder || parentFolder == nil {
		return os.ErrNotExist
	}

	oldFolder, oldMedia, isF, err := mfs.resolvePath(oldClean)
	if err != nil {
		return err
	}

	logger.Log("[WebDAV] 🔄 Renombrando: '%s' -> '%s'...", oldClean, newClean)

	if isF && oldFolder != nil {
		err := mfs.client.UpdateFolder(oldFolder.ID, newBaseName, parentFolder.ID)
		mfs.cacheMu.Lock()
		delete(mfs.createdFolders, oldClean)
		delete(mfs.deletedTombstones, newClean)
		mfs.cacheMu.Unlock()
		mfs.InvalidateCache(0)
		return err
	}

	if !isF && oldMedia != nil {
		// Comprobar si el destino ya existe en Movistar Cloud para borrarlo antes y evitar sufijo ' (1)'
		_, destMedia, destIsFolder, destErr := mfs.resolvePath(newClean)
		if destErr == nil && !destIsFolder && destMedia != nil && destMedia.ID != "" {
			logger.Log("[WebDAV] ℹ Archivo destino '%s' ya existe (ID: %s). Eliminándolo antes de MOVE...", newClean, destMedia.ID)
			_ = mfs.client.DeleteFile(destMedia.ID)
		}

		var reuploaded *MemoryFile
		if oldMedia.ID != "" {
			oldExt := strings.ToLower(path.Ext(oldMedia.Name))
			newExt := strings.ToLower(path.Ext(newBaseName))
			if oldExt != newExt {
				// SAPI no admite cambiar de extensión vía save-metadata (MED-1050 en multimedia y conserva extensión original en genéricos).
				// Se resuelve descargando y volviendo a subir con el nuevo nombre (moveByReupload), igual que rclone.
				src := *oldMedia
				modTime := oldMedia.ModificationDate.Time()
				var content []byte
				mfs.cacheMu.RLock()
				mem, inCache := mfs.uploadCache[oldClean]
				mfs.cacheMu.RUnlock()
				if inCache {
					modTime = mem.ModTime
					if mem.Size == 0 || len(mem.Content) > 0 {
						// Recién subido y pequeño: se re-sube desde la copia en memoria.
						content = mem.Content
						if content == nil {
							content = []byte{}
						}
					} else {
						// Recién subido: SAPI tarda unos segundos en servirlo y la URL de
						// la respuesta de subida no siempre sirve el archivo.
						src.URL = mfs.waitForDownloadURL(mem)
					}
				}
				var err error
				if reuploaded, err = mfs.moveByReupload(ctx, &src, content, newBaseName, parentFolder.ID, modTime); err != nil {
					return err
				}
			} else {
				if err := mfs.client.UpdateMedia(oldMedia.ID, oldMedia.Name, newBaseName, parentFolder.ID); err != nil {
					return err
				}
			}
		}
		mfs.cacheMu.Lock()
		delete(mfs.deletedTombstones, newClean)
		if reuploaded != nil {
			// El archivo nuevo tiene otro ID y SAPI tarda en listarlo: se registra como
			// una subida recién hecha para que se vea y se lea en seguida.
			delete(mfs.uploadCache, oldClean)
			mfs.uploadCache[newClean] = reuploaded
		} else if mem, ok := mfs.uploadCache[oldClean]; ok {
			delete(mfs.uploadCache, oldClean)
			mem.Name = newBaseName
			mem.FolderID = parentFolder.ID
			mfs.uploadCache[newClean] = mem
		}
		mfs.cacheMu.Unlock()
		mfs.InvalidateCache(0)
		return nil
	}

	return nil
}

// Espera máxima a que SAPI sirva un archivo recién subido antes de re-subirlo
// (en O2 Cloud se ha medido del orden de 10 s tras la subida).
var (
	reuploadSourceWait = 45 * time.Second
	reuploadSourcePoll = 2 * time.Second
)

// waitForDownloadURL consulta por ID la URL de descarga de un archivo recién subido
// hasta que SAPI la confirma o se agota reuploadSourceWait; en ese caso devuelve la
// URL conocida y la descarga posterior informará del error.
func (mfs *MovistarFS) waitForDownloadURL(mem *MemoryFile) string {
	deadline := time.Now().Add(reuploadSourceWait)
	for {
		u := mem.downloadURL(mfs)
		mfs.cacheMu.RLock()
		confirmed := mem.URLConfirmed
		mfs.cacheMu.RUnlock()
		if confirmed || mem.ID == "" || time.Now().After(deadline) {
			return u
		}
		time.Sleep(reuploadSourcePoll)
	}
}

// moveByReupload sube el archivo con el nombre nuevo y, solo si la subida se completa,
// borra el original. Si content no es nil se sube desde ahí en vez de descargarlo.
func (mfs *MovistarFS) moveByReupload(ctx context.Context, oldMedia *sapi.MediaItem, content []byte, newName string, newFolderID int64, modTime time.Time) (*MemoryFile, error) {
	logger.Log("[WebDAV] 🔄 Re-subiendo '%s' -> '%s' (ID: %s) por cambio de extensión...", oldMedia.Name, newName, oldMedia.ID)

	var reader io.ReadSeeker
	var cachedContent []byte
	var n int64

	// Se calcula el sha256 de los bytes que se van a subir (sea desde la copia en
	// memoria o según se descargan), para después verificar que el destino quedó con
	// exactamente ese contenido antes de borrar el original.
	hasher := sha256.New()

	if content != nil {
		n = int64(len(content))
		_, _ = hasher.Write(content)
		reader = bytes.NewReader(content)
		cachedContent = content
	} else {
		stream, err := mfs.client.DownloadFileStream(*oldMedia)
		if err != nil {
			return nil, fmt.Errorf("moveByReupload download error: %w", err)
		}
		defer stream.Close()

		if oldMedia.Size <= int64(GetUploadRAMBufferMaxBytes()) {
			buf := bytes.NewBuffer(make([]byte, 0, oldMedia.Size))
			if n, err = io.Copy(io.MultiWriter(buf, hasher), stream); err != nil {
				return nil, fmt.Errorf("moveByReupload buffer error: %w", err)
			}
			reader = bytes.NewReader(buf.Bytes())
			if oldMedia.Size <= 256*1024 {
				cachedContent = buf.Bytes()
			}
		} else {
			tmp, err := os.CreateTemp(getUploadTempDir(), "webdav_reupload_*")
			if err != nil {
				return nil, fmt.Errorf("moveByReupload temp file error: %w", err)
			}
			tmpPath := tmp.Name()
			defer func() {
				_ = tmp.Close()
				_ = os.Remove(tmpPath)
			}()

			if n, err = io.Copy(io.MultiWriter(tmp, hasher), stream); err != nil {
				return nil, fmt.Errorf("moveByReupload spool error: %w", err)
			}
			if _, err := tmp.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("moveByReupload seek error: %w", err)
			}
			reader = tmp
		}
	}

	// Garantía de integridad: solo se re-sube y se borra el original si tenemos
	// exactamente los bytes esperados. Una descarga corta terminada en EOF limpio
	// (menos bytes de los anunciados) no debe provocar la pérdida del original.
	if n != oldMedia.Size {
		return nil, fmt.Errorf("moveByReupload: '%s' incompleto (%d de %d bytes); no se modifica el origen", oldMedia.Name, n, oldMedia.Size)
	}

	srcHash := hex.EncodeToString(hasher.Sum(nil))
	// En la ruta de descarga (content == nil) el origen estaba en la nube: se releerá
	// el destino entero y se comparará por hash. En la ruta de contenido tenemos los
	// bytes exactos en memoria, así que se verifica el destino por tamaño (sin forzar
	// una descarga) y el contenido queda respaldado por la copia en RAM.
	return mfs.finishReupload(oldMedia, reader, cachedContent, newName, newFolderID, modTime, srcHash, content == nil)
}

func (mfs *MovistarFS) finishReupload(oldMedia *sapi.MediaItem, reader io.ReadSeeker, cachedContent []byte, newName string, newFolderID int64, modTime time.Time, srcHash string, verifyContent bool) (*MemoryFile, error) {
	newItem, err := mfs.client.UploadFileToFolder(newName, reader, oldMedia.Size, newFolderID, "", "", modTime)
	if err != nil {
		return nil, fmt.Errorf("moveByReupload upload error: %w", err)
	}

	// No fiarse del acuse de la subida: el ID nuevo debe existir y ser distinto del
	// original (si no, borraríamos el propio origen).
	if newItem.ID == "" || newItem.ID == oldMedia.ID {
		return nil, fmt.Errorf("moveByReupload: la subida de '%s' no devolvió un ID nuevo (%q); no se borra el origen", newName, newItem.ID)
	}

	// Antes de borrar el original se confirma que el destino quedó realmente
	// persistido: tamaño de metadato correcto, contenido recuperable entero, y con
	// el mismo sha256 que se subió. Un destino que aún no se sirve (lectura vacía con
	// EOF limpio) no es prueba de fallo: se reintenta hasta reuploadSourceWait.
	confirmedURL := newItem.URL
	confirmed := false
	deadline := time.Now().Add(reuploadSourceWait)
	for {
		dst, qErr := mfs.client.GetMediaByID(newItem.ID)
		if qErr == nil && dst != nil {
			if dst.Size != oldMedia.Size {
				return nil, fmt.Errorf("moveByReupload: destino '%s' con tamaño %d, se esperaban %d bytes; no se borra el origen", newName, dst.Size, oldMedia.Size)
			}
			if dst.URL != "" {
				confirmedURL = dst.URL
			}
			if !verifyContent || oldMedia.Size == 0 {
				// Ruta de contenido (bytes conocidos en RAM) o fichero vacío: el tamaño
				// del metadato confirma el destino; no se fuerza una descarga de relectura
				// (el contenido ya lo garantizan los bytes exactos que tenemos en memoria).
				//
				// AVISO (coste medido contra O2 real): confirmar el destino por metadato
				// añade la espera a que SAPI indexe el recién subido, así que un renombrado
				// pequeño con cambio de extensión pasa de ~1 s a unos segundos. Se elige a
				// propósito por seguridad (no borrar el origen sin confirmar el destino). Si
				// esa latencia molesta en cargas con muchos renombrados pequeños (p. ej.
				// Duplicati), aquí es donde se podría relajar a confiar solo en la copia en
				// RAM. No se ha hecho: el proyecto prioriza no perder datos.
				//
				// Nota: el etag de O2 NO sirve para verificar sin descargar; es un resumen
				// de 16 bytes sobre el objeto almacenado (cifrado), no el hash del contenido
				// original (comprobado contra la cuenta real), y no hay otro checksum del
				// contenido en el SAPI.
				confirmed = true
				break
			}
			vItem := sapi.MediaItem{ID: newItem.ID, Name: newName, Size: oldMedia.Size, URL: confirmedURL, FolderID: newFolderID}
			vstream, vErr := mfs.client.DownloadFileStream(vItem)
			if vErr != nil {
				return nil, fmt.Errorf("moveByReupload: no se pudo leer el destino '%s' para verificarlo: %w; no se borra el origen", newName, vErr)
			}
			h := sha256.New()
			willCache := oldMedia.Size <= int64(GetUploadRAMBufferMaxBytes())
			var vbuf *bytes.Buffer
			var vdst io.Writer = h
			if willCache {
				vbuf = bytes.NewBuffer(make([]byte, 0, oldMedia.Size))
				vdst = io.MultiWriter(vbuf, h)
			}
			vn, cpErr := io.Copy(vdst, vstream)
			_ = vstream.Close()
			if cpErr != nil {
				return nil, fmt.Errorf("moveByReupload: lectura del destino '%s' incompleta: %w; no se borra el origen", newName, cpErr)
			}
			if vn == oldMedia.Size {
				if srcHash != "" && hex.EncodeToString(h.Sum(nil)) != srcHash {
					return nil, fmt.Errorf("moveByReupload: el contenido del destino '%s' no coincide con el origen (sha256); no se borra el origen", newName)
				}
				if willCache {
					cachedContent = vbuf.Bytes()
				}
				confirmed = true
				break
			}
			// vn != size con EOF limpio: el destino existe pero aún no se sirve entero,
			// como cualquier subida reciente. No es prueba de fallo: se reintenta.
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("moveByReupload: no se pudo confirmar el destino '%s' a tiempo; no se borra el origen", newName)
		}
		time.Sleep(reuploadSourcePoll)
	}

	// Confirmado: ya es seguro borrar el original.
	if err := mfs.client.DeleteFile(oldMedia.ID); err != nil {
		logger.Log("[WebDAV] ⚠️ Advertencia: No se pudo eliminar el archivo original '%s' tras re-subida: %v", oldMedia.Name, err)
	}

	logger.Log("[WebDAV] ✔ Re-subida completada y verificada: '%s' -> '%s' (Nuevo ID: %s)", oldMedia.Name, newName, newItem.ID)
	now := time.Now()
	return &MemoryFile{
		ID:           newItem.ID,
		Name:         newName,
		FolderID:     newFolderID,
		Content:      cachedContent,
		Size:         oldMedia.Size,
		ModTime:      modTime,
		CachedAt:     now,
		URL:          confirmedURL,
		URLConfirmed: confirmed,
	}, nil
}

func (mfs *MovistarFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	cleanPath := path.Clean("/" + name)
	if isDuplicatiTestPath(cleanPath) {
		content, modTime, _, ok := mfs.getDuplicatiStore().Get(cleanPath)
		if !ok {
			return nil, os.ErrNotExist
		}
		return &VirtualFileInfo{
			name:    path.Base(cleanPath),
			size:    int64(len(content)),
			mode:    0644,
			modTime: modTime,
			isDir:   false,
		}, nil
	}

	_, mediaInfo, isFolder, err := mfs.resolvePath(cleanPath)
	if err != nil {
		return nil, err
	}

	if isFolder {
		return &VirtualFileInfo{
			name:    path.Base(cleanPath),
			size:    0,
			mode:    os.ModeDir | 0755,
			modTime: time.Now(),
			isDir:   true,
		}, nil
	}

	mfs.cacheMu.RLock()
	if mem, ok := mfs.uploadCache[cleanPath]; ok {
		mfs.cacheMu.RUnlock()
		return &VirtualFileInfo{
			name:    mem.Name,
			size:    mem.Size,
			mode:    0644,
			modTime: mem.ModTime,
			isDir:   false,
		}, nil
	}
	mfs.cacheMu.RUnlock()

	return &VirtualFileInfo{
		name:    mediaInfo.Name,
		size:    mediaInfo.Size,
		mode:    0644,
		modTime: parseTime(mediaInfo.ModificationDate),
		isDir:   false,
	}, nil
}

func parseTime(fd sapi.FlexDate) time.Time {
	return fd.Time()
}

// Tipos auxiliares de File y FileInfo

type VirtualFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	isDir   bool
}

func (v *VirtualFileInfo) Name() string       { return v.name }
func (v *VirtualFileInfo) Size() int64        { return v.size }
func (v *VirtualFileInfo) Mode() os.FileMode  { return v.mode }
func (v *VirtualFileInfo) ModTime() time.Time { return v.modTime }
func (v *VirtualFileInfo) IsDir() bool        { return v.isDir }
func (v *VirtualFileInfo) Sys() interface{}   { return nil }

func (v *VirtualFileInfo) ContentType(ctx context.Context) (string, error) {
	if v.isDir {
		return "httpd/unix-directory", nil
	}
	ext := filepath.Ext(v.name)
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct, nil
	}
	return "application/octet-stream", nil
}

type streamUploadResult struct {
	media *sapi.MediaItem
	err   error
}

type WebDAVFileWriter struct {
	mfs            *MovistarFS
	path           string
	name           string
	parentFolderID int64
	existingID     string
	buf            *bytes.Buffer
	tmpFile        *os.File
	size           int64
	closed         bool
	semAcquired    bool
	failed         bool
	modTime        time.Time // fecha pedida por el cliente (X-OC-Mtime); cero = ahora
	guard          *uploadGuard
	pathUnlock     func()
	streamPW       *io.PipeWriter
	streamDone     chan streamUploadResult
	streamFailed   bool
}

func NewWebDAVFileWriter(mfs *MovistarFS, path, name string, parentFolderID int64, existingID string) *WebDAVFileWriter {
	return &WebDAVFileWriter{
		mfs:            mfs,
		path:           path,
		name:           name,
		parentFolderID: parentFolderID,
		existingID:     existingID,
		buf:            bytes.NewBuffer(nil),
	}
}

func (w *WebDAVFileWriter) ensurePathLock() {
	if w.pathUnlock == nil {
		w.pathUnlock = acquireFolderLock(w.path)
	}
}

func (w *WebDAVFileWriter) releasePathLock() {
	if w.pathUnlock != nil {
		unlock := w.pathUnlock
		w.pathUnlock = nil
		unlock()
	}
}

func (w *WebDAVFileWriter) startStreamUpload(initialBytes []byte) {
	if w.guard == nil || w.guard.expected <= 0 || w.streamDone != nil {
		return
	}
	w.ensurePathLock()
	pr, pw := io.Pipe()
	w.streamPW = pw
	w.streamDone = make(chan streamUploadResult, 1)
	expectedSize := w.guard.expected
	sizeMB := float64(expectedSize) / (1024 * 1024)
	logger.Log("[WebDAV] ⬆ Subiendo en flujo directo: '%s' (%.2f MB) a Movistar/O2 Cloud...", w.name, sizeMB)
	mimeType := mime.TypeByExtension(filepath.Ext(w.name))

	go func() {
		uploadSem <- struct{}{}
		globalTransferSem <- struct{}{}
		defer func() {
			<-globalTransferSem
			<-uploadSem
		}()
		media, err := w.mfs.client.UploadFileToFolder(w.name, pr, expectedSize, w.parentFolderID, mimeType, w.existingID, w.modTime)
		_ = pr.Close()
		w.streamDone <- streamUploadResult{media: media, err: err}
	}()

	if len(initialBytes) > 0 {
		if _, err := w.streamPW.Write(initialBytes); err != nil {
			w.streamFailed = true
			_ = w.streamPW.CloseWithError(err)
		}
	}
}

func (w *WebDAVFileWriter) abortStream(err error) {
	if w.streamPW != nil {
		_ = w.streamPW.CloseWithError(err)
		w.streamPW = nil
	}
	if w.streamDone != nil {
		<-w.streamDone
		w.streamDone = nil
	}
	w.releasePathLock()
}

func (w *WebDAVFileWriter) releaseBufferSem() {
	if w.semAcquired {
		w.semAcquired = false
		<-uploadBufferSem
	}
}

func (w *WebDAVFileWriter) cleanupTemp() {
	if w.tmpFile != nil {
		name := w.tmpFile.Name()
		_ = w.tmpFile.Close()
		_ = os.Remove(name)
		w.tmpFile = nil
	}
}

func (w *WebDAVFileWriter) Write(p []byte) (n int, err error) {
	if w.failed {
		return 0, ErrUploadTooLarge
	}
	if !w.semAcquired {
		uploadBufferSem <- struct{}{}
		w.semAcquired = true
	}

	maxBytes := GetMaxUploadBytes()
	if maxBytes > 0 && w.size+int64(len(p)) > maxBytes {
		w.failed = true
		w.abortStream(ErrUploadTooLarge)
		w.cleanupTemp()
		w.releaseBufferSem()
		logger.Log("[WebDAV] ❌ Subida abortada por superar límite de %d MB: '%s'", GetMaxUploadMB(), w.name)
		return 0, ErrUploadTooLarge
	}

	rambufMax := GetUploadRAMBufferMaxBytes()
	if w.tmpFile == nil && (w.buf.Len()+len(p) > rambufMax || (w.guard != nil && w.guard.expected > int64(rambufMax))) {
		tmpDir := getUploadTempDir()
		tmp, err := os.CreateTemp(tmpDir, "webdav_upload_*")
		if err != nil {
			w.failed = true
			w.releaseBufferSem()
			logger.Log("[WebDAV] ❌ No se pudo crear archivo temporal en '%s': %v (verifica permisos o espacio en disco)", tmpDir, err)
			return 0, err
		}
		w.tmpFile = tmp
		var initialBytes []byte
		if w.buf != nil && w.buf.Len() > 0 {
			initialBytes = w.buf.Bytes()
			if _, err := tmp.Write(initialBytes); err != nil {
				w.failed = true
				w.cleanupTemp()
				w.releaseBufferSem()
				return 0, err
			}
		}
		w.buf = nil
		w.startStreamUpload(initialBytes)
	}

	w.size += int64(len(p))
	if w.tmpFile != nil {
		n, err = w.tmpFile.Write(p)
		if err != nil {
			w.failed = true
			w.abortStream(err)
			w.cleanupTemp()
			w.releaseBufferSem()
			return n, err
		}
		if w.streamPW != nil && !w.streamFailed {
			if _, pwErr := w.streamPW.Write(p[:n]); pwErr != nil {
				w.streamFailed = true
				_ = w.streamPW.CloseWithError(pwErr)
			}
		}
		return n, nil
	}
	n, err = w.buf.Write(p)
	if err != nil {
		w.failed = true
	}
	return n, err
}

func (w *WebDAVFileWriter) MarkUploadExceeded() {
	w.failed = true
	w.abortStream(ErrUploadTooLarge)
	w.cleanupTemp()
	w.releaseBufferSem()
}

func (w *WebDAVFileWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	defer w.releaseBufferSem()
	defer w.releasePathLock()

	if w.failed {
		w.abortStream(ErrUploadTooLarge)
		w.cleanupTemp()
		return ErrUploadTooLarge
	}

	// Si el cuerpo no llegó entero, no subir el trozo (la copia anterior queda intacta).
	if bad, why := w.guard.incomplete(w.size); bad {
		w.abortStream(ErrUploadIncomplete)
		w.cleanupTemp()
		logger.Log("[WebDAV] ❌ Subida incompleta, NO se sube a la nube (%s): '%s'", why, w.name)
		return ErrUploadIncomplete
	}

	if w.tmpFile == nil && w.buf == nil {
		w.abortStream(errors.New("upload buffer corrupted"))
		w.cleanupTemp()
		return errors.New("upload buffer corrupted or closed after failure")
	}

	// Lock por path: contiende con un MOVE/DELETE en curso sobre el mismo fichero,
	// de modo que la subida y la actualización de caché no se crucen con ellos.
	w.ensurePathLock()

	sizeMB := float64(w.size) / (1024 * 1024)
	var media *sapi.MediaItem
	var err error
	var cachedContent []byte

	if w.streamDone != nil {
		if w.streamPW != nil {
			_ = w.streamPW.Close()
			w.streamPW = nil
		}
		res := <-w.streamDone
		w.streamDone = nil
		if res.err == nil && res.media != nil {
			media = res.media
			w.cleanupTemp()
		} else {
			logger.Log("[WebDAV] ⚠️ Subida en flujo requirió reintento desde disco para '%s': %v", w.name, res.err)
		}
	}

	if media == nil {
		logger.Log("[WebDAV] ⬆ Subiendo: '%s' (%.2f MB) a Movistar/O2 Cloud...", w.name, sizeMB)

		uploadSem <- struct{}{}
		globalTransferSem <- struct{}{}

		var reader io.Reader
		if w.tmpFile != nil {
			defer w.cleanupTemp()
			_, _ = w.tmpFile.Seek(0, io.SeekStart)
			reader = w.tmpFile
		} else {
			bufBytes := w.buf.Bytes()
			reader = bytes.NewReader(bufBytes)
			if w.size <= 256*1024 {
				cachedContent = bufBytes
			}
		}

		mimeType := mime.TypeByExtension(filepath.Ext(w.name))
		media, err = w.mfs.client.UploadFileToFolder(w.name, reader, w.size, w.parentFolderID, mimeType, w.existingID, w.modTime)
		<-globalTransferSem
		<-uploadSem
		if err != nil {
			logger.Log("[WebDAV] ❌ Error al subir '%s': %v", w.name, err)
			return err
		}
	}

	logger.Log("[WebDAV] ✔ Subida completada: '%s' (%.2f MB, ID: %s)", w.name, sizeMB, media.ID)

	w.mfs.cacheMu.Lock()
	now := time.Now()
	modTime := now
	if !w.modTime.IsZero() {
		modTime = w.modTime
	}
	delete(w.mfs.deletedTombstones, w.path)
	w.mfs.uploadCache[w.path] = &MemoryFile{
		ID:       media.ID,
		Name:     w.name,
		FolderID: w.parentFolderID,
		Content:  cachedContent,
		Size:     w.size,
		ModTime:  modTime,
		CachedAt: now,
		URL:      media.URL,
	}

	// Limpieza de entradas expiradas para prevenir fugas de memoria
	for p, mem := range w.mfs.uploadCache {
		if now.Sub(mem.CachedAt) > 15*time.Minute {
			delete(w.mfs.uploadCache, p)
		}
	}
	for p, cf := range w.mfs.createdFolders {
		if now.Sub(cf.Time) > 5*time.Minute {
			delete(w.mfs.createdFolders, p)
		}
	}
	for p, exp := range w.mfs.deletedTombstones {
		if now.After(exp) {
			delete(w.mfs.deletedTombstones, p)
		}
	}
	w.mfs.cacheMu.Unlock()
	w.mfs.InvalidateCache(w.parentFolderID)

	return nil
}

func (w *WebDAVFileWriter) Read(p []byte) (n int, err error) { return 0, io.EOF }
func (w *WebDAVFileWriter) Seek(offset int64, whence int) (int64, error) { return 0, nil }
func (w *WebDAVFileWriter) Readdir(count int) ([]os.FileInfo, error) { return nil, fs.ErrInvalid }
func (w *WebDAVFileWriter) Stat() (os.FileInfo, error) {
	modTime := time.Now()
	if !w.modTime.IsZero() {
		modTime = w.modTime
	}
	return &VirtualFileInfo{name: w.name, size: w.size, mode: 0644, modTime: modTime}, nil
}

// noopDeadProps implementa webdav.DeadPropsHolder para aceptar PROPPATCH (ej. atributos Win32 de Windows WebClient)
// sin abrir el archivo en modo escritura ni truncarlo en la nube.
type noopDeadProps struct{}

func (noopDeadProps) DeadProps() (map[xml.Name]webdav.Property, error) {
	return nil, nil
}

func (noopDeadProps) Patch(patches []webdav.Proppatch) ([]webdav.Propstat, error) {
	ps := webdav.Propstat{Status: http.StatusOK}
	for _, patch := range patches {
		for _, p := range patch.Props {
			ps.Props = append(ps.Props, webdav.Property{XMLName: p.XMLName})
		}
	}
	return []webdav.Propstat{ps}, nil
}

type WebDAVFileReader struct {
	noopDeadProps
	mfs      *MovistarFS
	path     string
	item     sapi.MediaItem
	pos      int64
	stream   io.ReadCloser
	streamMu sync.Mutex
}

func NewWebDAVFileReader(mfs *MovistarFS, path string, item sapi.MediaItem) *WebDAVFileReader {
	return &WebDAVFileReader{mfs: mfs, path: path, item: item}
}

func (r *WebDAVFileReader) Read(p []byte) (n int, err error) {
	if r.item.Size == 0 || r.pos >= r.item.Size {
		return 0, io.EOF
	}

	r.streamMu.Lock()
	st := r.stream
	pos := r.pos
	r.streamMu.Unlock()

	if st == nil {
		sizeMB := float64(r.item.Size) / (1024 * 1024)
		if pos > 0 {
			logger.Log("[WebDAV] ⬇ Descargando rango [byte %d..] de '%s' (%.2f MB)...", pos, r.item.Name, sizeMB)
		} else {
			logger.Log("[WebDAV] ⬇ Descargando: '%s' (%.2f MB) desde Movistar Cloud...", r.item.Name, sizeMB)
		}
		downloadSem <- struct{}{}
		globalTransferSem <- struct{}{}
		rawStream, err := r.mfs.client.DownloadFileStreamRange(r.item, pos)
		if err != nil {
			<-globalTransferSem
			<-downloadSem
			return 0, err
		}
		semStream := &semaphoreReleaseReader{stream: rawStream}

		r.streamMu.Lock()
		if r.stream != nil {
			_ = semStream.Close()
			st = r.stream
		} else {
			r.stream = semStream
			st = semStream
		}
		r.streamMu.Unlock()
	}

	n, err = st.Read(p)
	if n > 0 {
		r.streamMu.Lock()
		r.pos += int64(n)
		r.streamMu.Unlock()
	}
	return n, err
}

func (r *WebDAVFileReader) Write(p []byte) (n int, err error) { return 0, fs.ErrPermission }

func (r *WebDAVFileReader) Close() error {
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	if r.stream != nil {
		err := r.stream.Close()
		r.stream = nil
		return err
	}
	return nil
}

func (r *WebDAVFileReader) Seek(offset int64, whence int) (int64, error) {
	r.streamMu.Lock()
	defer r.streamMu.Unlock()

	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = r.pos + offset
	case io.SeekEnd:
		newPos = r.item.Size + offset
	default:
		return 0, os.ErrInvalid
	}

	if newPos < 0 {
		return 0, os.ErrInvalid
	}

	if newPos != r.pos {
		if r.stream != nil {
			_ = r.stream.Close()
			r.stream = nil
		}
		r.pos = newPos
	}
	return newPos, nil
}
func (r *WebDAVFileReader) Readdir(count int) ([]os.FileInfo, error) { return nil, fs.ErrInvalid }
func (r *WebDAVFileReader) Stat() (os.FileInfo, error) {
	return &VirtualFileInfo{name: r.item.Name, size: r.item.Size, mode: 0644, modTime: parseTime(r.item.ModificationDate)}, nil
}

type semaphoreReleaseReader struct {
	stream   io.ReadCloser
	released bool
	mu       sync.Mutex
}

func (s *semaphoreReleaseReader) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.released {
		s.released = true
		<-globalTransferSem
		<-downloadSem
	}
}

func (s *semaphoreReleaseReader) Read(p []byte) (n int, err error) {
	n, err = s.stream.Read(p)
	if err == io.EOF {
		s.release()
	}
	return n, err
}

func (s *semaphoreReleaseReader) Close() error {
	s.release()
	return s.stream.Close()
}

type WebDAVMemoryReader struct {
	noopDeadProps
	path   string
	mem    *MemoryFile
	reader *bytes.Reader
}

func NewWebDAVMemoryReader(path string, mem *MemoryFile) *WebDAVMemoryReader {
	return &WebDAVMemoryReader{
		path:   path,
		mem:    mem,
		reader: bytes.NewReader(mem.Content),
	}
}

func (m *WebDAVMemoryReader) Read(p []byte) (n int, err error) { return m.reader.Read(p) }
func (m *WebDAVMemoryReader) Write(p []byte) (n int, err error) { return 0, fs.ErrPermission }
func (m *WebDAVMemoryReader) Close() error { return nil }
func (m *WebDAVMemoryReader) Seek(offset int64, whence int) (int64, error) { return m.reader.Seek(offset, whence) }
func (m *WebDAVMemoryReader) Readdir(count int) ([]os.FileInfo, error) { return nil, fs.ErrInvalid }
func (m *WebDAVMemoryReader) Stat() (os.FileInfo, error) {
	return &VirtualFileInfo{name: m.mem.Name, size: m.mem.Size, mode: 0644, modTime: m.mem.ModTime}, nil
}

type WebDAVDirReader struct {
	noopDeadProps
	mfs      *MovistarFS
	path     string
	folderID int64
	entries  []os.FileInfo
	readPos  int
	loaded   bool
	mu       sync.Mutex
}

func NewWebDAVDirReader(mfs *MovistarFS, path string, folderID int64) *WebDAVDirReader {
	return &WebDAVDirReader{mfs: mfs, path: path, folderID: folderID}
}

func (d *WebDAVDirReader) Read(p []byte) (n int, err error) { return 0, io.EOF }
func (d *WebDAVDirReader) Write(p []byte) (n int, err error) { return 0, fs.ErrPermission }
func (d *WebDAVDirReader) Close() error                     { return nil }

func (d *WebDAVDirReader) Seek(offset int64, whence int) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if offset == 0 && whence == io.SeekStart {
		d.readPos = 0
		d.loaded = false
		return 0, nil
	}
	return 0, nil
}

func (d *WebDAVDirReader) Stat() (os.FileInfo, error) {
	return &VirtualFileInfo{name: path.Base(d.path), size: 0, mode: os.ModeDir | 0755, modTime: time.Now(), isDir: true}, nil
}

func (d *WebDAVDirReader) Readdir(count int) ([]os.FileInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.loaded {
		var list []os.FileInfo
		subs, err := d.mfs.getSubfolders(d.folderID)
		if err != nil {
			return nil, err
		}
		media, err := d.mfs.getMedia(d.folderID)
		if err != nil {
			return nil, err
		}
		seen := make(map[string]bool)
		now := time.Now()

		d.mfs.cacheMu.RLock()
		for _, s := range subs {
			childPath := path.Clean(path.Join(d.path, s.Name))
			if exp, isTomb := d.mfs.deletedTombstones[childPath]; isTomb && now.Before(exp) {
				continue
			}
			if !seen[s.Name] && !isOSJunk(s.Name) && !isDuplicatiTestPath(s.Name) {
				seen[s.Name] = true
				list = append(list, &VirtualFileInfo{name: s.Name, size: 0, mode: os.ModeDir | 0755, modTime: parseTime(s.ModificationDate), isDir: true})
			}
		}

		for _, m := range media {
			childPath := path.Clean(path.Join(d.path, m.Name))
			if exp, isTomb := d.mfs.deletedTombstones[childPath]; isTomb && now.Before(exp) {
				continue
			}
			name := m.Name
			if !seen[name] && !isOSJunk(name) && !isDuplicatiTestPath(name) {
				seen[name] = true
				list = append(list, &VirtualFileInfo{name: name, size: m.Size, mode: 0644, modTime: parseTime(m.ModificationDate), isDir: false})
			}
		}

		for _, mem := range d.mfs.uploadCache {
			childPath := path.Clean(path.Join(d.path, mem.Name))
			if exp, isTomb := d.mfs.deletedTombstones[childPath]; isTomb && now.Before(exp) {
				continue
			}
			if mem.FolderID == d.folderID && !seen[mem.Name] && !isOSJunk(mem.Name) && !isDuplicatiTestPath(mem.Name) {
				seen[mem.Name] = true
				list = append(list, &VirtualFileInfo{name: mem.Name, size: mem.Size, mode: 0644, modTime: mem.ModTime, isDir: false})
			}
		}
		d.mfs.cacheMu.RUnlock()

		d.entries = list
		d.readPos = 0
		d.loaded = true
	}

	if count <= 0 {
		res := d.entries[d.readPos:]
		d.readPos = len(d.entries)
		return res, nil
	}

	if d.readPos >= len(d.entries) {
		return nil, io.EOF
	}

	end := d.readPos + count
	if end > len(d.entries) {
		end = len(d.entries)
	}

	batch := d.entries[d.readPos:end]
	d.readPos = end
	return batch, nil
}

// DevNullFileWriter implementa webdav.File actuando como un agujero negro (/dev/null)
// para absorber escrituras de archivos basura del sistema operativo sin subirlos a la nube
// ni devolver errores al cliente WebDAV (Windows Explorer, macOS Finder, Linux).
type DevNullFileWriter struct {
	noopDeadProps
	path   string
	name   string
	size   int64
	closed bool
}

func NewDevNullFileWriter(path, name string) *DevNullFileWriter {
	return &DevNullFileWriter{
		path: path,
		name: name,
	}
}

func (d *DevNullFileWriter) Write(p []byte) (n int, err error) {
	d.size += int64(len(p))
	return len(p), nil
}

func (d *DevNullFileWriter) Close() error {
	if !d.closed {
		d.closed = true
		logger.Log("[WebDAV] 🧹 Ignorado archivo del sistema: '%s' (%d bytes absorbidos)", d.path, d.size)
	}
	return nil
}

func (d *DevNullFileWriter) Read(p []byte) (n int, err error) { return 0, io.EOF }
func (d *DevNullFileWriter) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		return offset, nil
	case io.SeekCurrent, io.SeekEnd:
		return d.size + offset, nil
	default:
		return 0, os.ErrInvalid
	}
}
func (d *DevNullFileWriter) Readdir(count int) ([]os.FileInfo, error) { return nil, fs.ErrInvalid }
func (d *DevNullFileWriter) Stat() (os.FileInfo, error) {
	return &VirtualFileInfo{name: d.name, size: d.size, mode: 0644, modTime: time.Now()}, nil
}

// Compile-time interface checks
var (
	_ webdav.File         = (*WebDAVFileWriter)(nil)
	_ webdav.File         = (*WebDAVFileReader)(nil)
	_ webdav.File         = (*WebDAVMemoryReader)(nil)
	_ webdav.File         = (*WebDAVDirReader)(nil)
	_ webdav.File         = (*DevNullFileWriter)(nil)
	_ webdav.FileSystem   = (*MovistarFS)(nil)
	_ webdav.ContentTyper = (*VirtualFileInfo)(nil)
)
