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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/webdav"

	"movistar-webdav-proxy/internal/cache"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"
)

var (
	uploadSem           = make(chan struct{}, 1) // 1 subida simultánea (estricta serie como cliente oficial)
	downloadSem         = make(chan struct{}, 1) // 1 descarga simultánea (estricta serie como cliente oficial)
	globalTransferSem   = make(chan struct{}, 1) // Techo conjunto 1:1 para evitar ráfagas hacia SAPI/CloudFront
	uploadBufferSem     = make(chan struct{}, 1) // Límite de ingestión PUT local alineado a 1
	folderLocks         sync.Map
	ErrUploadTooLarge   = errors.New("file exceeds maximum allowed upload size")
	ErrUploadIncomplete = errors.New("upload incomplete: connection cut or missing bytes")
)

// ErrUploadUnsupported: O2 no admite el archivo con el tipo que indica su extensión
// (MED-1050). Se responde 415 y el cliente no debe reintentarlo.
var ErrUploadUnsupported = errors.New("O2 Cloud does not support this file type/content (MED-1050)")

// SetTransferLimits fija cuántas descargas y subidas simultáneas admite el proxy.
// Por defecto es 1 y 1 (como el cliente oficial de escritorio). En modo web las
// descargas van con URL firmada a CloudFront y la API la llama el navegador, así
// que se puede abrir más. Debe llamarse al arrancar, antes de servir peticiones.
func SetTransferLimits(downloads, uploads int) {
	if downloads < 1 {
		downloads = 1
	}
	if uploads < 1 {
		uploads = 1
	}
	downloadSem = make(chan struct{}, downloads)
	uploadSem = make(chan struct{}, uploads)
	uploadBufferSem = make(chan struct{}, uploads)
	// El tope conjunto ya no frena a unas con otras: una subida masiva no debe
	// bloquear las lecturas.
	globalTransferSem = make(chan struct{}, downloads+uploads)
}

// rejectUnsupported marca la subida para responder 415: O2 no admite el archivo con
// el tipo que indica su extensión y reintentarlo no lo arreglará.
func (w *WebDAVFileWriter) rejectUnsupported() error {
	if w.guard != nil {
		w.guard.unsupported.Store(true)
	}
	logger.Log("[WebDAV] ❌ O2 Cloud no admite '%s': su contenido no corresponde a la extensión (MED-1050). No se reintenta.", w.name)
	return ErrUploadUnsupported
}

type uploadGuardKey struct{}

type uploadGuard struct {
	expected int64 // Content-Length anunciado (-1 si no se sabe)
	broken   atomic.Bool
	tooLarge atomic.Bool
	// unsupported: O2 rechazó el archivo por su contenido (MED-1050) → 415.
	unsupported atomic.Bool
	// retryLater: el cuerpo llegó entero pero O2 no aceptó la subida (sesión,
	// validación MED-1017, corte de red, cortafuegos…). x/net/webdav lo convierte en
	// 405, que Hyper Backup muestra como «No permission»; se responde 503 +
	// Retry-After para que el cliente lo reintente (issues #60 y MED-1017).
	retryLater atomic.Bool
	reason     atomic.Value // string: por qué O2 no aceptó la subida (va en el cuerpo del 503)
}

// setRetryLater marca la subida para responder 503 y guarda el motivo, sin datos de
// sesión, para que el cliente vea qué pasó en vez de «Method Not Allowed».
func (g *uploadGuard) setRetryLater(err error) {
	if g == nil {
		return
	}
	if err != nil {
		msg := validationKeyRe.ReplaceAllString(err.Error(), "${1}X")
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		g.reason.Store(msg)
	}
	g.retryLater.Store(true)
}

var validationKeyRe = regexp.MustCompile(`((?:validationkey|JSESSIONID|access_token|token)=)[^&\s;]+`)

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

// isControlOrLockFile comprueba si la ruta corresponde a un fichero o directorio efímero de bloqueo de Hyper Backup.
// @writer contiene estado persistente del repositorio (sequence_id, seq_mapping, v1.cformat y cinfo),
// por lo que no debe emularse localmente como los locks o los directorios @reader temporales.
func isControlOrLockFile(rawPath string) bool {
	clean := strings.ToLower(path.Clean("/" + rawPath))
	base := strings.ToLower(path.Base(clean))
	return strings.Contains(clean, "/control/@reader") ||
		strings.HasPrefix(base, "@reader") ||
		strings.HasPrefix(base, "lock_") ||
		strings.Contains(base, "error_detect")
}

// isTolerantDelete comprueba si un error al borrar un archivo debe gestionarse de forma tolerante (204 en WebDAV)
// para evitar que un 403 transitorio de CloudFront WAF o el borrado de índices obsoletos en remove_conflict
// interrumpa y suspenda una tarea de copia de seguridad ya completada.
func isTolerantDelete(rawPath string, err error) bool {
	if isControlOrLockFile(rawPath) {
		return true
	}
	clean := strings.ToLower(path.Clean("/" + rawPath))
	if strings.Contains(clean, "/config/virtual_file.index/") || strings.HasSuffix(clean, ".idx") || strings.Contains(clean, "/control/") {
		return true
	}
	if err != nil {
		errStr := strings.ToLower(err.Error())
		if strings.Contains(errStr, "403") || strings.Contains(errStr, "cloudfront") || strings.Contains(errStr, "request blocked") {
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
	ID       string
	Name     string
	FolderID int64
	Content  []byte
	Size     int64
	ModTime  time.Time // fecha de modificación que se muestra al cliente
	CachedAt time.Time // momento en que entró en la caché (caducidad)
	URL      string
	// URLConfirmed indica que URL ya se ha obtenido de SAPI consultando el archivo por su ID.
	URLConfirmed bool
	// URLLookupFailedAt es el momento del último error al consultar la URL (cero si no hubo).
	URLLookupFailedAt time.Time
	MD5               string
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

	defer func() {
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
		c.wg.Done()
	}()

	c.val, c.err = fn()
	return c.val, c.err
}

type MovistarFS struct {
	client            *sapi.Client
	metaStore         *cache.MetadataStore
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

	rootFolderIDCache atomic.Int64
	sapiFallbackMu    sync.Mutex
	sapiFallbackCD    map[int64]time.Time

	lastSyncTime    time.Time
	nextSyncAllowed time.Time
	syncFailures    int
	syncInterval    time.Duration
	syncMu          sync.Mutex
	isSyncing       atomic.Bool

	emptyFileManager *EmptyFileManager

	removeErrMu   sync.Mutex
	lastRemoveErr error
}

func (mfs *MovistarFS) setLastRemoveErr(err error) {
	mfs.removeErrMu.Lock()
	mfs.lastRemoveErr = err
	mfs.removeErrMu.Unlock()
}

// LastRemoveErr devuelve el último error de RemoveAll (para mapear renovaciones
// transitorias de SAPI a HTTP 503 en lugar del 405 genérico de x/net/webdav).
func (mfs *MovistarFS) LastRemoveErr() error {
	mfs.removeErrMu.Lock()
	defer mfs.removeErrMu.Unlock()
	return mfs.lastRemoveErr
}

type cachedFolders struct {
	folders []sapi.FolderItem
	byExact map[string]sapi.FolderItem
	byLower map[string]sapi.FolderItem
	time    time.Time
}

func newCachedFolders(folders []sapi.FolderItem) *cachedFolders {
	byExact := make(map[string]sapi.FolderItem, len(folders))
	byLower := make(map[string]sapi.FolderItem, len(folders))
	for _, f := range folders {
		byExact[f.Name] = f
		if _, ok := byLower[strings.ToLower(f.Name)]; !ok {
			byLower[strings.ToLower(f.Name)] = f
		}
	}
	return &cachedFolders{
		folders: folders,
		byExact: byExact,
		byLower: byLower,
		time:    time.Now(),
	}
}

func (cf *cachedFolders) find(name string, fold bool) (*sapi.FolderItem, bool) {
	get := func(n string) (*sapi.FolderItem, bool) {
		if item, ok := cf.byExact[n]; ok {
			itemCopy := item
			return &itemCopy, true
		}
		if fold {
			if item, ok := cf.byLower[strings.ToLower(n)]; ok {
				itemCopy := item
				return &itemCopy, true
			}
		}
		return nil, false
	}
	if item, ok := get(name); ok {
		return item, true
	}
	// Tolerancia Synology HyperBackup .hbk
	if strings.HasSuffix(strings.ToLower(name), ".hbk") && len(name) > 4 {
		return get(name[:len(name)-4])
	}
	return get(name + ".hbk")
}

type cachedMedia struct {
	media   []sapi.MediaItem
	byExact map[string]sapi.MediaItem
	byLower map[string]sapi.MediaItem
	time    time.Time
}

func isMoreRecentMedia(a, b sapi.MediaItem) bool {
	if a.ModificationDate != b.ModificationDate {
		return a.ModificationDate > b.ModificationDate
	}
	idA, errA := strconv.ParseInt(a.ID, 10, 64)
	idB, errB := strconv.ParseInt(b.ID, 10, 64)
	if errA == nil && errB == nil {
		return idA > idB
	}
	return a.ID > b.ID
}

func newCachedMedia(media []sapi.MediaItem) *cachedMedia {
	byExact := make(map[string]sapi.MediaItem, len(media))
	byLower := make(map[string]sapi.MediaItem, len(media))
	// Nombres que solo difieren en mayúsculas son archivos distintos en O2: cada uno
	// se busca por su nombre exacto. byLower solo sirve de respaldo para quien pide
	// un nombre con otras mayúsculas (clientes de Windows), y apunta al más reciente.
	for _, m := range media {
		if existing, exists := byExact[m.Name]; exists {
			logger.Log("[WebDAV] ⚠️ Nombres duplicados en caché para '%s' (IDs: %s y %s). Seleccionando el más reciente.", m.Name, existing.ID, m.ID)
			if isMoreRecentMedia(m, existing) {
				byExact[m.Name] = m
			}
		} else {
			byExact[m.Name] = m
		}
		lower := strings.ToLower(m.Name)
		if existing, exists := byLower[lower]; !exists || isMoreRecentMedia(m, existing) {
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

func (cm *cachedMedia) find(name string, fold bool) (*sapi.MediaItem, bool) {
	if item, ok := cm.byExact[name]; ok {
		itemCopy := item
		return &itemCopy, true
	}
	if !fold {
		return nil, false
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

func NewMovistarFS(client *sapi.Client, metaStores ...*cache.MetadataStore) *MovistarFS {
	var ms *cache.MetadataStore
	if len(metaStores) > 0 {
		ms = metaStores[0]
	}
	fs := &MovistarFS{
		client:            client,
		metaStore:         ms,
		uploadCache:       make(map[string]*MemoryFile),
		createdFolders:    make(map[string]*CreatedFolder),
		deletedTombstones: make(map[string]time.Time),
		folderCache:       make(map[int64]*cachedFolders),
		mediaCache:        make(map[int64]*cachedMedia),
		sapiFallbackCD:    make(map[int64]time.Time),
		cacheTTL:          120 * time.Second,
		duplicatiStore:    NewDuplicatiProbeStore(),
		syncInterval:      config.GetSyncInterval(),
	}
	fs.emptyFileManager = NewEmptyFileManager(fs)
	if client != nil {
		client.SetMediaRefreshHook(fs.applyMediaRefresh)
	}
	if ms != nil {
		ms.SetTombstoneCleaner(fs.ClearTombstone)
		ms.SetTombstoneFilter(fs.HasTombstones)
		ms.SetNameFolding(fs.caseInsensitive)
		go func() {
			time.Sleep(2 * time.Second)
			fs.emptyFileManager.BootstrapLoad()
		}()
	}
	return fs
}

// applyMediaRefresh guarda lo que devuelve la relectura tras subir (como la app): la
// URL de descarga firmada, en la caché de subidas recientes y en la base de
// metadatos. Así la primera descarga no pasa por la URL por defecto, que O2 rechaza
// con COM-1005. Si el archivo cambió o se borró entretanto, no se toca.
func (mfs *MovistarFS) applyMediaRefresh(r sapi.MediaRefresh) {
	if mfs == nil || r.ID == "" || r.URL == "" {
		return
	}
	folderID := int64(-1)
	if ms := mfs.MetadataStore(); ms != nil {
		if m, err := ms.GetMediaByID(r.ID); err == nil && m != nil {
			if r.UploadETag != "" && m.ETag != "" && m.ETag != r.UploadETag {
				return // el archivo cambió después de esa subida
			}
			m.URL = r.URL
			if r.ETag != "" {
				m.ETag = r.ETag
			}
			logMetaErr("UpsertMedia", ms.UpsertMedia(*m))
			folderID = m.FolderID
		}
	}
	stored := folderID >= 0
	mfs.cacheMu.Lock()
	for _, mem := range mfs.uploadCache {
		if mem.ID == r.ID {
			mem.URL = r.URL
			mem.URLConfirmed = true
			mem.URLLookupFailedAt = time.Time{}
			folderID = mem.FolderID
		}
	}
	// los listados en memoria también: sin base de metadatos no hay de dónde recargarla
	for _, cm := range mfs.mediaCache {
		for i := range cm.media {
			if cm.media[i].ID == r.ID {
				cm.media[i].URL = r.URL
			}
		}
		for k, m := range cm.byExact {
			if m.ID == r.ID {
				m.URL = r.URL
				cm.byExact[k] = m
			}
		}
		for k, m := range cm.byLower {
			if m.ID == r.ID {
				m.URL = r.URL
				cm.byLower[k] = m
			}
		}
	}
	mfs.cacheMu.Unlock()
	if stored {
		mfs.InvalidateCache(folderID) // se recarga desde la base, ya con la URL
	}
}

// cachedDownloadURL: la URL de descarga que el proxy conoce para un ID (base de
// metadatos o listados en memoria).
func (mfs *MovistarFS) cachedDownloadURL(id string) string {
	if ms := mfs.MetadataStore(); ms != nil {
		if m, err := ms.GetMediaByID(id); err == nil && m != nil {
			return m.URL
		}
	}
	mfs.cacheMu.RLock()
	defer mfs.cacheMu.RUnlock()
	for _, cm := range mfs.mediaCache {
		for _, m := range cm.media {
			if m.ID == id && m.URL != "" {
				return m.URL
			}
		}
	}
	return ""
}

func logMetaErr(op string, err error) {
	if err != nil {
		logger.Log("[Metadata] ⚠️ Error en %s: %v", op, err)
	}
}

func (mfs *MovistarFS) MetadataStore() *cache.MetadataStore {
	if mfs == nil {
		return nil
	}
	mfs.cacheMu.RLock()
	defer mfs.cacheMu.RUnlock()
	return mfs.metaStore
}

func (mfs *MovistarFS) SetMetadataStore(ms *cache.MetadataStore) {
	if mfs == nil {
		return
	}
	mfs.cacheMu.Lock()
	mfs.metaStore = ms
	mfs.rootFolderIDCache.Store(0)
	mfs.cacheMu.Unlock()
	if ms != nil {
		ms.SetTombstoneCleaner(mfs.ClearTombstone)
		ms.SetTombstoneFilter(mfs.HasTombstones)
		ms.SetNameFolding(mfs.caseInsensitive)
		if mfs.emptyFileManager != nil {
			go func() {
				time.Sleep(2 * time.Second)
				mfs.emptyFileManager.BootstrapLoad()
			}()
		}
	}
}

// realEmptyFiles: los archivos de 0 bytes se suben de verdad a la nube
// (config.RealEmptyFiles), salvo en el modo web, que no usa la subida de la app.
func (mfs *MovistarFS) realEmptyFiles() bool {
	return config.RealEmptyFiles() && mfs != nil && mfs.client != nil && !mfs.client.WebMode()
}

// BootstrapEmptyFiles carga desde la nube el manifiesto de archivos vacíos emulados hacia SQLite.
func (mfs *MovistarFS) BootstrapEmptyFiles() {
	if mfs == nil || mfs.emptyFileManager == nil {
		return
	}
	mfs.emptyFileManager.BootstrapLoad()
}

// BootstrapEmptyFilesIfPending carga los vacíos solo si en este arranque aún no se pudo
// (p. ej. la sincronización inicial falló y no había metadatos). Para la sincronización
// periódica: una vez cargados no vuelve a descargar el manifiesto.
func (mfs *MovistarFS) BootstrapEmptyFilesIfPending() {
	if mfs == nil || mfs.emptyFileManager == nil || mfs.emptyFileManager.bootstrapped.Load() {
		return
	}
	mfs.emptyFileManager.BootstrapLoad()
}

// HasTombstones reports whether there are any active deleted tombstones in memory.
func (mfs *MovistarFS) HasTombstones() bool {
	if mfs == nil {
		return false
	}
	mfs.cacheMu.RLock()
	has := len(mfs.deletedTombstones) > 0
	mfs.cacheMu.RUnlock()
	return has
}

// ClearTombstone removes an entry from deletedTombstones if it was present.
func (mfs *MovistarFS) ClearTombstone(itemPath string) {
	if mfs == nil {
		return
	}
	mfs.cacheMu.RLock()
	if len(mfs.deletedTombstones) == 0 {
		mfs.cacheMu.RUnlock()
		return
	}
	mfs.cacheMu.RUnlock()

	lower := cacheKey(itemPath)
	mfs.cacheMu.Lock()
	delete(mfs.deletedTombstones, lower)
	mfs.cacheMu.Unlock()
}

// cacheKey es la clave de las cachés en memoria (subidas recientes, carpetas
// creadas y marcas de borrado). Distingue mayúsculas: O2 Cloud admite "A.txt" y
// "a.txt" en la misma carpeta, y con claves en minúsculas una pisaba a la otra.
func cacheKey(p string) string { return path.Clean("/" + p) }

// foldLookup busca key en m; si no está, una clave que solo difiera en mayúsculas.
// Devuelve la clave encontrada ("" si ninguna).
func foldLookup[T any](m map[string]T, key string) (string, T, bool) {
	if v, ok := m[key]; ok {
		return key, v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return k, v, true
		}
	}
	var zero T
	return "", zero, false
}

// pickByName elige, de n candidatos, el que se llama exactamente want; si no hay
// ninguno, el primero que coincide sin distinguir mayúsculas (y, con hbk, la
// tolerancia de Hyper Backup al sufijo .hbk). -1 si ninguno.
func pickByName(n int, nameAt func(int) string, want string, hbk, fold bool) int {
	for i := 0; i < n; i++ {
		if nameAt(i) == want {
			return i
		}
	}
	same := func(a, b string) bool {
		if fold {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	for i := 0; i < n; i++ {
		got := nameAt(i)
		if same(got, want) {
			return i
		}
		if hbk && strings.HasSuffix(strings.ToLower(want), ".hbk") && same(got, want[:len(want)-4]) {
			return i
		}
	}
	return -1
}

// recentUpload busca un archivo recién subido en la caché de subidas (con cacheMu
// tomado): por su ruta exacta y, con WEBDAV_CASE_INSENSITIVE, también con otras
// mayúsculas si es el mismo archivo que se resolvió (o no se resolvió ninguno).
func (mfs *MovistarFS) recentUpload(key string, resolved *sapi.MediaItem) (*MemoryFile, bool) {
	if mem, ok := mfs.uploadCache[key]; ok {
		return mem, true
	}
	if !mfs.caseInsensitive() {
		return nil, false
	}
	if _, mem, ok := foldLookup(mfs.uploadCache, key); ok && (resolved == nil || resolved.ID == mem.ID) {
		return mem, true
	}
	return nil, false
}

// caseInsensitiveNames: WEBDAV_CASE_INSENSITIVE=true vuelve al comportamiento
// anterior (un nombre con otras mayúsculas encuentra el archivo). Por defecto los
// nombres son exactos, como en O2.
func (mfs *MovistarFS) caseInsensitive() bool {
	return config.CaseInsensitiveNames()
}

// isSameEntryName: el mismo elemento de O2 (nombre idéntico, mayúsculas incluidas).
func isSameEntryName(a, b string) bool { return a == b }

// SetSyncInterval configures the minimum interval between automatic metadata synchronizations.
func (mfs *MovistarFS) SetSyncInterval(d time.Duration) {
	if mfs == nil {
		return
	}
	mfs.syncMu.Lock()
	mfs.syncInterval = d
	mfs.syncMu.Unlock()
}

// GetSyncInterval returns the configured delta sync interval.
func (mfs *MovistarFS) GetSyncInterval() time.Duration {
	if mfs == nil {
		return config.GetSyncInterval()
	}
	mfs.syncMu.Lock()
	defer mfs.syncMu.Unlock()
	if mfs.syncInterval <= 0 {
		return config.GetSyncInterval()
	}
	return mfs.syncInterval
}

// RecordSyncTime updates the timestamp of the last successful sync run and resets backoff.
func (mfs *MovistarFS) RecordSyncTime(t time.Time) {
	if mfs == nil {
		return
	}
	mfs.syncMu.Lock()
	mfs.lastSyncTime = t
	mfs.syncFailures = 0
	mfs.nextSyncAllowed = time.Time{}
	mfs.syncMu.Unlock()
}

// MaybeTriggerSync launches an asynchronous, non-blocking SyncChanges if time.Since(lastSyncTime) > syncInterval.
func (mfs *MovistarFS) MaybeTriggerSync() {
	if mfs == nil {
		return
	}
	mfs.cacheMu.RLock()
	ms := mfs.metaStore
	client := mfs.client
	mfs.cacheMu.RUnlock()

	if ms == nil || client == nil || client.IsSessionExpired() {
		return
	}
	cfg := client.GetConfig()
	if cfg.RefreshToken == "" && cfg.AccessToken == "" && cfg.ValidationKey == "" {
		return
	}

	interval := mfs.GetSyncInterval()

	mfs.syncMu.Lock()
	now := time.Now()
	if !mfs.nextSyncAllowed.IsZero() && now.Before(mfs.nextSyncAllowed) {
		mfs.syncMu.Unlock()
		return
	}
	if !mfs.lastSyncTime.IsZero() && now.Sub(mfs.lastSyncTime) < interval {
		mfs.syncMu.Unlock()
		return
	}
	if mfs.isSyncing.Load() {
		mfs.syncMu.Unlock()
		return
	}
	mfs.isSyncing.Store(true)
	mfs.lastSyncTime = now
	mfs.syncMu.Unlock()

	go func() {
		defer mfs.isSyncing.Store(false)
		changed, err := ms.SyncChanges(client, false)
		if err != nil {
			mfs.syncMu.Lock()
			mfs.syncFailures++
			factor := 1 << mfs.syncFailures
			if factor > 15 {
				factor = 15
			}
			backoff := interval * time.Duration(factor)
			mfs.nextSyncAllowed = time.Now().Add(backoff)
			failures := mfs.syncFailures
			mfs.syncMu.Unlock()
			logger.Log("[Metadata] ⚠️ Sincronización no bloqueante falló: %v. Pausando sincronizaciones por %v (%d fallos consecutivos)", err, backoff, failures)
		} else {
			mfs.RecordSyncTime(time.Now())
			if changed > 0 {
				mfs.InvalidateCache(0)
				logger.Log("[Metadata] 🔄 Sincronización no bloqueante: %d cambios aplicados.", changed)
			}
		}
	}()
}

func (mfs *MovistarFS) getDuplicatiStore() *DuplicatiProbeStore {
	if mfs != nil && mfs.duplicatiStore != nil {
		return mfs.duplicatiStore
	}
	return globalDuplicatiStore
}

func (mfs *MovistarFS) InvalidateCache(folderID int64) {
	if mfs == nil {
		return
	}
	mfs.cacheMu.Lock()
	defer mfs.cacheMu.Unlock()
	if folderID != 0 {
		delete(mfs.folderCache, folderID)
		delete(mfs.mediaCache, folderID)
	} else {
		mfs.folderCache = make(map[int64]*cachedFolders)
		mfs.mediaCache = make(map[int64]*cachedMedia)
		mfs.rootFolderIDCache.Store(0)
	}
}

func (mfs *MovistarFS) GetCachedStorageSpace() *sapi.StorageSpace {
	mfs.cacheMu.RLock()
	defer mfs.cacheMu.RUnlock()
	if mfs.storageCache != nil {
		return mfs.storageCache.space
	}
	return nil
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
	ms := mfs.MetadataStore()
	if ms != nil && (ms.HasInitialSync() || mfs.client == nil) {
		subs, err := ms.ListSubfolders(parentID)
		if err == nil {
			return mfs.mergeCreatedFolders(parentID, subs), nil
		}
		logMetaErr("ListSubfolders", err)
	}

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
	if ms != nil && len(subs) > 0 {
		logMetaErr("UpsertFoldersBatch", ms.UpsertFoldersBatch(subs))
	}

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
	var recent []sapi.FolderItem
	for _, cf := range mfs.createdFolders {
		if now.Sub(cf.Time) <= 60*time.Second && cf.Info.ParentID == parentID {
			recent = append(recent, cf.Info)
		}
	}
	if i := pickByName(len(recent), func(i int) string { return recent[i].Name }, name, true, mfs.caseInsensitive()); i >= 0 {
		f := recent[i]
		mfs.cacheMu.RUnlock()
		return &f, true, nil
	}

	cached, ok := mfs.folderCache[parentID]
	if ok && time.Since(cached.time) < mfs.cacheTTL {
		item, found := cached.find(name, mfs.caseInsensitive())
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
		item, found := c.find(name, mfs.caseInsensitive())
		mfs.cacheMu.RUnlock()
		return item, found, nil
	}
	mfs.cacheMu.RUnlock()

	if i := pickByName(len(subs), func(i int) string { return subs[i].Name }, name, true, mfs.caseInsensitive()); i >= 0 {
		return &subs[i], true, nil
	}
	return nil, false, nil
}

func (mfs *MovistarFS) getMedia(folderID int64) ([]sapi.MediaItem, error) {
	ms := mfs.MetadataStore()
	if ms != nil && (ms.HasInitialSync() || mfs.client == nil) {
		items, err := ms.ListMediaInFolder(folderID)
		if err == nil {
			return items, nil
		}
		logMetaErr("ListMediaInFolder", err)
	}

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
	if ms != nil && len(items) > 0 {
		logMetaErr("UpsertMediaBatch", ms.UpsertMediaBatch(items))
	}

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
		item, found := cached.find(name, mfs.caseInsensitive())
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
		item, found := c.find(name, mfs.caseInsensitive())
		mfs.cacheMu.RUnlock()
		return item, found, nil
	}
	mfs.cacheMu.RUnlock()

	for i := range items {
		if items[i].Name == name {
			return &items[i], true, nil
		}
	}
	if mfs.caseInsensitive() {
		for i := range items {
			if strings.EqualFold(items[i].Name, name) {
				return &items[i], true, nil
			}
		}
	}
	return nil, false, nil
}

func (mfs *MovistarFS) fallbackQuerySAPI(folderID int64, needMedia bool) {
	if mfs == nil || mfs.client == nil {
		return
	}
	mfs.sapiFallbackMu.Lock()
	if mfs.sapiFallbackCD == nil {
		mfs.sapiFallbackCD = make(map[int64]time.Time)
	}
	lastCall, inCooldown := mfs.sapiFallbackCD[folderID]
	now := time.Now()
	if inCooldown && now.Sub(lastCall) < 15*time.Second {
		mfs.sapiFallbackMu.Unlock()
		return
	}
	mfs.sapiFallbackCD[folderID] = now
	mfs.sapiFallbackMu.Unlock()

	ms := mfs.MetadataStore()

	subs, err := mfs.client.ListAllSubfolders(folderID)
	if err == nil && len(subs) > 0 && ms != nil {
		logMetaErr("UpsertFoldersBatch fallback", ms.UpsertFoldersBatch(subs))
	}

	if needMedia {
		media, errMedia := mfs.client.ListAllMediaInFolder(folderID)
		if errMedia == nil && len(media) > 0 && ms != nil {
			logMetaErr("UpsertMediaBatch fallback", ms.UpsertMediaBatch(media))
		}
	}
}

func (mfs *MovistarFS) getRootFolderID() (int64, error) {
	if mfs == nil {
		return 0, errors.New("mfs is nil")
	}
	if id := mfs.rootFolderIDCache.Load(); id != 0 {
		return id, nil
	}

	ms := mfs.MetadataStore()
	if ms != nil {
		if rf, err := ms.GetRootFolder(); err == nil && rf != nil && rf.ID != 0 {
			mfs.rootFolderIDCache.Store(rf.ID)
			return rf.ID, nil
		}
	}

	if mfs.client == nil {
		return 0, errors.New("sapi client is nil")
	}

	if !mfs.client.HasCredentials() {
		return 0, errors.New("sesión SAPI no autorizada: vincula la cuenta en el panel web")
	}

	rootID, err := mfs.client.GetRootFolderID()
	if err != nil {
		return 0, err
	}

	if rootID != 0 {
		mfs.rootFolderIDCache.Store(rootID)
		if ms != nil {
			logMetaErr("SetSyncState root_folder_id", ms.SetSyncState("root_folder_id", strconv.FormatInt(rootID, 10)))
			logMetaErr("UpsertFolder root", ms.UpsertFolder(sapi.FolderItem{
				ID:       rootID,
				Name:     "/",
				Magic:    true,
				ParentID: 0,
			}))
		}
	}
	return rootID, nil
}

// ensureFolderChain ensures all directory segments in dirPath exist in SQLite/SAPI and returns the final folder.
func (mfs *MovistarFS) ensureFolderChain(dirPath string) (*sapi.FolderItem, error) {
	cleanPath := path.Clean("/" + dirPath)
	rootID, err := mfs.getRootFolderID()
	if err != nil {
		return nil, err
	}
	if cleanPath == "/" || cleanPath == "." {
		return &sapi.FolderItem{ID: rootID, Name: "/", Magic: true}, nil
	}
	parts := strings.Split(strings.Trim(cleanPath, "/"), "/")
	currentID := rootID
	var currentFolder *sapi.FolderItem = &sapi.FolderItem{ID: rootID, Name: "/", Magic: true}
	ms := mfs.MetadataStore()

	for _, part := range parts {
		if part == "" {
			continue
		}
		var found *sapi.FolderItem
		if ms != nil {
			f, err := ms.GetFolderByParentAndName(currentID, part)
			if err == nil && f != nil {
				found = f
			}
		}
		if found == nil {
			subs, err := mfs.getSubfolders(currentID)
			if err == nil {
				if i := pickByName(len(subs), func(i int) string { return subs[i].Name }, part, false, mfs.caseInsensitive()); i >= 0 {
					s := subs[i]
					{
						item := s
						found = &item
						if ms != nil {
							_ = ms.UpsertFolder(item)
						}
					}
				}
			}
		}
		if found == nil {
			created, err := mfs.client.CreateFolder(part, currentID)
			if err != nil {
				return nil, fmt.Errorf("create folder '%s' in %d: %w", part, currentID, err)
			}
			found = created
			if ms != nil {
				_ = ms.UpsertFolder(*created)
			}
			mfs.InvalidateCache(currentID)
		}
		currentID = found.ID
		currentFolder = found
	}
	return currentFolder, nil
}

// ResolvePath resolves a path to folderInfo or mediaInfo.
func (mfs *MovistarFS) ResolvePath(rawPath string) (*sapi.FolderItem, *sapi.MediaItem, bool, error) {
	return mfs.resolvePath(rawPath)
}

func (mfs *MovistarFS) resolvePath(rawPath string) (folderInfo *sapi.FolderItem, mediaInfo *sapi.MediaItem, isFolder bool, err error) {
	return mfs.resolvePathOpts(rawPath, true)
}

// localMetadataReady indica si la base SQLite ya tiene la sincronización inicial hecha:
// entonces "¿existe ya?" se puede responder en local, sin consultar a la API.
func (mfs *MovistarFS) localMetadataReady() bool {
	ms := mfs.MetadataStore()
	return ms != nil && ms.HasInitialSync()
}

// resolvePathOpts es resolvePath con la consulta de respaldo a SAPI (fallbackQuerySAPI)
// opcional. Con allowSAPI=false, lo que no está en SQLite se da por inexistente sin
// llamar a la API (ver OpenFile: la comprobación previa a cada subida).
func (mfs *MovistarFS) resolvePathOpts(rawPath string, allowSAPI bool) (folderInfo *sapi.FolderItem, mediaInfo *sapi.MediaItem, isFolder bool, err error) {
	folderInfo, mediaInfo, isFolder, err = mfs.resolvePathExact(rawPath, allowSAPI)
	if !errors.Is(err, os.ErrNotExist) || !mfs.caseInsensitive() {
		return
	}
	// Nada con ese nombre exacto ni en la base ni en O2: un archivo o una carpeta
	// recién creados que solo difieren en mayúsculas (clientes de Windows que piden
	// el mismo con otras). La marca de borrado manda también aquí.
	key := cacheKey(rawPath)
	mfs.cacheMu.RLock()
	defer mfs.cacheMu.RUnlock()
	tomb := func(k string) bool {
		exp, ok := mfs.deletedTombstones[k]
		return ok && time.Now().Before(exp)
	}
	if tomb(key) {
		return
	}
	if k, cf, ok := foldLookup(mfs.createdFolders, key); ok && !tomb(k) && time.Since(cf.Time) < 60*time.Second {
		info := cf.Info
		return &info, nil, true, nil
	}
	if k, mem, ok := foldLookup(mfs.uploadCache, key); ok && !tomb(k) {
		return nil, &sapi.MediaItem{
			ID:               mem.ID,
			Name:             mem.Name,
			Size:             mem.Size,
			URL:              mem.URL,
			FolderID:         mem.FolderID,
			ModificationDate: sapi.FlexDate(mem.ModTime.UTC().Format("20060102T150405Z")),
		}, false, nil
	}
	return
}

func (mfs *MovistarFS) resolvePathExact(rawPath string, allowSAPI bool) (folderInfo *sapi.FolderItem, mediaInfo *sapi.MediaItem, isFolder bool, err error) {
	cleanPath := path.Clean("/" + rawPath)
	pathKey := cacheKey(cleanPath)

	rootID, err := mfs.getRootFolderID()
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

	// Directorio interno del proxy (manifiesto de archivos vacíos, etc.)
	if isInternalProxyPath(cleanPath) {
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

	// Check in-memory caches and active tombstones
	mfs.cacheMu.RLock()
	// Las cachés van por nombre exacto (O2 distingue mayúsculas); la búsqueda con
	// otras mayúsculas se hace al final, en resolvePathOpts, si nada coincide exacto.
	cf, cfOK := mfs.createdFolders[pathKey]
	mem, upOK := mfs.uploadCache[pathKey]
	var isTomb bool
	if exp, ok := mfs.deletedTombstones[pathKey]; ok && time.Now().Before(exp) {
		isTomb = true
	}

	// 1. Check createdFolders con verificación de TTL (60s)
	var createdF *sapi.FolderItem
	if cfOK && time.Since(cf.Time) < 60*time.Second {
		info := cf.Info
		createdF = &info
	}

	// 2. Check uploadCache
	var uploadMi *sapi.MediaItem
	if upOK {
		uploadMi = &sapi.MediaItem{
			ID:               mem.ID,
			Name:             mem.Name,
			Size:             mem.Size,
			URL:              mem.URL,
			FolderID:         mem.FolderID,
			ModificationDate: sapi.FlexDate(mem.ModTime.UTC().Format("20060102T150405Z")),
		}
	}
	ms := mfs.metaStore
	mfs.cacheMu.RUnlock()

	// When metaStore is nil, tombstones immediately suppress the path (transient 10-15s suppression)
	if ms == nil && isTomb {
		return nil, nil, false, os.ErrNotExist
	}
	if !isTomb {
		if createdF != nil {
			return createdF, nil, true, nil
		}
		if uploadMi != nil {
			return nil, uploadMi, false, nil
		}
	}

	parts := strings.Split(strings.Trim(cleanPath, "/"), "/")
	currentID := rootID
	var currentFolder *sapi.FolderItem

	if ms != nil {
		for idx, part := range parts {
			isLast := (idx == len(parts)-1)

			// 1. Check SQLite metadata store first (0 SAPI calls)
			f, fErr := ms.GetFolderByParentAndName(currentID, part)
			if fErr == nil && f != nil {
				currentID = f.ID
				currentFolder = f
				if isLast {
					// When item exists in SQLite metaStore, clear any tombstone! SQLite is authoritative.
					mfs.ClearTombstone(pathKey)
					return currentFolder, nil, true, nil
				}
				continue
			}

			// 2. If it is the last segment, check SQLite media before making any network calls
			if isLast {
				m, mErr := ms.GetMediaByFolderAndName(currentID, part)
				if mErr == nil && m != nil {
					// When item exists in SQLite metaStore, clear any tombstone! SQLite is authoritative.
					mfs.ClearTombstone(pathKey)
					return nil, m, false, nil
				}
				if vf, vfErr := ms.GetVirtualEmptyFile(currentID, part); vfErr == nil && vf != nil {
					mfs.ClearTombstone(pathKey)
					vMedia := &sapi.MediaItem{
						ID:               fmt.Sprintf("virtual:%d:%s", currentID, vf.Name),
						Name:             vf.Name,
						FolderID:         currentID,
						Size:             0,
						ModificationDate: sapi.FlexDate(vf.ModTime),
					}
					return nil, vMedia, false, nil
				}
			}

			// 3. Check active createdFolders in memory
			mfs.cacheMu.RLock()
			var foundCreated *sapi.FolderItem
			now := time.Now()
			var recent []sapi.FolderItem
			for _, cf := range mfs.createdFolders {
				if now.Sub(cf.Time) <= 60*time.Second && cf.Info.ParentID == currentID {
					recent = append(recent, cf.Info)
				}
			}
			if i := pickByName(len(recent), func(i int) string { return recent[i].Name }, part, true, mfs.caseInsensitive()); i >= 0 {
				item := recent[i]
				foundCreated = &item
			}
			mfs.cacheMu.RUnlock()

			if foundCreated != nil {
				currentID = foundCreated.ID
				currentFolder = foundCreated
				if isLast {
					if isTomb {
						return nil, nil, false, os.ErrNotExist
					}
					return currentFolder, nil, true, nil
				}
				continue
			}

			// 4. Item does not exist in SQLite or createdFolders:
			// If it is an active tombstone, do NOT resurrect it via SAPI fallback!
			if isTomb {
				return nil, nil, false, os.ErrNotExist
			}

			// 5. Fallback to direct SAPI query with cooldown and write-through to SQLite
			if allowSAPI {
				mfs.fallbackQuerySAPI(currentID, isLast)
			}

			// Re-check SQLite after SAPI write-through
			f, fErr = ms.GetFolderByParentAndName(currentID, part)
			if fErr == nil && f != nil {
				currentID = f.ID
				currentFolder = f
				if isLast {
					mfs.ClearTombstone(pathKey)
					return currentFolder, nil, true, nil
				}
				continue
			}

			if isLast {
				m, mErr := ms.GetMediaByFolderAndName(currentID, part)
				if mErr == nil && m != nil {
					mfs.ClearTombstone(pathKey)
					return nil, m, false, nil
				}
				if vf, vfErr := ms.GetVirtualEmptyFile(currentID, part); vfErr == nil && vf != nil {
					mfs.ClearTombstone(pathKey)
					vMedia := &sapi.MediaItem{
						ID:               fmt.Sprintf("virtual:%d:%s", currentID, vf.Name),
						Name:             vf.Name,
						FolderID:         currentID,
						Size:             0,
						ModificationDate: sapi.FlexDate(vf.ModTime),
					}
					return nil, vMedia, false, nil
				}
			}

			return nil, nil, false, os.ErrNotExist
		}

		return nil, nil, false, os.ErrNotExist
	}

	// Fallback path traversal when metaStore is nil
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
	pathKey := cacheKey(cleanPath)

	// Si es una carpeta de sistema o metadatos de SO ($RECYCLE.BIN, .Trashes, etc.), absorber silenciosamente
	if isOSJunkPath(cleanPath) {
		logger.Log("[WebDAV] 🧹 Ignorada creación de carpeta del sistema: '%s'", cleanPath)
		return nil
	}

	unlock := acquireFolderLock(cleanPath)
	defer unlock()

	// Clear tombstone AND clear any conflicting uploadCache entry if re-creating
	mfs.cacheMu.Lock()
	delete(mfs.deletedTombstones, pathKey)
	delete(mfs.uploadCache, pathKey)
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
		// Solo con el nombre exacto: O2 admite carpetas que difieren en mayúsculas.
		if isSameEntryName(s.Name, folderName) {
			logger.Log("[WebDAV] ℹ Carpeta '%s' ya existía en Movistar/O2 Cloud (ID: %d). Reutilizando.", folderName, s.ID)
			mfs.cacheMu.Lock()
			delete(mfs.uploadCache, pathKey)
			mfs.createdFolders[pathKey] = &CreatedFolder{Info: s, Time: time.Now()}
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
	if ms := mfs.MetadataStore(); ms != nil {
		logMetaErr("UpsertFolder", ms.UpsertFolder(*created))
		logMetaErr("UpdateFolderModificationDate", ms.UpdateFolderModificationDate(parentFolder.ID, time.Now().UTC().Format("20060102T150405Z")))
	}
	mfs.cacheMu.Lock()
	delete(mfs.uploadCache, pathKey)
	mfs.createdFolders[pathKey] = &CreatedFolder{Info: *created, Time: time.Now()}
	mfs.cacheMu.Unlock()
	mfs.InvalidateCache(parentFolder.ID)

	return nil
}

func (mfs *MovistarFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	cleanPath := path.Clean("/" + name)
	pathKey := cacheKey(cleanPath)

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
		// ¿Existe ya el destino? Con la base SQLite ya sincronizada se responde en local.
		// Consultar a la API cada archivo nuevo (fallbackQuerySAPI: ListAllSubfolders +
		// ListAllMediaInFolder) costaba 2 llamadas por subida y era lo que disparaba el
		// 403 del WAF en subidas masivas (medido el 12/11/2026 con la v2.3.0). Lo creado
		// desde otro cliente entra por la sincronización de cambios (SYNC_INTERVAL_SEC).
		existingFolder, mediaInfo, isF, err := mfs.resolvePathOpts(cleanPath, !mfs.localMetadataReady())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil && isF && existingFolder != nil {
			logger.Log("[WebDAV] ℹ️ Rechazado PUT sobre carpeta existente: '%s' (RFC 4918 405 Method Not Allowed)", cleanPath)
			return nil, os.ErrInvalid
		}

		// Chequear si ya existe un archivo con ese nombre para borrarlo antes y no crear sufijo ' (1)'
		// Solo se sobrescribe un archivo con el nombre EXACTO: O2 admite "A.txt" y
		// "a.txt" juntos, y antes subir "a.txt" pisaba "A.txt" (y al revés en cada copia).
		var existingID string
		if err == nil && !isF && mediaInfo != nil && isSameEntryName(mediaInfo.Name, fileName) {
			existingID = mediaInfo.ID
		}

		fw := NewWebDAVFileWriter(mfs, cleanPath, fileName, parentFolder.ID, existingID)
		fw.ctx = ctx
		if existingID != "" {
			fw.existingItem = mediaInfo
		}
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
		var mt time.Time
		if folderInfo != nil && string(folderInfo.ModificationDate) != "" {
			mt = parseTime(folderInfo.ModificationDate)
		}
		return NewWebDAVDirReader(mfs, cleanPath, folderInfo.ID, mt), nil
	}

	// Comprobar memoria primero (con otras mayúsculas, solo si es el mismo archivo resuelto)
	mfs.cacheMu.RLock()
	if mem, ok := mfs.recentUpload(pathKey, mediaInfo); ok {
		mfs.cacheMu.RUnlock()
		if mem.Size == 0 || len(mem.Content) > 0 {
			return NewWebDAVMemoryReader(cleanPath, mem), nil
		}
		mfs.cacheMu.RLock()
		knownURL := mem.URL
		mfs.cacheMu.RUnlock()
		item := sapi.MediaItem{
			ID:               mem.ID,
			Name:             mem.Name,
			Size:             mem.Size,
			URL:              knownURL,
			FolderID:         mem.FolderID,
			ModificationDate: sapi.FlexDate(mem.ModTime.UTC().Format("20060102T150405Z")),
		}
		// La URL de descarga se consulta a SAPI solo si de verdad se lee el contenido
		// (primer Read), en los dos modos. x/net/webdav abre el archivo en cada PROPFIND
		// solo para leer sus propiedades, y rclone hace un PROPFIND tras cada PUT:
		// consultarla aquí suponía una petición extra a O2 por cada archivo subido (el
		// doble de tráfico en una subida masiva, y más cerca del límite del WAF).
		r := NewWebDAVFileReader(mfs, cleanPath, item)
		// La subida no siempre devuelve URL de descarga y SAPI tarda unos segundos en
		// indexar el archivo recién subido; esperamos brevemente en ambos modos a que
		// GetMediaByID devuelva la URL firmada en lugar de caer en /media/content?id=.
		r.resolveURL = func() string {
			mfs.waitUploadSettled(ctx, mem.ID, mem.Name)
			return mfs.waitForDownloadURL(mem)
		}
		return r, nil
	}
	mfs.cacheMu.RUnlock()

	if mediaInfo != nil && (mediaInfo.Size == 0 || strings.HasPrefix(mediaInfo.ID, "virtual:")) {
		return NewWebDAVMemoryReader(cleanPath, &MemoryFile{
			ID:       mediaInfo.ID,
			Name:     mediaInfo.Name,
			FolderID: mediaInfo.FolderID,
			Content:  []byte{},
			Size:     0,
			ModTime:  parseTime(mediaInfo.ModificationDate),
			CachedAt: time.Now(),
		}), nil
	}

	r := NewWebDAVFileReader(mfs, cleanPath, *mediaInfo)
	if mfs.client != nil && mfs.client.UploadSettling(mediaInfo.ID) {
		id, name := mediaInfo.ID, mediaInfo.Name
		r.resolveURL = func() string {
			if mfs.waitUploadSettled(ctx, id, name) {
				return mfs.cachedDownloadURL(id)
			}
			return ""
		}
	}
	return r, nil
}

// waitUploadSettled: un archivo recién subido no se puede descargar hasta que O2
// termina de validarlo (antes, la URL por defecto da COM-1005 y la firmada aún no
// existe). La descarga espera a que termine la validación y la relectura, sin límite
// propio: mientras el cliente siga conectado.
func (mfs *MovistarFS) waitUploadSettled(ctx context.Context, id, name string) bool {
	if mfs.client == nil || id == "" || !mfs.client.UploadSettling(id) {
		return false
	}
	logger.Log("[WebDAV] ⏳ '%s' aún se está validando en O2: la descarga espera a que termine...", name)
	start := time.Now()
	ok := mfs.client.WaitUploadSettled(ctx, id)
	if ok {
		logger.Log("[WebDAV] ✔ '%s' validado tras %s: se descarga", name, time.Since(start).Round(time.Second))
	} else if ctx.Err() != nil {
		logger.Log("[WebDAV] ⚠️ El cliente dejó de esperar a '%s' (%s en validación)", name, time.Since(start).Round(time.Second))
	}
	return ok
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

func (mfs *MovistarFS) RemoveAll(ctx context.Context, name string) (err error) {
	mfs.setLastRemoveErr(nil)
	defer func() { mfs.setLastRemoveErr(err) }()
	cleanPath := path.Clean("/" + name)
	pathKey := cacheKey(cleanPath)
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
		if errors.Is(err, os.ErrNotExist) || isControlOrLockFile(cleanPath) {
			mfs.cacheMu.Lock()
			delete(mfs.uploadCache, pathKey)
			mfs.cacheMu.Unlock()
			return nil // WebDAV idempotencia / lock file graceful
		}
		return err
	}

	if isFolder && folderInfo != nil {
		logger.Log("[WebDAV] 🗑 Eliminando carpeta: '%s' (ID: %d)...", folderInfo.Name, folderInfo.ID)
		if folderInfo.ID < 0 {
			logger.Log("[WebDAV] ℹ️ Eliminando carpeta virtual '%s' (ID: %d) localmente (sin SAPI)", folderInfo.Name, folderInfo.ID)
		} else {
			var delErr error
			for attempt := 0; attempt < 3; attempt++ {
				delErr = mfs.client.DeleteFolder(folderInfo.ID, false)
				if delErr == nil || !errors.Is(delErr, sapi.ErrSessionRefreshTransient) {
					break
				}
				wait := time.Duration(1<<attempt) * 2 * time.Second
				logger.Log("[WebDAV] ⏳ Renovación transitoria al borrar carpeta '%s'; reintentando en %v (%d/3)...", folderInfo.Name, wait, attempt+1)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(wait):
				}
			}
			if delErr != nil {
				if isControlOrLockFile(cleanPath) {
					logger.Log("[WebDAV] ℹ️ Carpeta de control/lock '%s' (ID: %d) gestionada de forma tolerante: %v", folderInfo.Name, folderInfo.ID, delErr)
				} else {
					logger.Log("[WebDAV] ❌ Error eliminando carpeta '%s' (ID: %d): %v", folderInfo.Name, folderInfo.ID, delErr)
					return delErr
				}
			}
		}
		if ms := mfs.MetadataStore(); ms != nil {
			logMetaErr("DeleteFolder", ms.DeleteFolder(folderInfo.ID))
			logMetaErr("DeleteVirtualEmptyFilesInFolder", ms.DeleteVirtualEmptyFilesInFolder(folderInfo.ID))
			logMetaErr("UpdateFolderModificationDate", ms.UpdateFolderModificationDate(folderInfo.ParentID, time.Now().UTC().Format("20060102T150405Z")))
			if !isControlOrLockFile(cleanPath) && mfs.emptyFileManager != nil {
				mfs.emptyFileManager.ScheduleSave()
			}
		}
		mfs.cacheMu.Lock()
		delete(mfs.createdFolders, pathKey)
		delete(mfs.uploadCache, pathKey)
		mfs.deletedTombstones[pathKey] = time.Now().Add(15 * time.Second)
		mfs.cacheMu.Unlock()
		mfs.InvalidateCache(folderInfo.ParentID)
		mfs.InvalidateCache(folderInfo.ID)
		return nil
	}

	if !isFolder && mediaInfo != nil {
		if strings.HasPrefix(mediaInfo.ID, "virtual:") {
			logger.Log("[WebDAV] 🗑 Eliminando archivo vacío emulado: '%s'...", mediaInfo.Name)
			if ms := mfs.MetadataStore(); ms != nil {
				logMetaErr("DeleteVirtualEmptyFile", ms.DeleteVirtualEmptyFile(mediaInfo.FolderID, mediaInfo.Name))
				logMetaErr("UpdateFolderModificationDate", ms.UpdateFolderModificationDate(mediaInfo.FolderID, time.Now().UTC().Format("20060102T150405Z")))
				if !isControlOrLockFile(cleanPath) && mfs.emptyFileManager != nil {
					mfs.emptyFileManager.ScheduleSave()
				}
			}
			mfs.cacheMu.Lock()
			delete(mfs.uploadCache, pathKey)
			mfs.deletedTombstones[pathKey] = time.Now().Add(15 * time.Second)
			mfs.cacheMu.Unlock()
			mfs.InvalidateCache(mediaInfo.FolderID)
			return nil
		}

		logger.Log("[WebDAV] 🗑 Eliminando archivo: '%s' (ID: %s)...", mediaInfo.Name, mediaInfo.ID)
		if mediaInfo.ID != "" {
			// Si el archivo fue subido hace apenas unos segundos, Telefónica aún está procesándolo
			// de forma asíncrona (MED-1017). Para evitar reintentos continuos que activen el rate-limit
			// de AWS CloudFront WAF, esperamos de forma proactiva a que cumpla una ventana mínima (6s)
			// desde la subida antes de lanzar la petición de borrado a SAPI.
			mfs.cacheMu.RLock()
			var waitTime time.Duration
			if mem, ok := mfs.uploadCache[pathKey]; ok && !mem.CachedAt.IsZero() {
				if elapsed := time.Since(mem.CachedAt); elapsed < 6*time.Second {
					waitTime = 6*time.Second - elapsed
				}
			}
			mfs.cacheMu.RUnlock()

			if waitTime > 0 {
				logger.Log("[WebDAV] ⏳ Archivo '%s' recién subido. Esperando %v antes de borrar para permitir validación en nube...", mediaInfo.Name, waitTime.Round(time.Millisecond))
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(waitTime):
				}
			}

			var delErr error
			for attempt := 0; attempt < 3; attempt++ {
				delErr = mfs.client.DeleteFile(mediaInfo.ID, mediaInfo.Name)
				if delErr == nil || !errors.Is(delErr, sapi.ErrSessionRefreshTransient) {
					break
				}
				wait := time.Duration(1<<attempt) * 2 * time.Second
				logger.Log("[WebDAV] ⏳ Renovación transitoria al borrar '%s'; reintentando en %v (%d/3)...", mediaInfo.Name, wait, attempt+1)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(wait):
				}
			}
			if delErr != nil {
				if isTolerantDelete(cleanPath, delErr) {
					logger.Log("[WebDAV] ℹ️ Fichero '%s' (ID: %s) gestionado de forma tolerante ante error de borrado SAPI: %v", mediaInfo.Name, mediaInfo.ID, delErr)
				} else {
					logger.Log("[WebDAV] ❌ Error eliminando archivo '%s' (ID: %s): %v", mediaInfo.Name, mediaInfo.ID, delErr)
					return fs.ErrPermission
				}
			}
		}
		if ms := mfs.MetadataStore(); ms != nil {
			logMetaErr("DeleteMedia", ms.DeleteMedia(mediaInfo.ID))
			logMetaErr("UpdateFolderModificationDate", ms.UpdateFolderModificationDate(mediaInfo.FolderID, time.Now().UTC().Format("20060102T150405Z")))
		}
		mfs.cacheMu.Lock()
		delete(mfs.uploadCache, pathKey)
		mfs.deletedTombstones[pathKey] = time.Now().Add(15 * time.Second)
		mfs.cacheMu.Unlock()
		mfs.InvalidateCache(mediaInfo.FolderID)
		return nil
	}

	return nil
}

func (mfs *MovistarFS) Rename(ctx context.Context, oldName, newName string) error {
	oldClean := path.Clean("/" + oldName)
	newClean := path.Clean("/" + newName)
	oldKey := cacheKey(oldClean)
	newKey := cacheKey(newClean)

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
		err := mfs.client.UpdateFolderItem(*oldFolder, newBaseName, parentFolder.ID)
		if err != nil {
			return err
		}
		if ms := mfs.MetadataStore(); ms != nil {
			logMetaErr("MoveFolder", ms.MoveFolder(oldFolder.ID, parentFolder.ID, newBaseName))
			nowMod := time.Now().UTC().Format("20060102T150405Z")
			logMetaErr("UpdateFolderModificationDate old", ms.UpdateFolderModificationDate(oldFolder.ParentID, nowMod))
			logMetaErr("UpdateFolderModificationDate parent", ms.UpdateFolderModificationDate(parentFolder.ID, nowMod))
		}
		mfs.cacheMu.Lock()
		delete(mfs.createdFolders, oldKey)
		delete(mfs.deletedTombstones, newKey)
		mfs.deletedTombstones[oldKey] = time.Now().Add(15 * time.Second)
		mfs.cacheMu.Unlock()
		mfs.InvalidateCache(oldFolder.ParentID)
		mfs.InvalidateCache(parentFolder.ID)
		return nil
	}

	if !isF && oldMedia != nil {
		if strings.HasPrefix(oldMedia.ID, "virtual:") {
			var destToDelete *sapi.MediaItem
			_, destMedia, destIsFolder, destErr := mfs.resolvePath(newClean)
			if destErr == nil && !destIsFolder && destMedia != nil && destMedia.ID != "" && isSameEntryName(destMedia.Name, newBaseName) && destMedia.ID != oldMedia.ID {
				destToDelete = destMedia
			}
			if destToDelete != nil {
				if strings.HasPrefix(destToDelete.ID, "virtual:") {
					if ms := mfs.MetadataStore(); ms != nil {
						_ = ms.DeleteVirtualEmptyFile(destToDelete.FolderID, destToDelete.Name)
					}
				} else {
					_ = mfs.client.DeleteFile(destToDelete.ID, destToDelete.Name)
					if ms := mfs.MetadataStore(); ms != nil {
						_ = ms.DeleteMedia(destToDelete.ID)
					}
				}
			}
			nowMod := time.Now().UTC().Format("20060102T150405Z")
			if ms := mfs.MetadataStore(); ms != nil {
				_ = ms.DeleteVirtualEmptyFile(oldMedia.FolderID, oldMedia.Name)
				_ = ms.UpsertVirtualEmptyFile(parentFolder.ID, newBaseName, nowMod)
				_ = ms.UpdateFolderModificationDate(oldMedia.FolderID, nowMod)
				_ = ms.UpdateFolderModificationDate(parentFolder.ID, nowMod)
			}
			mfs.cacheMu.Lock()
			delete(mfs.uploadCache, oldKey)
			delete(mfs.deletedTombstones, newKey)
			mfs.deletedTombstones[oldKey] = time.Now().Add(15 * time.Second)
			mfs.uploadCache[newKey] = &MemoryFile{
				ID:       fmt.Sprintf("virtual:%d:%s", parentFolder.ID, newBaseName),
				Name:     newBaseName,
				FolderID: parentFolder.ID,
				Content:  []byte{},
				Size:     0,
				ModTime:  time.Now(),
				CachedAt: time.Now(),
			}
			mfs.cacheMu.Unlock()
			mfs.InvalidateCache(oldMedia.FolderID)
			mfs.InvalidateCache(parentFolder.ID)
			if mfs.emptyFileManager != nil {
				mfs.emptyFileManager.ScheduleSave()
			}
			return nil
		}

		// Comprobar si el destino ya existe en Movistar Cloud para borrarlo SOLO tras completar el movimiento con éxito
		var destToDelete *sapi.MediaItem
		_, destMedia, destIsFolder, destErr := mfs.resolvePath(newClean)
		// Solo es «el destino» un archivo con ese nombre exacto y distinto del que se
		// mueve: O2 distingue mayúsculas, y en un cambio solo de mayúsculas la búsqueda
		// sin distinguirlas encontraría el propio origen (y se borraría tras moverlo).
		if destErr == nil && !destIsFolder && destMedia != nil && destMedia.ID != "" && isSameEntryName(destMedia.Name, newBaseName) && destMedia.ID != oldMedia.ID {
			destToDelete = destMedia
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
				mem, inCache := mfs.uploadCache[oldKey]
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
				// Borrar destino previo sólo tras completar la re-subida con éxito
				if destToDelete != nil {
					logger.Log("[WebDAV] ℹ Eliminando archivo destino previo '%s' (ID: %s) tras re-subida exitosa...", newClean, destToDelete.ID)
					_ = mfs.client.DeleteFile(destToDelete.ID, destToDelete.Name)
					if ms := mfs.MetadataStore(); ms != nil {
						logMetaErr("DeleteMedia dest", ms.DeleteMedia(destToDelete.ID))
					}
				}
				if ms := mfs.MetadataStore(); ms != nil {
					_ = ms.DeleteVirtualEmptyFile(parentFolder.ID, newBaseName)
					logMetaErr("DeleteMedia old", ms.DeleteMedia(oldMedia.ID))
					logMetaErr("UpsertMedia reuploaded", ms.UpsertMedia(sapi.MediaItem{
						ID:               reuploaded.ID,
						Name:             newBaseName,
						Size:             reuploaded.Size,
						URL:              reuploaded.URL,
						FolderID:         parentFolder.ID,
						ModificationDate: sapi.FlexDate(modTime.UTC().Format("20060102T150405Z")),
					}))
				}
			} else {
				if err := mfs.client.UpdateMediaItem(*oldMedia, newBaseName, parentFolder.ID); err != nil {
					return err
				}
				// Borrar destino previo sólo tras completar UpdateMedia con éxito
				if destToDelete != nil {
					logger.Log("[WebDAV] ℹ Eliminando archivo destino previo '%s' (ID: %s) tras UpdateMedia exitoso...", newClean, destToDelete.ID)
					_ = mfs.client.DeleteFile(destToDelete.ID, destToDelete.Name)
					if ms := mfs.MetadataStore(); ms != nil {
						logMetaErr("DeleteMedia dest", ms.DeleteMedia(destToDelete.ID))
					}
				}
				if ms := mfs.MetadataStore(); ms != nil {
					_ = ms.DeleteVirtualEmptyFile(parentFolder.ID, newBaseName)
					logMetaErr("MoveMedia", ms.MoveMedia(oldMedia.ID, parentFolder.ID, newBaseName))
					nowMod := time.Now().UTC().Format("20060102T150405Z")
					logMetaErr("UpdateFolderModificationDate old", ms.UpdateFolderModificationDate(oldMedia.FolderID, nowMod))
					logMetaErr("UpdateFolderModificationDate parent", ms.UpdateFolderModificationDate(parentFolder.ID, nowMod))
				}
			}
		}
		mfs.cacheMu.Lock()
		delete(mfs.deletedTombstones, newKey)
		mfs.deletedTombstones[oldKey] = time.Now().Add(15 * time.Second)
		if reuploaded != nil {
			// El archivo nuevo tiene otro ID y SAPI tarda en listarlo: se registra como
			// una subida recién hecha para que se vea y se lea en seguida.
			delete(mfs.uploadCache, oldKey)
			mfs.uploadCache[newKey] = reuploaded
		} else if mem, ok := mfs.uploadCache[oldKey]; ok {
			delete(mfs.uploadCache, oldKey)
			mem.Name = newBaseName
			mem.FolderID = parentFolder.ID
			mfs.uploadCache[newKey] = mem
		}
		mfs.cacheMu.Unlock()
		mfs.InvalidateCache(oldMedia.FolderID)
		mfs.InvalidateCache(parentFolder.ID)
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
		failed := !mem.URLLookupFailedAt.IsZero()
		mfs.cacheMu.RUnlock()
		if confirmed || failed || mem.ID == "" || time.Now().After(deadline) {
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

		if oldMedia.Size <= int64(GetUploadRAMBufferMaxBytes()) {
			buf := bytes.NewBuffer(make([]byte, 0, oldMedia.Size))
			if n, err = io.Copy(io.MultiWriter(buf, hasher), stream); err != nil {
				_ = stream.Close()
				return nil, fmt.Errorf("moveByReupload buffer error: %w", err)
			}
			_ = stream.Close()
			reader = bytes.NewReader(buf.Bytes())
			if oldMedia.Size <= 256*1024 {
				cachedContent = buf.Bytes()
			}
		} else {
			tmp, err := os.CreateTemp(getUploadTempDir(), "webdav_reupload_*")
			if err != nil {
				_ = stream.Close()
				return nil, fmt.Errorf("moveByReupload temp file error: %w", err)
			}
			tmpPath := tmp.Name()
			defer func() {
				_ = tmp.Close()
				_ = os.Remove(tmpPath)
			}()

			if n, err = io.Copy(io.MultiWriter(tmp, hasher), stream); err != nil {
				_ = stream.Close()
				return nil, fmt.Errorf("moveByReupload spool error: %w", err)
			}
			_ = stream.Close()
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
	if err := mfs.client.DeleteFile(oldMedia.ID, oldMedia.Name); err != nil {
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

	folderInfo, mediaInfo, isFolder, err := mfs.resolvePath(cleanPath)
	if err != nil {
		return nil, err
	}

	if isFolder {
		var modTime time.Time
		if folderInfo != nil && string(folderInfo.ModificationDate) != "" {
			if t := parseTime(folderInfo.ModificationDate); !t.IsZero() {
				modTime = t
			}
		}
		return &VirtualFileInfo{
			name:    path.Base(cleanPath),
			size:    0,
			mode:    os.ModeDir | 0755,
			modTime: modTime,
			isDir:   true,
		}, nil
	}

	mfs.cacheMu.RLock()
	if mem, ok := mfs.recentUpload(cacheKey(cleanPath), mediaInfo); ok {
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
	existingItem   *sapi.MediaItem // el archivo que se sobrescribe (para reenviar su fecha de creación)
	ctx            context.Context // petición del cliente: si se va, la subida a O2 se aborta
	buf            *bytes.Buffer
	tmpFile        *os.File
	size           int64
	closed         bool
	semAcquired    bool
	spoolOnly      bool // el hueco de RAM estaba ocupado: este cuerpo va directo al spool en disco
	failed         bool
	modTime        time.Time // fecha pedida por el cliente (X-OC-Mtime); cero = ahora
	guard          *uploadGuard
	pathUnlock     func()
	streamPW       *io.PipeWriter
	streamDone     chan streamUploadResult
	streamFailed   bool
	// pending: subida en flujo cortada a medias; el reintento desde disco continúa
	// sobre ese ID (perfil desktop, como la app oficial) en lugar de crear otro.
	pending *sapi.UploadInterruptedError
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

// upload sube el contenido: sobrescribe el archivo existente (con su fecha de
// creación, como la app) o crea uno nuevo.
func (w *WebDAVFileWriter) upload(body io.Reader, size int64, mimeType string) (*sapi.MediaItem, error) {
	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if id := w.sapiExistingID(); id != "" && size == 0 {
		return w.replaceWithEmpty(ctx, id, body, mimeType)
	}
	if id := w.sapiExistingID(); id != "" && w.existingItem != nil && w.existingItem.ID == id {
		return w.mfs.client.UploadFileReplacingCtx(ctx, *w.existingItem, w.name, body, size, w.parentFolderID, mimeType, w.modTime)
	}
	return w.mfs.client.UploadFileToFolderCtx(ctx, w.name, body, size, w.parentFolderID, mimeType, w.sapiExistingID(), w.modTime)
}

// replaceWithEmpty vacía un archivo que tenía contenido. O2 no admite sobrescribir un
// archivo con 0 bytes: responde MED-1049 «Wrong byte uploaded» al envío vacío
// (comprobado el 20/11/2026), aunque sí crea archivos nuevos de 0 bytes. Se sube uno
// nuevo vacío y, cuando está, se manda el anterior a la papelera, como hace la app al
// cambiar la extensión de un archivo.
func (w *WebDAVFileWriter) replaceWithEmpty(ctx context.Context, oldID string, body io.Reader, mimeType string) (*sapi.MediaItem, error) {
	media, err := w.mfs.client.UploadFileToFolderCtx(ctx, w.name, body, 0, w.parentFolderID, mimeType, "", w.modTime)
	if err != nil {
		return nil, err
	}
	derr := w.mfs.client.DeleteFile(oldID, w.name)
	if derr != nil {
		logger.Log("[WebDAV] ⚠️ '%s' se vació subiendo uno nuevo (ID %s), pero el anterior (ID %s) no se pudo mandar a la papelera: %v", w.name, media.ID, oldID, derr)
	} else {
		logger.Log("[WebDAV] 🗑 '%s' vaciado: nuevo de 0 bytes (ID %s); el anterior (ID %s), a la papelera.", w.name, media.ID, oldID)
	}
	if ms := w.mfs.MetadataStore(); ms != nil && derr == nil {
		logMetaErr("DeleteMedia", ms.DeleteMedia(oldID))
	}
	return media, nil
}

// sapiExistingID devuelve el ID real de SAPI que se puede sobrescribir. Los IDs
// virtual:<folder>:<name> existen solo en SQLite/memoria; enviarlos a SAPI provoca
// PAPI-0000 / HTTP 405 al convertir un archivo vacío emulado en uno con contenido.
func (w *WebDAVFileWriter) sapiExistingID() string {
	if strings.HasPrefix(w.existingID, "virtual:") {
		return ""
	}
	return w.existingID
}

// streamUploadsEnabled indica si se sube en flujo directo (por defecto sí; con
// WEBDAV_STREAM_UPLOAD=false, no): el contenido se reenvía a O2 mientras llega, como
// la app oficial, y el cliente recibe la respuesta poco después del último byte. Sin
// flujo, el cuerpo se recibe entero (RAM o spool) y el cliente espera en silencio a
// que el proxy lo suba: en archivos grandes, más que el tiempo de espera de rclone
// (5 min). El envío en flujo no retiene el hueco de la API (ver sapi.DoRequest), y
// nunca se espera turno ni hueco de RAM antes de leer un cuerpo (tryAcquireUploadTurn,
// Write), así que varias subidas a la vez no se bloquean entre sí.
func (mfs *MovistarFS) streamUploads() bool { return config.StreamUploads() }

// tryAcquireUploadTurn intenta coger, sin esperar, el turno de subida a SAPI
// (uploadSem + globalTransferSem). Devuelve false si alguno está ocupado.
func tryAcquireUploadTurn() bool {
	select {
	case uploadSem <- struct{}{}:
	default:
		return false
	}
	select {
	case globalTransferSem <- struct{}{}:
		return true
	default:
		<-uploadSem
		return false
	}
}

func (w *WebDAVFileWriter) startStreamUpload(initialBytes []byte) {
	if !w.mfs.streamUploads() {
		return
	}
	if w.guard == nil || w.guard.expected <= 0 || w.streamDone != nil || w.parentFolderID < 0 {
		return
	}
	// Solo se sube en flujo si el turno de subida está libre AHORA. Si otro fichero
	// lo tiene, esperar aquí bloquearía la lectura de este cuerpo (el io.Pipe no
	// avanza) y, con clientes que multiplexan varias subidas en una conexión
	// (restic -> rclone por HTTP/2), el que tiene el turno puede estar esperando
	// precisamente datos que van detrás de este cuerpo: interbloqueo hasta que el
	// cliente corta por timeout. En ese caso el cuerpo se guarda entero en el
	// spool y se sube en Close() cuando llegue el turno.
	if !tryAcquireUploadTurn() {
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
		// el turno ya se cogió en tryAcquireUploadTurn
		defer func() {
			<-globalTransferSem
			<-uploadSem
		}()
		body, tr := trackUpload(w.path, expectedSize, pr)
		media, err := w.upload(body, expectedSize, mimeType)
		tr.Done()
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
		res := <-w.streamDone
		w.streamDone = nil
		w.mfs.client.DiscardInterruptedUpload(res.err, w.name)
	}
	if w.pending != nil {
		w.mfs.client.DiscardInterruptedUpload(w.pending, w.name)
		w.pending = nil
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
	if !w.semAcquired && !w.spoolOnly {
		// No esperar nunca aquí: mientras se espera no se lee el cuerpo del cliente,
		// y quien tiene el hueco puede depender de que este cuerpo avance (ver
		// startStreamUpload). Si el hueco de RAM está ocupado, este cuerpo va al
		// spool en disco. Los ficheros de control se emulan en memoria y son
		// pequeños: siguen en RAM aunque no haya hueco.
		select {
		case uploadBufferSem <- struct{}{}:
			w.semAcquired = true
		default:
			if !(w.parentFolderID < 0 || isControlOrLockFile(w.path)) {
				w.spoolOnly = true
			}
		}
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
	if w.tmpFile == nil && (w.spoolOnly || w.buf.Len()+len(p) > rambufMax || (w.guard != nil && w.guard.expected > int64(rambufMax))) {
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

	// Se emulan en SQLite y memoria, sin subir a la nube: los archivos en carpetas
	// virtuales heredadas, los vacíos de bloqueo y control (viven segundos) y, si no se
	// suben los vacíos de verdad (realEmptyFiles), todos los de 0 bytes.
	if w.parentFolderID < 0 || (w.size == 0 && (isControlOrLockFile(w.path) || !w.mfs.realEmptyFiles())) {
		w.cleanupTemp()
		now := time.Now()
		modTime := now
		if !w.modTime.IsZero() {
			modTime = w.modTime
		}
		modTimeStr := modTime.UTC().Format("20060102T150405Z")

		virtualID := fmt.Sprintf("virtual:%d:%s", w.parentFolderID, w.name)
		var cachedContent []byte
		if w.buf != nil {
			cachedContent = w.buf.Bytes()
		}

		if w.size == 0 {
			if ms := w.mfs.MetadataStore(); ms != nil {
				logMetaErr("UpsertVirtualEmptyFile", ms.UpsertVirtualEmptyFile(w.parentFolderID, w.name, modTimeStr))
				logMetaErr("UpdateFolderModificationDate", ms.UpdateFolderModificationDate(w.parentFolderID, modTimeStr))
				if w.existingID != "" && !strings.HasPrefix(w.existingID, "virtual:") {
					_ = w.mfs.client.DeleteFile(w.existingID)
					_ = ms.DeleteMedia(w.existingID)
				}
			}
		} else {
			// Archivo de control con contenido (e.g. v1.cformat)
			item := sapi.MediaItem{
				ID:               virtualID,
				Name:             w.name,
				Size:             w.size,
				FolderID:         w.parentFolderID,
				CreationDate:     sapi.FlexDate(modTimeStr),
				ModificationDate: sapi.FlexDate(modTimeStr),
			}
			if ms := w.mfs.MetadataStore(); ms != nil {
				logMetaErr("UpsertMedia", ms.UpsertMedia(item))
				logMetaErr("UpdateFolderModificationDate", ms.UpdateFolderModificationDate(w.parentFolderID, modTimeStr))
			}
		}

		pathKey := cacheKey(w.path)
		w.mfs.cacheMu.Lock()
		delete(w.mfs.deletedTombstones, pathKey)
		w.mfs.uploadCache[pathKey] = &MemoryFile{
			ID:       virtualID,
			Name:     w.name,
			FolderID: w.parentFolderID,
			Content:  cachedContent,
			Size:     w.size,
			ModTime:  modTime,
			CachedAt: now,
		}
		w.mfs.cacheMu.Unlock()
		w.mfs.InvalidateCache(w.parentFolderID)
		if w.size == 0 && !isControlOrLockFile(w.path) && w.mfs.emptyFileManager != nil {
			w.mfs.emptyFileManager.ScheduleSave()
		}
		if w.size == 0 {
			logger.Log("[WebDAV] Archivo vacío emulado: '%s'", w.path)
		} else {
			logger.Log("[WebDAV] Archivo de control emulado en memoria: '%s' (%d bytes)", w.path, w.size)
		}
		return nil
	}

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
		} else if errors.Is(res.err, sapi.ErrFileTooLarge) {
			w.cleanupTemp()
			if w.guard != nil {
				w.guard.tooLarge.Store(true)
			}
			logger.Log("[WebDAV] ❌ Subida rechazada por Movistar/O2 Cloud (MED-1020 tamaño máximo excedido): '%s'", w.name)
			return ErrUploadTooLarge
		} else if errors.Is(res.err, sapi.ErrMediaUnsupported) && !config.UnsupportedAsFile() {
			w.cleanupTemp()
			return w.rejectUnsupported()
		} else {
			var ie *sapi.UploadInterruptedError
			if errors.As(res.err, &ie) && ie.MediaID != "" {
				w.pending = ie
			}
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
			var bufBytes []byte
			if w.buf != nil {
				bufBytes = w.buf.Bytes() // nil: archivo de 0 bytes sin ninguna escritura
			}
			reader = bytes.NewReader(bufBytes)
			if w.size <= 256*1024 {
				cachedContent = bufBytes
			}
		}

		mimeType := mime.TypeByExtension(filepath.Ext(w.name))
		body, tr := trackUpload(w.path, w.size, reader)
		if w.pending != nil && w.tmpFile != nil {
			ctx := w.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			media, err = w.mfs.client.UploadFileContinueCtx(ctx, w.pending.MediaID, w.pending.New, w.name, body, w.size, w.parentFolderID, mimeType, w.modTime)
		} else {
			if w.pending != nil {
				w.mfs.client.DiscardInterruptedUpload(w.pending, w.name)
			}
			media, err = w.upload(body, w.size, mimeType)
		}
		w.pending = nil
		tr.Done()
		<-globalTransferSem
		<-uploadSem
		if err != nil {
			if errors.Is(err, sapi.ErrFileTooLarge) {
				if w.guard != nil {
					w.guard.tooLarge.Store(true)
				}
				logger.Log("[WebDAV] ❌ Subida rechazada por Movistar/O2 Cloud (MED-1020 tamaño máximo excedido): '%s'", w.name)
				return ErrUploadTooLarge
			}
			if errors.Is(err, sapi.ErrMediaUnsupported) {
				return w.rejectUnsupported()
			}
			logger.Log("[WebDAV] ❌ Error al subir '%s': %v", w.name, err)
			w.guard.setRetryLater(err)
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
	pathKey := cacheKey(w.path)
	delete(w.mfs.deletedTombstones, pathKey)
	w.mfs.uploadCache[pathKey] = &MemoryFile{
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

	if ms := w.mfs.MetadataStore(); ms != nil && media != nil {
		_ = ms.DeleteVirtualEmptyFile(w.parentFolderID, w.name)
		if !isControlOrLockFile(w.path) && w.mfs.emptyFileManager != nil {
			w.mfs.emptyFileManager.ScheduleSave()
		}
		logMetaErr("UpsertMedia", ms.UpsertMedia(*media))
		logMetaErr("UpdateFolderModificationDate", ms.UpdateFolderModificationDate(w.parentFolderID, time.Now().UTC().Format("20060102T150405Z")))
		if strings.EqualFold(os.Getenv("DEDUPE_ON_OVERWRITE"), "true") && w.parentFolderID > 0 {
			if dups, err := ms.GetDuplicateMediaInFolder(w.parentFolderID, w.name); err == nil && len(dups) > 1 {
				for _, oldDup := range dups[1:] {
					logger.Log("[WebDAV] 🧹 DEDUPE_ON_OVERWRITE: eliminando duplicado antiguo '%s' (ID: %s)", oldDup.Name, oldDup.ID)
					_ = w.mfs.client.DeleteFile(oldDup.ID)
					_ = ms.DeleteMedia(oldDup.ID)
				}
			}
		}
	}
	w.mfs.InvalidateCache(w.parentFolderID)

	return nil
}

func (w *WebDAVFileWriter) Read(p []byte) (n int, err error)             { return 0, io.EOF }
func (w *WebDAVFileWriter) Seek(offset int64, whence int) (int64, error) { return 0, nil }
func (w *WebDAVFileWriter) Readdir(count int) ([]os.FileInfo, error)     { return nil, fs.ErrInvalid }
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
	mfs        *MovistarFS
	path       string
	item       sapi.MediaItem
	pos        int64
	stream     io.ReadCloser
	cacheBuf   *bytes.Buffer
	streamMu   sync.Mutex
	resolveURL func() string // si no es nil, obtiene la URL de descarga antes de la primera lectura
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
		r.streamMu.Lock()
		resolve := r.resolveURL
		r.resolveURL = nil
		r.streamMu.Unlock()
		if resolve != nil {
			if u := resolve(); u != "" {
				r.item.URL = u
			}
		}
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
			if pos == 0 && r.item.Size > 0 && r.item.Size <= 256*1024 {
				r.cacheBuf = bytes.NewBuffer(make([]byte, 0, r.item.Size))
			} else {
				r.cacheBuf = nil
			}
		}
		r.streamMu.Unlock()
	}

	n, err = st.Read(p)
	if n > 0 {
		r.streamMu.Lock()
		r.pos += int64(n)
		if r.cacheBuf != nil {
			_, _ = r.cacheBuf.Write(p[:n])
			if r.pos == r.item.Size && int64(r.cacheBuf.Len()) == r.item.Size {
				data := append([]byte(nil), r.cacheBuf.Bytes()...)
				r.cacheBuf = nil
				now := time.Now()
				r.mfs.cacheMu.Lock()
				readKey := cacheKey(r.path)
				if _, isTomb := r.mfs.deletedTombstones[readKey]; !isTomb {
					r.mfs.uploadCache[readKey] = &MemoryFile{
						ID:       r.item.ID,
						Name:     r.item.Name,
						FolderID: r.item.FolderID,
						Content:  data,
						Size:     r.item.Size,
						ModTime:  parseTime(r.item.ModificationDate),
						CachedAt: now,
						URL:      r.item.URL,
					}
				}
				for pth, mem := range r.mfs.uploadCache {
					if now.Sub(mem.CachedAt) > 15*time.Minute {
						delete(r.mfs.uploadCache, pth)
					}
				}
				r.mfs.cacheMu.Unlock()
			}
		}
		r.streamMu.Unlock()
	}
	return n, err
}

func (r *WebDAVFileReader) Write(p []byte) (n int, err error) { return 0, fs.ErrPermission }

func (r *WebDAVFileReader) Close() error {
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	r.cacheBuf = nil
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
		r.cacheBuf = nil
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

func (m *WebDAVMemoryReader) Read(p []byte) (n int, err error)  { return m.reader.Read(p) }
func (m *WebDAVMemoryReader) Write(p []byte) (n int, err error) { return 0, fs.ErrPermission }
func (m *WebDAVMemoryReader) Close() error                      { return nil }
func (m *WebDAVMemoryReader) Seek(offset int64, whence int) (int64, error) {
	return m.reader.Seek(offset, whence)
}
func (m *WebDAVMemoryReader) Readdir(count int) ([]os.FileInfo, error) { return nil, fs.ErrInvalid }
func (m *WebDAVMemoryReader) Stat() (os.FileInfo, error) {
	return &VirtualFileInfo{name: m.mem.Name, size: m.mem.Size, mode: 0644, modTime: m.mem.ModTime}, nil
}

func (mfs *MovistarFS) extractSeqIDFromSeqMapping(seqItem sapi.MediaItem) int64 {
	if mfs == nil || mfs.client == nil || seqItem.ID == "" || seqItem.Size <= 0 {
		return 1
	}
	rc, err := mfs.client.DownloadFileStream(seqItem)
	if err != nil {
		logger.Log("[WebDAV] ⚠️ No se pudo descargar seq_mapping para auto-reparar @writer: %v", err)
		return 1
	}
	defer rc.Close()

	tmpFile, err := os.CreateTemp(getUploadTempDir(), "hb_seq_mapping_*.db")
	if err != nil {
		return 1
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()

	if _, err := io.Copy(tmpFile, io.LimitReader(rc, 64<<20)); err != nil {
		return 1
	}
	_ = tmpFile.Close()

	maxSeq, err := cache.ExtractMaxSeqIDFromSQLiteFile(tmpPath)
	if err != nil {
		logger.Log("[WebDAV] ⚠️ No se pudo leer seq_id desde seq_mapping (%v); usando seq_id=1", err)
		return 1
	}
	return maxSeq
}

// Hyper Backup guarda la versión de la copia en el nombre de un vacío de
// Control/@writer, v1.N.-1.0.0.none.none.cinfo, y la misma N en el nombre de
// Config/version_info.db.N. N no es el número de copias (tras la 4.ª puede ser 5):
// al terminar una copia sube version_info.db.N, escribe v1.N…cinfo y borra los de N-1.
var (
	hbStateCinfoRe  = regexp.MustCompile(`^v1\.(\d+)\.-1\.0\.0\.none\.none\.cinfo$`)
	hbVersionInfoRe = regexp.MustCompile(`^version_info\.db\.(\d+)$`)
)

// hbV1Checked: carpetas @writer cuyo v1.1 ya se comprobó contra Config y es correcto
// (tarea con una sola copia). Evita listar Config en cada PROPFIND de @writer.
var hbV1Checked sync.Map // folderID → time.Time

// hbStateSeq devuelve N si name es el .cinfo de estado v1.N.-1.0.0.none.none.cinfo.
func hbStateSeq(name string) (int64, bool) {
	m := hbStateCinfoRe.FindStringSubmatch(strings.ToLower(name))
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	return n, err == nil && n > 0
}

// hbVersionFromNames deduce la versión confirmada de los nombres de Config: la N de
// version_info.db.N. Si hay varias (una copia cortada después de subir la nueva y
// antes de borrar la anterior), la menor, que es la última confirmada.
func hbVersionFromNames(names []string) (int64, bool) {
	var best int64
	for _, name := range names {
		m := hbVersionInfoRe.FindStringSubmatch(strings.ToLower(name))
		if m == nil {
			continue
		}
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > 0 && (best == 0 || n < best) {
			best = n
		}
	}
	return best, best > 0
}

// hyperBackupVersionFromConfig lee de la nube los nombres de <tarea>.hbk/Config para
// deducir la versión confirmada (hbVersionFromNames).
func (mfs *MovistarFS) hyperBackupVersionFromConfig(writerDir string) (int64, bool) {
	if mfs.client == nil {
		return 0, false
	}
	cfg, _, isFolder, err := mfs.resolvePath(path.Join(path.Dir(path.Dir(writerDir)), "Config"))
	if err != nil || !isFolder || cfg == nil {
		return 0, false
	}
	items, err := mfs.client.ListAllMediaInFolder(cfg.ID)
	if err != nil {
		return 0, false
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.Name)
	}
	return hbVersionFromNames(names)
}

// autoHealHyperBackupWriter repone en Control/@writer los vacíos de estado de Hyper
// Backup que falten (v1.cformat y el .cinfo de la versión) y corrige el .cinfo v1.1
// que ponían versiones anteriores del proxy a ciegas. La versión sale, por orden, de
// seq_mapping, de Config/version_info.db.N o, si no hay nada (tarea nueva), 1. Con un
// v1.1 equivocado, Hyper Backup responde «The backup data on the cloud is older than
// the NAS's cache» (ST_CLOUD_NOT_SYNC_OLDER): la nube dice 1 y su caché, N.
func (mfs *MovistarFS) autoHealHyperBackupWriter(dirPath string, folderID int64, media []sapi.MediaItem, vFiles []cache.VirtualEmptyFileRecord) []cache.VirtualEmptyFileRecord {
	clean := path.Clean("/" + dirPath)
	if !strings.HasSuffix(strings.ToLower(clean), "/control/@writer") {
		return vFiles
	}
	ms := mfs.MetadataStore()
	if ms == nil {
		return vFiles
	}

	var seqMappingItem *sapi.MediaItem
	var hasCformat, hasCinfo, otherCinfo bool
	var stateSeqs []int64
	look := func(name string) {
		lower := strings.ToLower(name)
		switch {
		case lower == "v1.cformat":
			hasCformat = true
		case strings.HasSuffix(lower, ".cinfo"):
			hasCinfo = true
			if n, ok := hbStateSeq(lower); ok {
				stateSeqs = append(stateSeqs, n)
			} else {
				otherCinfo = true // fases de una copia en curso
			}
		}
	}
	scan := func(vf []cache.VirtualEmptyFileRecord) {
		hasCformat, hasCinfo, otherCinfo, stateSeqs = false, false, false, nil
		for i := range media {
			if strings.ToLower(media[i].Name) == "seq_mapping" {
				seqMappingItem = &media[i]
			}
			look(media[i].Name)
		}
		for i := range vf {
			look(vf[i].Name)
		}
		mfs.cacheMu.RLock()
		for _, mem := range mfs.uploadCache {
			if mem.FolderID == folderID {
				look(mem.Name)
			}
		}
		mfs.cacheMu.RUnlock()
	}
	// Solo se corrige un v1.1 si es el único .cinfo (ninguna copia en curso): es el
	// que ponía el proxy cuando no encontraba el de verdad.
	suspect := func() bool {
		if otherCinfo || len(stateSeqs) != 1 || stateSeqs[0] != 1 {
			return false
		}
		if t, ok := hbV1Checked.Load(folderID); ok && time.Since(t.(time.Time)) < 30*time.Minute {
			return false
		}
		return true
	}

	scan(vFiles)
	if hasCformat && hasCinfo && !suspect() {
		return vFiles
	}

	unlock := acquireFolderLock(clean)
	defer unlock()

	// Re-verificar dentro del lock por si otra petición concurrente ya lo reparó
	if refreshed, err := ms.ListVirtualEmptyFilesInFolder(folderID); err == nil {
		vFiles = refreshed
		scan(vFiles)
		if hasCformat && hasCinfo && !suspect() {
			return vFiles
		}
	}

	modTimeStr := ""
	if seqMappingItem != nil {
		modTimeStr = string(seqMappingItem.ModificationDate)
	}
	if modTimeStr == "" {
		modTimeStr = time.Now().UTC().Format("20060102T150405Z")
	}
	add := func(name string) bool {
		if err := ms.UpsertVirtualEmptyFile(folderID, name, modTimeStr); err != nil {
			return false
		}
		vFiles = append(vFiles, cache.VirtualEmptyFileRecord{FolderID: folderID, Name: name, ModTime: modTimeStr})
		return true
	}

	healed := false
	if !hasCformat && add("v1.cformat") {
		healed = true
		logger.Log("[WebDAV] 🔧 Auto-reparado fichero de formato Hyper Backup en '%s': 'v1.cformat'", clean)
	}

	seq, source := int64(0), ""
	if !hasCinfo || suspect() {
		if seqMappingItem != nil {
			seq, source = mfs.extractSeqIDFromSeqMapping(*seqMappingItem), "recuperado de seq_mapping"
		}
		if seq <= 1 {
			if n, ok := mfs.hyperBackupVersionFromConfig(clean); ok {
				seq, source = n, "deducido de Config/version_info.db.N"
			}
		}
	}
	switch {
	case !hasCinfo:
		if seq <= 0 {
			seq, source = 1, "por defecto: sin seq_mapping ni version_info.db en Config"
		}
		cinfoName := fmt.Sprintf("v1.%d.-1.0.0.none.none.cinfo", seq)
		if add(cinfoName) {
			healed = true
			logger.Log("[WebDAV] 🔧 Auto-reparado fichero de estado Hyper Backup en '%s': '%s' (seq_id=%d %s)", clean, cinfoName, seq, source)
		}
	case suspect() && seq <= 1:
		hbV1Checked.Store(folderID, time.Now()) // v1.1 correcto: tarea con una sola copia
	case suspect() && seq > 1:
		// Se añade el bueno sin borrar el v1.1: Hyper Backup se queda con el mayor y
		// borra los anteriores al terminar la siguiente copia.
		cinfoName := fmt.Sprintf("v1.%d.-1.0.0.none.none.cinfo", seq)
		if add(cinfoName) {
			healed = true
			logger.Log("[WebDAV] 🔧 Corregido el estado Hyper Backup en '%s': solo había 'v1.1.-1.0.0.none.none.cinfo'; añadido '%s' (seq_id=%d %s)", clean, cinfoName, seq, source)
		}
	}

	if healed && mfs.emptyFileManager != nil {
		if mfs.realEmptyFiles() {
			mfs.emptyFileManager.startMigration() // se sube a la nube como cualquier vacío
		} else {
			mfs.emptyFileManager.ScheduleSave()
		}
	}
	return vFiles
}

type WebDAVDirReader struct {
	noopDeadProps
	mfs      *MovistarFS
	path     string
	folderID int64
	modTime  time.Time
	entries  []os.FileInfo
	readPos  int
	loaded   bool
	mu       sync.Mutex
}

func NewWebDAVDirReader(mfs *MovistarFS, path string, folderID int64, modTimes ...time.Time) *WebDAVDirReader {
	var mt time.Time
	if len(modTimes) > 0 {
		mt = modTimes[0]
	}
	return &WebDAVDirReader{mfs: mfs, path: path, folderID: folderID, modTime: mt}
}

func (d *WebDAVDirReader) Read(p []byte) (n int, err error)  { return 0, io.EOF }
func (d *WebDAVDirReader) Write(p []byte) (n int, err error) { return 0, fs.ErrPermission }
func (d *WebDAVDirReader) Close() error                      { return nil }

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
	mt := d.modTime
	if mt.IsZero() && d.mfs != nil {
		if folderInfo, _, isFolder, err := d.mfs.resolvePath(d.path); err == nil && isFolder && folderInfo != nil && string(folderInfo.ModificationDate) != "" {
			mt = parseTime(folderInfo.ModificationDate)
		}
	}
	return &VirtualFileInfo{name: path.Base(d.path), size: 0, mode: os.ModeDir | 0755, modTime: mt, isDir: true}, nil
}

func (d *WebDAVDirReader) Readdir(count int) ([]os.FileInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.loaded {
		d.mfs.MaybeTriggerSync()
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
		ms := d.mfs.MetadataStore()

		d.mfs.cacheMu.RLock()
		var tombstonesToClear []string
		for _, s := range subs {
			if strings.EqualFold(s.Name, ProxyInternalDir) {
				continue
			}
			childPath := path.Clean(path.Join(d.path, s.Name))
			childKey := cacheKey(childPath)
			if exp, isTomb := d.mfs.deletedTombstones[childKey]; isTomb && now.Before(exp) {
				if ms != nil {
					tombstonesToClear = append(tombstonesToClear, childKey)
				} else {
					continue
				}
			}
			if !seen[s.Name] && !isOSJunk(s.Name) && !isDuplicatiTestPath(s.Name) {
				seen[s.Name] = true
				list = append(list, &VirtualFileInfo{name: s.Name, size: 0, mode: os.ModeDir | 0755, modTime: parseTime(s.ModificationDate), isDir: true})
			}
		}

		for _, m := range media {
			childPath := path.Clean(path.Join(d.path, m.Name))
			childKey := cacheKey(childPath)
			if exp, isTomb := d.mfs.deletedTombstones[childKey]; isTomb && now.Before(exp) {
				if ms != nil {
					tombstonesToClear = append(tombstonesToClear, childKey)
				} else {
					continue
				}
			}
			name := m.Name
			if !seen[name] && !isOSJunk(name) && !isDuplicatiTestPath(name) {
				seen[name] = true
				list = append(list, &VirtualFileInfo{name: name, size: m.Size, mode: 0644, modTime: parseTime(m.ModificationDate), isDir: false})
			}
		}

		for _, mem := range d.mfs.uploadCache {
			childPath := path.Clean(path.Join(d.path, mem.Name))
			childKey := cacheKey(childPath)
			if exp, isTomb := d.mfs.deletedTombstones[childKey]; isTomb && now.Before(exp) {
				continue
			}
			if mem.FolderID == d.folderID && !seen[mem.Name] && !isOSJunk(mem.Name) && !isDuplicatiTestPath(mem.Name) {
				seen[mem.Name] = true
				list = append(list, &VirtualFileInfo{name: mem.Name, size: mem.Size, mode: 0644, modTime: mem.ModTime, isDir: false})
			}
		}
		d.mfs.cacheMu.RUnlock()

		if ms != nil {
			if vFiles, vErr := ms.ListVirtualEmptyFilesInFolder(d.folderID); vErr == nil {
				vFiles = d.mfs.autoHealHyperBackupWriter(d.path, d.folderID, media, vFiles)
				d.mfs.cacheMu.RLock()
				for _, vf := range vFiles {
					childPath := path.Clean(path.Join(d.path, vf.Name))
					childKey := cacheKey(childPath)
					if exp, isTomb := d.mfs.deletedTombstones[childKey]; isTomb && now.Before(exp) {
						continue
					}
					name := vf.Name
					if !seen[name] && !isOSJunk(name) && !isDuplicatiTestPath(name) {
						seen[name] = true
						modTime := parseTime(sapi.FlexDate(vf.ModTime))
						list = append(list, &VirtualFileInfo{name: name, size: 0, mode: 0644, modTime: modTime, isDir: false})
					}
				}
				d.mfs.cacheMu.RUnlock()
			}
		}

		if len(tombstonesToClear) > 0 {
			d.mfs.cacheMu.Lock()
			for _, p := range tombstonesToClear {
				delete(d.mfs.deletedTombstones, p)
			}
			d.mfs.cacheMu.Unlock()
		}

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
