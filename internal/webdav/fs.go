package webdav

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"
)

var (
	transferSem    = make(chan struct{}, 3)
	collisionRegex = regexp.MustCompile(` \(\d+\)(\.[^.]+)?$`)
	folderLocks    sync.Map
)

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
	ModTime   time.Time
	URL       string
	MD5       string
}

type CreatedFolder struct {
	Info sapi.FolderItem
	Time time.Time
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
}

type cachedFolders struct {
	folders []sapi.FolderItem
	time    time.Time
}

type cachedMedia struct {
	media []sapi.MediaItem
	time  time.Time
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
		cacheTTL:          15 * time.Second,
	}
}

func (mfs *MovistarFS) InvalidateCache(folderID int64) {
	mfs.cacheMu.Lock()
	defer mfs.cacheMu.Unlock()
	mfs.storageCache = nil
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
	if mfs.storageCache != nil && time.Since(mfs.storageCache.time) < 60*time.Second {
		sp := mfs.storageCache.space
		mfs.cacheMu.RUnlock()
		return sp
	}
	mfs.cacheMu.RUnlock()

	sp, err := mfs.client.GetStorageSpace()
	if err == nil && sp != nil {
		mfs.cacheMu.Lock()
		mfs.storageCache = &cachedStorage{space: sp, time: time.Now()}
		mfs.cacheMu.Unlock()
		return sp
	}
	return &sapi.StorageSpace{TotalBytes: 0, UsedBytes: 0, NoLimit: true}
}

func (mfs *MovistarFS) getSubfolders(parentID int64) []sapi.FolderItem {
	mfs.cacheMu.RLock()
	cached, ok := mfs.folderCache[parentID]
	if ok && time.Since(cached.time) < mfs.cacheTTL {
		folders := make([]sapi.FolderItem, len(cached.folders))
		copy(folders, cached.folders)
		mfs.cacheMu.RUnlock()
		return mfs.mergeCreatedFolders(parentID, folders)
	}
	mfs.cacheMu.RUnlock()

	subs, err := mfs.client.ListAllSubfolders(parentID)
	if err != nil {
		logger.Log("[WebDAV] ⚠️ Error listando subcarpetas de %d: %v", parentID, err)
		mfs.cacheMu.RLock()
		if ok && cached != nil {
			folders := make([]sapi.FolderItem, len(cached.folders))
			copy(folders, cached.folders)
			mfs.cacheMu.RUnlock()
			return mfs.mergeCreatedFolders(parentID, folders)
		}
		mfs.cacheMu.RUnlock()
		return nil
	}

	mfs.cacheMu.Lock()
	mfs.folderCache[parentID] = &cachedFolders{folders: subs, time: time.Now()}
	mfs.cacheMu.Unlock()

	return mfs.mergeCreatedFolders(parentID, subs)
}

func (mfs *MovistarFS) mergeCreatedFolders(parentID int64, folders []sapi.FolderItem) []sapi.FolderItem {
	mfs.cacheMu.Lock()
	defer mfs.cacheMu.Unlock()

	now := time.Now()
	seen := make(map[string]bool)
	for _, f := range folders {
		seen[f.Name] = true
	}

	for p, cf := range mfs.createdFolders {
		if now.Sub(cf.Time) > 60*time.Second {
			delete(mfs.createdFolders, p)
			continue
		}
		if cf.Info.ParentID == parentID && !seen[cf.Info.Name] {
			folders = append(folders, cf.Info)
			seen[cf.Info.Name] = true
		}
	}
	return folders
}

func (mfs *MovistarFS) getMedia(folderID int64) []sapi.MediaItem {
	mfs.cacheMu.RLock()
	cached, ok := mfs.mediaCache[folderID]
	if ok && time.Since(cached.time) < mfs.cacheTTL {
		items := make([]sapi.MediaItem, len(cached.media))
		copy(items, cached.media)
		mfs.cacheMu.RUnlock()
		return items
	}
	mfs.cacheMu.RUnlock()

	items, err := mfs.client.ListAllMediaInFolder(folderID)
	if err != nil {
		logger.Log("[WebDAV] ⚠️ Error listando media de %d: %v", folderID, err)
		mfs.cacheMu.RLock()
		if ok && cached != nil {
			res := make([]sapi.MediaItem, len(cached.media))
			copy(res, cached.media)
			mfs.cacheMu.RUnlock()
			return res
		}
		mfs.cacheMu.RUnlock()
		return nil
	}

	mfs.cacheMu.Lock()
	mfs.mediaCache[folderID] = &cachedMedia{media: items, time: time.Now()}
	mfs.cacheMu.Unlock()

	return items
}

func (mfs *MovistarFS) resolvePath(rawPath string) (folderInfo *sapi.FolderItem, mediaInfo *sapi.MediaItem, isFolder bool, err error) {
	cleanPath := path.Clean("/" + rawPath)

	// Check if path is currently a tombstone (deleted within 5m)
	mfs.cacheMu.RLock()
	if exp, ok := mfs.deletedTombstones[cleanPath]; ok {
		if time.Now().Before(exp) {
			mfs.cacheMu.RUnlock()
			return nil, nil, false, os.ErrNotExist
		}
	}
	mfs.cacheMu.RUnlock()

	rootID, err := mfs.client.GetRootFolderID()
	if err != nil {
		return nil, nil, false, err
	}

	if cleanPath == "/" || cleanPath == "." {
		return &sapi.FolderItem{ID: rootID, Name: "/", Magic: true}, nil, true, nil
	}

	// 1. Check uploadCache
	mfs.cacheMu.RLock()
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
	// 2. Check createdFolders con verificación de TTL (60s)
	if cf, ok := mfs.createdFolders[cleanPath]; ok {
		if time.Since(cf.Time) < 60*time.Second {
			info := cf.Info
			mfs.cacheMu.RUnlock()
			return &info, nil, true, nil
		}
	}
	mfs.cacheMu.RUnlock()

	parts := strings.Split(strings.Trim(cleanPath, "/"), "/")
	currentID := rootID
	var currentFolder *sapi.FolderItem

	for idx, part := range parts {
		isLast := (idx == len(parts)-1)

		subs := mfs.getSubfolders(currentID)
		var foundSub *sapi.FolderItem
		for _, s := range subs {
			sCopy := s
			if s.Name == part || strings.EqualFold(s.Name, part) {
				foundSub = &sCopy
				break
			}
			// Tolerancia Synology HyperBackup .hbk
			if strings.EqualFold(s.Name, part+".hbk") || (strings.HasSuffix(strings.ToLower(part), ".hbk") && strings.EqualFold(s.Name, part[:len(part)-4])) {
				foundSub = &sCopy
				break
			}
		}

		if foundSub != nil {
			currentID = foundSub.ID
			currentFolder = foundSub
			if isLast {
				return currentFolder, nil, true, nil
			}
			continue
		}

		// Si no se encontró en subcarpetas y es el último elemento, buscar en archivos
		if isLast {
			media := mfs.getMedia(currentID)
			var foundMedia *sapi.MediaItem
			stem := strings.TrimSuffix(part, filepath.Ext(part))
			for _, m := range media {
				mCopy := m
				if m.Name == part || strings.EqualFold(m.Name, part) {
					foundMedia = &mCopy
					break
				}
				// Tolerancia sufijos de colisión ' (1)'
				cleanMName := collisionRegex.ReplaceAllString(m.Name, "$1")
				if cleanMName != "" && (cleanMName == part || strings.EqualFold(cleanMName, part)) {
					foundMedia = &mCopy
					break
				}
				if strings.HasPrefix(m.Name, stem+" (") {
					rest := m.Name[len(stem)+2:]
					if closeParen := strings.Index(rest, ")"); closeParen != -1 {
						digits := rest[:closeParen]
						if _, err := strconv.Atoi(digits); err == nil {
							foundMedia = &mCopy
							break
						}
					}
				}
			}
			if foundMedia != nil {
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
	unlock := acquireFolderLock(cleanPath)
	defer unlock()

	// Clear tombstone if re-creating
	mfs.cacheMu.Lock()
	delete(mfs.deletedTombstones, cleanPath)
	mfs.cacheMu.Unlock()

	parentDir := path.Dir(cleanPath)
	folderName := path.Base(cleanPath)

	parentFolder, _, isFolder, err := mfs.resolvePath(parentDir)
	if err != nil || !isFolder || parentFolder == nil {
		return os.ErrNotExist
	}

	// Comprobar si ya existe
	subs := mfs.getSubfolders(parentFolder.ID)
	for _, s := range subs {
		if strings.EqualFold(s.Name, folderName) {
			logger.Log("[WebDAV] ℹ Carpeta '%s' ya existía en Movistar/O2 Cloud (ID: %d). Reutilizando.", folderName, s.ID)
			mfs.cacheMu.Lock()
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
	mfs.createdFolders[cleanPath] = &CreatedFolder{Info: *created, Time: time.Now()}
	mfs.cacheMu.Unlock()
	mfs.InvalidateCache(parentFolder.ID)

	return nil
}

func (mfs *MovistarFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	cleanPath := path.Clean("/" + name)

	// Manejo de escritura (flag os.O_RDWR o os.O_WRONLY o os.O_CREATE)
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE) != 0 {
		parentDir := path.Dir(cleanPath)
		fileName := path.Base(cleanPath)

		parentFolder, _, isFolder, err := mfs.resolvePath(parentDir)
		if err != nil || !isFolder || parentFolder == nil {
			return nil, os.ErrNotExist
		}

		// Chequear si ya existe un archivo con ese nombre para borrarlo antes y no crear sufijo ' (1)'
		var existingID string
		_, mediaInfo, isF, err := mfs.resolvePath(cleanPath)
		if err == nil && !isF && mediaInfo != nil {
			existingID = mediaInfo.ID
		}

		return NewWebDAVFileWriter(mfs, cleanPath, fileName, parentFolder.ID, existingID), nil
	}

	// Modo lectura
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
			URL:              mem.URL,
			FolderID:         mem.FolderID,
			ModificationDate: sapi.FlexDate(mem.ModTime.UTC().Format("20060102T150405Z")),
		}
		return NewWebDAVFileReader(mfs, cleanPath, item), nil
	}
	mfs.cacheMu.RUnlock()

	return NewWebDAVFileReader(mfs, cleanPath, *mediaInfo), nil
}

func (mfs *MovistarFS) RemoveAll(ctx context.Context, name string) error {
	cleanPath := path.Clean("/" + name)
	folderInfo, mediaInfo, isFolder, err := mfs.resolvePath(cleanPath)
	if err != nil {
		return nil // WebDAV idempotencia
	}

	if isFolder && folderInfo != nil {
		logger.Log("[WebDAV] 🗑 Eliminando carpeta: '%s' (ID: %d)...", folderInfo.Name, folderInfo.ID)
		_ = mfs.client.DeleteFolder(folderInfo.ID, false)
		mfs.cacheMu.Lock()
		delete(mfs.createdFolders, cleanPath)
		mfs.deletedTombstones[cleanPath] = time.Now().Add(5 * time.Minute)
		mfs.cacheMu.Unlock()
		mfs.InvalidateCache(0)
		return nil
	}

	if !isFolder && mediaInfo != nil {
		logger.Log("[WebDAV] 🗑 Eliminando archivo: '%s' (ID: %s)...", mediaInfo.Name, mediaInfo.ID)
		if mediaInfo.ID != "" {
			_ = mfs.client.DeleteFile(mediaInfo.ID)
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
	newParentDir := path.Dir(newClean)
	newBaseName := path.Base(newClean)

	parentFolder, _, isFolder, err := mfs.resolvePath(newParentDir)
	if err != nil || !isFolder || parentFolder == nil {
		return os.ErrNotExist
	}

	oldFolder, oldMedia, isF, err := mfs.resolvePath(oldClean)
	if err != nil {
		return err
	}

	logger.Log("[WebDAV] 🔄 Renombrando: '%s' -> '%s'...", oldClean, newClean)

	if isF && oldFolder != nil {
		err := mfs.client.UpdateFolder(oldFolder.ID, newBaseName, parentFolder.ID)
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

		if oldMedia.ID != "" {
			if err := mfs.client.UpdateMedia(oldMedia.ID, newBaseName, parentFolder.ID); err != nil {
				return err
			}
		}
		mfs.cacheMu.Lock()
		if mem, ok := mfs.uploadCache[oldClean]; ok {
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

func (mfs *MovistarFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	cleanPath := path.Clean("/" + name)
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

func (w *WebDAVFileWriter) Write(p []byte) (n int, err error) {
	w.size += int64(len(p))
	if w.tmpFile != nil {
		return w.tmpFile.Write(p)
	}
	if w.buf.Len()+len(p) > 10*1024*1024 {
		tmp, err := os.CreateTemp("", "webdav_upload_*")
		if err != nil {
			return 0, err
		}
		w.tmpFile = tmp
		if _, err := tmp.Write(w.buf.Bytes()); err != nil {
			return 0, err
		}
		w.buf = nil
		return tmp.Write(p)
	}
	return w.buf.Write(p)
}

func (w *WebDAVFileWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true

	sizeMB := float64(w.size) / (1024 * 1024)
	logger.Log("[WebDAV] ⬆ Subiendo: '%s' (%.2f MB) a Movistar/O2 Cloud...", w.name, sizeMB)

	transferSem <- struct{}{}
	defer func() { <-transferSem }()

	var reader io.Reader
	var cachedContent []byte

	if w.tmpFile != nil {
		defer os.Remove(w.tmpFile.Name())
		defer w.tmpFile.Close()
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
	media, err := w.mfs.client.UploadFileToFolder(w.name, reader, w.size, w.parentFolderID, mimeType, w.existingID)
	if err != nil {
		logger.Log("[WebDAV] ❌ Error al subir '%s': %v", w.name, err)
		return err
	}

	logger.Log("[WebDAV] ✔ Subida completada: '%s' (%.2f MB, ID: %s)", w.name, sizeMB, media.ID)

	w.mfs.cacheMu.Lock()
	now := time.Now()
	delete(w.mfs.deletedTombstones, w.path)
	w.mfs.uploadCache[w.path] = &MemoryFile{
		ID:       media.ID,
		Name:     w.name,
		FolderID: w.parentFolderID,
		Content:  cachedContent,
		Size:     w.size,
		ModTime:  now,
		URL:      media.URL,
	}

	// Limpieza de entradas expiradas para prevenir fugas de memoria
	for p, mem := range w.mfs.uploadCache {
		if now.Sub(mem.ModTime) > 15*time.Minute {
			delete(w.mfs.uploadCache, p)
		}
	}
	for p, cf := range w.mfs.createdFolders {
		if now.Sub(cf.Time) > 5*time.Minute {
			delete(w.mfs.createdFolders, p)
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
	return &VirtualFileInfo{name: w.name, size: w.size, mode: 0644, modTime: time.Now()}, nil
}

type WebDAVFileReader struct {
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
	if r.stream == nil {
		sizeMB := float64(r.item.Size) / (1024 * 1024)
		if r.pos > 0 {
			logger.Log("[WebDAV] ⬇ Descargando rango [byte %d..] de '%s' (%.2f MB)...", r.pos, r.item.Name, sizeMB)
		} else {
			logger.Log("[WebDAV] ⬇ Descargando: '%s' (%.2f MB) desde Movistar Cloud...", r.item.Name, sizeMB)
		}
		transferSem <- struct{}{}
		st, err := r.mfs.client.DownloadFileStreamRange(r.item, r.pos)
		if err != nil {
			<-transferSem
			r.streamMu.Unlock()
			return 0, err
		}
		r.stream = &semaphoreReleaseReader{stream: st}
	}
	st := r.stream
	r.streamMu.Unlock()

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

func (s *semaphoreReleaseReader) Read(p []byte) (n int, err error) {
	n, err = s.stream.Read(p)
	if err == io.EOF {
		s.mu.Lock()
		if !s.released {
			s.released = true
			<-transferSem
		}
		s.mu.Unlock()
	}
	return n, err
}

func (s *semaphoreReleaseReader) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.released {
		s.released = true
		<-transferSem
	}
	return s.stream.Close()
}

type WebDAVMemoryReader struct {
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
		subs := d.mfs.getSubfolders(d.folderID)
		seen := make(map[string]bool)

		for _, s := range subs {
			if !seen[s.Name] {
				seen[s.Name] = true
				list = append(list, &VirtualFileInfo{name: s.Name, size: 0, mode: os.ModeDir | 0755, modTime: parseTime(s.ModificationDate), isDir: true})
			}
		}

		media := d.mfs.getMedia(d.folderID)
		for _, m := range media {
			name := m.Name
			cleanName := collisionRegex.ReplaceAllString(name, "$1")
			if cleanName != "" && !seen[cleanName] {
				name = cleanName
			}
			if !seen[name] {
				seen[name] = true
				list = append(list, &VirtualFileInfo{name: name, size: m.Size, mode: 0644, modTime: parseTime(m.ModificationDate), isDir: false})
			}
		}

		d.mfs.cacheMu.RLock()
		for _, mem := range d.mfs.uploadCache {
			if mem.FolderID == d.folderID && !seen[mem.Name] {
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

// Compile-time interface checks
var (
	_ webdav.File         = (*WebDAVFileWriter)(nil)
	_ webdav.File         = (*WebDAVFileReader)(nil)
	_ webdav.File         = (*WebDAVMemoryReader)(nil)
	_ webdav.File         = (*WebDAVDirReader)(nil)
	_ webdav.FileSystem   = (*MovistarFS)(nil)
	_ webdav.ContentTyper = (*VirtualFileInfo)(nil)
)
