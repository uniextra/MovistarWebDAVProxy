package webdav

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"
)

const galleryCacheTTL = 5 * time.Minute

type galleryEntry struct {
	displayName string
	item        sapi.MediaItem
	modTime     time.Time
	year        string
	month       string
}

type galleryIndex struct {
	builtAt     time.Time
	dirName     string
	layout      string
	dirs        map[string][]os.FileInfo
	dirModTimes map[string]time.Time
	files       map[string]galleryEntry
	filesLower  map[string]galleryEntry
}

func (mfs *MovistarFS) invalidateGalleryCache() {
	if mfs == nil {
		return
	}
	mfs.galleryMu.Lock()
	mfs.galleryCache = nil
	mfs.galleryMu.Unlock()
}

// hasRealGalleryConflict comprueba si ya existe una carpeta real en / con el mismo
// nombre que GALLERY_DIR; en ese caso no se monta la galería virtual.
func (mfs *MovistarFS) hasRealGalleryConflict() bool {
	if mfs == nil || !config.GalleryEnabled() {
		return false
	}
	dirName := config.GalleryDir()
	rootID, err := mfs.getRootFolderID()
	if err != nil || rootID <= 0 {
		return false
	}
	if ms := mfs.MetadataStore(); ms != nil {
		if subs, err := ms.ListSubfolders(rootID); err == nil {
			for _, s := range subs {
				if strings.EqualFold(s.Name, dirName) {
					if mfs.galleryConflict.CompareAndSwap(false, true) {
						logger.Log("[WebDAV] ⚠️ Galería virtual '/%s' desactivada: ya existe una carpeta real con ese nombre en Movistar/O2 Cloud.", dirName)
					}
					return true
				}
			}
		}
	}
	subs, err := mfs.getSubfolders(rootID)
	if err != nil {
		return false
	}
	for _, s := range subs {
		if strings.EqualFold(s.Name, dirName) {
			if mfs.galleryConflict.CompareAndSwap(false, true) {
				logger.Log("[WebDAV] ⚠️ Galería virtual '/%s' desactivada: ya existe una carpeta real con ese nombre en Movistar/O2 Cloud.", dirName)
			}
			return true
		}
	}
	return false
}

func (mfs *MovistarFS) isGalleryRootNameMatch(seg string) bool {
	dirName := config.GalleryDir()
	if mfs != nil && mfs.caseInsensitive() {
		return strings.EqualFold(seg, dirName)
	}
	return seg == dirName
}

func (mfs *MovistarFS) isGalleryPath(cleanPath string) bool {
	if mfs == nil || !config.GalleryEnabled() || mfs.hasRealGalleryConflict() {
		return false
	}
	dirName := config.GalleryDir()
	basePrefix := "/" + dirName
	if mfs.caseInsensitive() {
		if strings.EqualFold(cleanPath, basePrefix) {
			return true
		}
		return len(cleanPath) > len(basePrefix) &&
			strings.EqualFold(cleanPath[:len(basePrefix)], basePrefix) &&
			cleanPath[len(basePrefix)] == '/'
	}
	return cleanPath == basePrefix || strings.HasPrefix(cleanPath, basePrefix+"/")
}

func galleryNormalizedRelativeSegments(cleanPath string) ([]string, bool) {
	trimmed := strings.Trim(cleanPath, "/")
	if trimmed == "" {
		return nil, false
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 {
		return nil, false
	}
	return parts[1:], true
}

func isFourDigits(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isTwoDigitMonth(s string) bool {
	if len(s) != 2 || s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' {
		return false
	}
	m, err := strconv.Atoi(s)
	return err == nil && m >= 1 && m <= 12
}

// isGalleryVirtualDir identifica los directorios virtuales propios de la galería
// (/dirName y, en layout year-month, /dirName/YYYY, /dirName/YYYY/MM y /dirName/unknown).
func (mfs *MovistarFS) isGalleryVirtualDir(cleanPath string) bool {
	if !mfs.isGalleryPath(cleanPath) {
		return false
	}
	rel, ok := galleryNormalizedRelativeSegments(cleanPath)
	if !ok {
		return false
	}
	if len(rel) == 0 {
		return true
	}
	if strings.EqualFold(config.GalleryLayout(), "year-month") {
		if len(rel) == 1 && (isFourDigits(rel[0]) || strings.EqualFold(rel[0], "unknown")) {
			return true
		}
		if len(rel) == 2 && isFourDigits(rel[0]) && isTwoDigitMonth(rel[1]) {
			return true
		}
	}
	return false
}

// validGalleryUploadPath comprueba si la ruta destino de un PUT pertenece a un
// nivel válido dentro de la galería virtual.
func (mfs *MovistarFS) validGalleryUploadPath(cleanPath string) bool {
	if !mfs.isGalleryPath(cleanPath) || mfs.isGalleryVirtualDir(cleanPath) {
		return false
	}
	if path.Base(cleanPath) == "" {
		return false
	}
	rel, ok := galleryNormalizedRelativeSegments(cleanPath)
	if !ok || len(rel) == 0 {
		return false
	}
	if config.GalleryLayout() == "flat" {
		return len(rel) == 1
	}
	// Layout "year-month": admite /_gallery/<file>, /_gallery/unknown/<file>
	// y /_gallery/<YYYY>/<MM>/<file>.
	switch len(rel) {
	case 1:
		return rel[0] != ""
	case 2:
		return strings.EqualFold(rel[0], "unknown") && rel[1] != ""
	case 3:
		return isFourDigits(rel[0]) && isTwoDigitMonth(rel[1]) && rel[2] != ""
	default:
		return false
	}
}

// parseGalleryDate analiza fechas sin recurrir al fallback time.Now() de FlexDate.Time().
func parseGalleryDate(fd sapi.FlexDate) (time.Time, bool) {
	str := strings.TrimSpace(string(fd))
	if str == "" || str == "0" {
		return time.Time{}, false
	}
	if num, err := strconv.ParseInt(str, 10, 64); err == nil {
		if num <= 0 {
			return time.Time{}, false
		}
		var t time.Time
		if num > 100000000000 {
			t = time.UnixMilli(num).UTC()
		} else {
			t = time.Unix(num, 0).UTC()
		}
		if t.Year() >= 1970 && t.Year() <= 2100 {
			return t, true
		}
		return time.Time{}, false
	}
	if f, err := strconv.ParseFloat(str, 64); err == nil {
		if f <= 0 {
			return time.Time{}, false
		}
		var t time.Time
		if f > 100000000000 {
			t = time.UnixMilli(int64(f)).UTC()
		} else {
			sec := int64(f)
			nsec := int64((f - float64(sec)) * 1e9)
			t = time.Unix(sec, nsec).UTC()
		}
		if t.Year() >= 1970 && t.Year() <= 2100 {
			return t, true
		}
		return time.Time{}, false
	}
	layouts := []string{
		"20060102T150405Z",
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		time.RFC1123,
		time.RFC1123Z,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, str); err == nil {
			u := t.UTC()
			if u.Year() >= 1970 && u.Year() <= 2100 {
				return u, true
			}
			return time.Time{}, false
		}
	}
	return time.Time{}, false
}

func galleryItemDate(m sapi.MediaItem) (modTime time.Time, year string, month string) {
	t, ok := parseGalleryDate(m.CreationDate)
	if !ok {
		t, ok = parseGalleryDate(m.ModificationDate)
	}
	if !ok {
		return time.Unix(0, 0).UTC(), "unknown", ""
	}
	return t, fmt.Sprintf("%04d", t.Year()), fmt.Sprintf("%02d", int(t.Month()))
}

func disambiguateGalleryName(name, id string) string {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s (%s)%s", stem, id, ext)
}

func (idx *galleryIndex) lookupDir(cleanPath string, fold bool) ([]os.FileInfo, bool) {
	if idx == nil {
		return nil, false
	}
	if entries, ok := idx.dirs[cleanPath]; ok {
		return entries, true
	}
	if fold {
		for k, entries := range idx.dirs {
			if strings.EqualFold(k, cleanPath) {
				return entries, true
			}
		}
	}
	return nil, false
}

func (idx *galleryIndex) lookupDirModTime(cleanPath string, fold bool) time.Time {
	if idx == nil {
		return time.Unix(0, 0).UTC()
	}
	if mt, ok := idx.dirModTimes[cleanPath]; ok {
		return mt
	}
	if fold {
		for k, mt := range idx.dirModTimes {
			if strings.EqualFold(k, cleanPath) {
				return mt
			}
		}
	}
	return time.Unix(0, 0).UTC()
}

func (idx *galleryIndex) lookupFile(cleanPath string, fold bool) (galleryEntry, bool) {
	if idx == nil {
		return galleryEntry{}, false
	}
	if e, ok := idx.files[cleanPath]; ok {
		return e, true
	}
	if fold {
		if e, ok := idx.filesLower[strings.ToLower(cleanPath)]; ok {
			return e, true
		}
	}
	return galleryEntry{}, false
}

func (mfs *MovistarFS) getGalleryIndex() (*galleryIndex, error) {
	dirName := config.GalleryDir()
	layout := config.GalleryLayout()

	mfs.galleryMu.RLock()
	cached := mfs.galleryCache
	if cached != nil && cached.dirName == dirName && cached.layout == layout && time.Since(cached.builtAt) < galleryCacheTTL {
		mfs.galleryMu.RUnlock()
		return cached, nil
	}
	mfs.galleryMu.RUnlock()

	mfs.galleryMu.Lock()
	defer mfs.galleryMu.Unlock()

	// Double check under write lock
	if mfs.galleryCache != nil && mfs.galleryCache.dirName == dirName && mfs.galleryCache.layout == layout && time.Since(mfs.galleryCache.builtAt) < galleryCacheTTL {
		return mfs.galleryCache, nil
	}

	var items []sapi.MediaItem
	if ms := mfs.MetadataStore(); ms != nil {
		list, err := ms.ListGalleryMedia()
		if err != nil {
			return nil, err
		}
		items = list
	}

	seenIDs := make(map[string]bool, len(items))
	for _, it := range items {
		if it.ID != "" {
			seenIDs[it.ID] = true
		}
	}

	now := time.Now()
	tombstones := make(map[string]time.Time)
	mfs.cacheMu.RLock()
	for k, exp := range mfs.deletedTombstones {
		if now.Before(exp) {
			tombstones[k] = exp
		}
	}
	for _, mem := range mfs.uploadCache {
		if mem.FolderID == 0 && (mem.ID == "" || !seenIDs[mem.ID]) {
			mt := mem.ModTime.UTC()
			if mt.IsZero() {
				mt = now.UTC()
			}
			ts := sapi.FlexDate(mt.Format("20060102T150405Z"))
			items = append(items, sapi.MediaItem{
				ID:               mem.ID,
				Name:             mem.Name,
				Size:             mem.Size,
				URL:              mem.URL,
				FolderID:         0,
				CreationDate:     ts,
				ModificationDate: ts,
			})
			if mem.ID != "" {
				seenIDs[mem.ID] = true
			}
		}
	}
	mfs.cacheMu.RUnlock()

	// Garantizar orden determinista incluso con elementos añadidos desde uploadCache.
	sort.SliceStable(items, func(i, j int) bool {
		ni, nj := strings.ToLower(items[i].Name), strings.ToLower(items[j].Name)
		if ni != nj {
			return ni < nj
		}
		idI, errI := strconv.ParseInt(items[i].ID, 10, 64)
		idJ, errJ := strconv.ParseInt(items[j].ID, 10, 64)
		if errI == nil && errJ == nil && idI != idJ {
			return idI < idJ
		}
		return items[i].ID < items[j].ID
	})

	rootVirtual := "/" + dirName
	idx := &galleryIndex{
		builtAt:     now,
		dirName:     dirName,
		layout:      layout,
		dirs:        map[string][]os.FileInfo{rootVirtual: {}},
		dirModTimes: map[string]time.Time{},
		files:       make(map[string]galleryEntry),
		filesLower:  make(map[string]galleryEntry),
	}

	bucketUsed := make(map[string]map[string]bool)
	bucketFiles := make(map[string][]os.FileInfo)
	yearsSet := make(map[string]bool)
	yearMonths := make(map[string]map[string]bool)

	updateDirMT := func(dirPath string, mt time.Time) {
		if cur, ok := idx.dirModTimes[dirPath]; !ok || mt.After(cur) {
			idx.dirModTimes[dirPath] = mt
		}
	}

	for _, it := range items {
		if it.Name == "" || isOSJunk(it.Name) || isDuplicatiTestPath(it.Name) {
			continue
		}
		mt, year, month := galleryItemDate(it)
		var dirPath string
		if layout == "flat" {
			dirPath = rootVirtual
		} else if year == "unknown" {
			dirPath = rootVirtual + "/unknown"
			yearsSet["unknown"] = true
		} else {
			dirPath = rootVirtual + "/" + year + "/" + month
			yearsSet[year] = true
			if yearMonths[year] == nil {
				yearMonths[year] = make(map[string]bool)
			}
			yearMonths[year][month] = true
		}

		used := bucketUsed[dirPath]
		if used == nil {
			used = make(map[string]bool)
			bucketUsed[dirPath] = used
		}
		displayName := it.Name
		if used[strings.ToLower(displayName)] {
			displayName = disambiguateGalleryName(it.Name, it.ID)
			if used[strings.ToLower(displayName)] {
				displayName = disambiguateGalleryName(it.Name, it.ID+"_"+strconv.FormatInt(int64(len(used)), 10))
			}
		}
		fullPath := dirPath + "/" + displayName
		if _, isTomb := tombstones[cacheKey(fullPath)]; isTomb {
			continue
		}
		if _, isTomb := tombstones[strings.ToLower(cacheKey(fullPath))]; isTomb {
			continue
		}
		used[strings.ToLower(displayName)] = true

		entry := galleryEntry{
			displayName: displayName,
			item:        it,
			modTime:     mt,
			year:        year,
			month:       month,
		}
		idx.files[fullPath] = entry
		idx.filesLower[strings.ToLower(fullPath)] = entry
		bucketFiles[dirPath] = append(bucketFiles[dirPath], &VirtualFileInfo{
			name:    displayName,
			size:    it.Size,
			mode:    0644,
			modTime: mt,
			isDir:   false,
		})
		updateDirMT(dirPath, mt)
		if layout != "flat" && year != "unknown" {
			updateDirMT(rootVirtual+"/"+year, mt)
		}
		updateDirMT(rootVirtual, mt)
	}

	if layout == "flat" {
		idx.dirs[rootVirtual] = bucketFiles[rootVirtual]
	} else {
		years := make([]string, 0, len(yearsSet))
		for y := range yearsSet {
			years = append(years, y)
		}
		sort.Slice(years, func(i, j int) bool {
			if years[i] == "unknown" {
				return false
			}
			if years[j] == "unknown" {
				return true
			}
			return years[i] < years[j]
		})
		rootEntries := make([]os.FileInfo, 0, len(years))
		for _, y := range years {
			yPath := rootVirtual + "/" + y
			rootEntries = append(rootEntries, &VirtualFileInfo{
				name:    y,
				size:    0,
				mode:    os.ModeDir | 0755,
				modTime: idx.lookupDirModTime(yPath, false),
				isDir:   true,
			})
			if y == "unknown" {
				idx.dirs[yPath] = bucketFiles[yPath]
				continue
			}
			monthsMap := yearMonths[y]
			months := make([]string, 0, len(monthsMap))
			for m := range monthsMap {
				months = append(months, m)
			}
			sort.Strings(months)
			yEntries := make([]os.FileInfo, 0, len(months))
			for _, m := range months {
				mPath := yPath + "/" + m
				yEntries = append(yEntries, &VirtualFileInfo{
					name:    m,
					size:    0,
					mode:    os.ModeDir | 0755,
					modTime: idx.lookupDirModTime(mPath, false),
					isDir:   true,
				})
				idx.dirs[mPath] = bucketFiles[mPath]
			}
			idx.dirs[yPath] = yEntries
		}
		idx.dirs[rootVirtual] = rootEntries
	}

	if _, ok := idx.dirModTimes[rootVirtual]; !ok {
		idx.dirModTimes[rootVirtual] = time.Now().UTC()
	}

	mfs.galleryCache = idx
	return idx, nil
}

func (mfs *MovistarFS) recentGalleryUpload(cleanPath string) (*MemoryFile, bool) {
	pathKey := cacheKey(cleanPath)
	mfs.cacheMu.RLock()
	defer mfs.cacheMu.RUnlock()

	if exp, ok := mfs.deletedTombstones[pathKey]; ok && time.Now().Before(exp) {
		return nil, false
	}
	if mem, ok := mfs.uploadCache[pathKey]; ok && mem.FolderID == 0 {
		return mem, true
	}
	if mfs.caseInsensitive() {
		if _, mem, ok := foldLookup(mfs.uploadCache, pathKey); ok && mem.FolderID == 0 {
			return mem, true
		}
	}
	return nil, false
}

func (mfs *MovistarFS) galleryStat(cleanPath string) (os.FileInfo, error) {
	idx, err := mfs.getGalleryIndex()
	if err != nil {
		return nil, err
	}
	fold := mfs.caseInsensitive()

	if _, isDir := idx.lookupDir(cleanPath, fold); isDir {
		return &VirtualFileInfo{
			name:    path.Base(cleanPath),
			size:    0,
			mode:    os.ModeDir | 0755,
			modTime: idx.lookupDirModTime(cleanPath, fold),
			isDir:   true,
		}, nil
	}

	if entry, ok := idx.lookupFile(cleanPath, fold); ok {
		return &VirtualFileInfo{
			name:    entry.displayName,
			size:    entry.item.Size,
			mode:    0644,
			modTime: entry.modTime,
			isDir:   false,
		}, nil
	}

	if mem, ok := mfs.recentGalleryUpload(cleanPath); ok {
		return &VirtualFileInfo{
			name:    mem.Name,
			size:    mem.Size,
			mode:    0644,
			modTime: mem.ModTime,
			isDir:   false,
		}, nil
	}

	return nil, os.ErrNotExist
}

func (mfs *MovistarFS) galleryOpenFile(ctx context.Context, cleanPath string, flag int, perm os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_CREATE|os.O_TRUNC) != 0 {
		if !config.GalleryUpload() {
			return nil, fs.ErrPermission
		}
		if mfs.isGalleryVirtualDir(cleanPath) {
			return nil, os.ErrInvalid
		}
		if !mfs.validGalleryUploadPath(cleanPath) {
			return nil, os.ErrNotExist
		}
		fileName := path.Base(cleanPath)
		if !sapi.IsGalleryMediaName(fileName) {
			if guard, _ := ctx.Value(uploadGuardKey{}).(*uploadGuard); guard != nil {
				guard.unsupported.Store(true)
			}
			return nil, ErrUploadUnsupported
		}

		idx, err := mfs.getGalleryIndex()
		if err != nil {
			return nil, err
		}
		uploadName := fileName
		var existingID string
		var existingItem *sapi.MediaItem
		if entry, ok := idx.lookupFile(cleanPath, mfs.caseInsensitive()); ok {
			existingID = entry.item.ID
			it := entry.item
			existingItem = &it
			uploadName = entry.item.Name
		}

		fw := NewWebDAVFileWriter(mfs, cleanPath, uploadName, 0, existingID)
		fw.ctx = ctx
		if existingItem != nil {
			fw.existingItem = existingItem
		}
		fw.modTime = ocMtimeFromContext(ctx)
		fw.guard, _ = ctx.Value(uploadGuardKey{}).(*uploadGuard)
		return fw, nil
	}

	// Modo lectura
	idx, err := mfs.getGalleryIndex()
	if err != nil {
		return nil, err
	}
	fold := mfs.caseInsensitive()

	if _, isDir := idx.lookupDir(cleanPath, fold); isDir {
		return NewWebDAVGalleryDirReader(mfs, cleanPath, idx), nil
	}

	entry, ok := idx.lookupFile(cleanPath, fold)
	if !ok {
		if mem, okMem := mfs.recentGalleryUpload(cleanPath); okMem {
			if mem.Size == 0 || len(mem.Content) > 0 {
				return NewWebDAVMemoryReader(cleanPath, mem), nil
			}
			item := sapi.MediaItem{
				ID:               mem.ID,
				Name:             mem.Name,
				Size:             mem.Size,
				URL:              mem.URL,
				FolderID:         0,
				ModificationDate: sapi.FlexDate(mem.ModTime.UTC().Format("20060102T150405Z")),
			}
			r := NewWebDAVFileReader(mfs, cleanPath, item)
			r.resolveURL = func() string {
				mfs.waitUploadSettled(ctx, mem.ID, mem.Name)
				return mfs.waitForDownloadURL(mem)
			}
			return r, nil
		}
		return nil, os.ErrNotExist
	}

	mediaInfo := entry.item
	pathKey := cacheKey(cleanPath)

	mfs.cacheMu.RLock()
	if mem, okMem := mfs.recentUpload(pathKey, &mediaInfo); okMem {
		mfs.cacheMu.RUnlock()
		if mem.Size == 0 || len(mem.Content) > 0 {
			return NewWebDAVMemoryReader(cleanPath, mem), nil
		}
		item := sapi.MediaItem{
			ID:               mem.ID,
			Name:             mem.Name,
			Size:             mem.Size,
			URL:              mem.URL,
			FolderID:         0,
			ModificationDate: sapi.FlexDate(mem.ModTime.UTC().Format("20060102T150405Z")),
		}
		r := NewWebDAVFileReader(mfs, cleanPath, item)
		r.resolveURL = func() string {
			mfs.waitUploadSettled(ctx, mem.ID, mem.Name)
			return mfs.waitForDownloadURL(mem)
		}
		return r, nil
	}
	mfs.cacheMu.RUnlock()

	if mediaInfo.Size == 0 {
		return NewWebDAVMemoryReader(cleanPath, &MemoryFile{
			ID:       mediaInfo.ID,
			Name:     mediaInfo.Name,
			FolderID: 0,
			Content:  []byte{},
			Size:     0,
			ModTime:  entry.modTime,
			CachedAt: time.Now(),
		}), nil
	}

	r := NewWebDAVFileReader(mfs, cleanPath, mediaInfo)
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

func (mfs *MovistarFS) galleryRemoveAll(ctx context.Context, cleanPath string) error {
	if !config.GalleryDelete() {
		return fs.ErrPermission
	}
	if mfs.isGalleryVirtualDir(cleanPath) {
		return fs.ErrPermission
	}

	unlock := acquireFolderLock(cleanPath)
	defer unlock()

	idx, err := mfs.getGalleryIndex()
	if err != nil {
		return err
	}

	var targetID, targetName string
	if entry, ok := idx.lookupFile(cleanPath, mfs.caseInsensitive()); ok {
		if entry.item.FolderID != 0 {
			return fs.ErrPermission
		}
		targetID = entry.item.ID
		targetName = entry.item.Name
	} else if mem, okMem := mfs.recentGalleryUpload(cleanPath); okMem {
		if mem.FolderID != 0 {
			return fs.ErrPermission
		}
		targetID = mem.ID
		targetName = mem.Name
	} else {
		pathKey := cacheKey(cleanPath)
		mfs.cacheMu.Lock()
		delete(mfs.uploadCache, pathKey)
		mfs.cacheMu.Unlock()
		return nil
	}

	pathKey := cacheKey(cleanPath)
	if targetID != "" {
		mfs.cacheMu.RLock()
		var waitTime time.Duration
		if mem, ok := mfs.uploadCache[pathKey]; ok && !mem.CachedAt.IsZero() {
			if elapsed := time.Since(mem.CachedAt); elapsed < 6*time.Second {
				waitTime = 6*time.Second - elapsed
			}
		}
		mfs.cacheMu.RUnlock()

		if waitTime > 0 {
			logger.Log("[WebDAV] ⏳ Archivo de galería '%s' recién subido. Esperando %v antes de borrar...", targetName, waitTime.Round(time.Millisecond))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(waitTime):
			}
		}

		logger.Log("[WebDAV] 🗑 Eliminando elemento de galería: '%s' (ID: %s)...", targetName, targetID)
		var delErr error
		for attempt := 0; attempt < 3; attempt++ {
			delErr = mfs.client.DeleteFile(targetID, targetName)
			if delErr == nil || !errors.Is(delErr, sapi.ErrSessionRefreshTransient) {
				break
			}
			wait := time.Duration(1<<attempt) * 2 * time.Second
			logger.Log("[WebDAV] ⏳ Renovación transitoria al borrar '%s' de galería; reintentando en %v (%d/3)...", targetName, wait, attempt+1)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		if delErr != nil {
			logger.Log("[WebDAV] ❌ Error eliminando de galería '%s' (ID: %s): %v", targetName, targetID, delErr)
			return fs.ErrPermission
		}
	}

	if ms := mfs.MetadataStore(); ms != nil && targetID != "" {
		logMetaErr("DeleteMedia", ms.DeleteMedia(targetID))
	}

	exp := time.Now().Add(15 * time.Second)
	mfs.cacheMu.Lock()
	delete(mfs.uploadCache, pathKey)
	if targetID != "" {
		for k, mem := range mfs.uploadCache {
			if mem.ID == targetID {
				delete(mfs.uploadCache, k)
				mfs.deletedTombstones[k] = exp
			}
		}
	}
	mfs.deletedTombstones[pathKey] = exp
	mfs.cacheMu.Unlock()

	if targetID != "" {
		mfs.removeFromGalleryCache(targetID)
	} else {
		mfs.invalidateGalleryCache()
	}
	return nil
}

func (mfs *MovistarFS) updateGalleryCacheURL(id, downloadURL, etag string) {
	if mfs == nil || id == "" || downloadURL == "" {
		return
	}
	mfs.galleryMu.Lock()
	defer mfs.galleryMu.Unlock()
	idx := mfs.galleryCache
	if idx == nil {
		return
	}
	for k, e := range idx.files {
		if e.item.ID == id {
			e.item.URL = downloadURL
			if etag != "" {
				e.item.ETag = etag
			}
			idx.files[k] = e
			idx.filesLower[strings.ToLower(k)] = e
		}
	}
}

func (mfs *MovistarFS) addToGalleryCache(item sapi.MediaItem) {
	if mfs == nil || item.Name == "" || item.FolderID != 0 {
		return
	}
	mfs.galleryMu.Lock()
	defer mfs.galleryMu.Unlock()
	idx := mfs.galleryCache
	if idx == nil {
		return
	}

	rootVirtual := "/" + idx.dirName
	mt, year, month := galleryItemDate(item)
	var dirPath string
	if idx.layout == "flat" {
		dirPath = rootVirtual
	} else if year == "unknown" {
		dirPath = rootVirtual + "/unknown"
	} else {
		dirPath = rootVirtual + "/" + year + "/" + month
	}

	// Evitar duplicar el mismo ID si es sobrescritura
	for k, e := range idx.files {
		if e.item.ID == item.ID {
			delete(idx.files, k)
			delete(idx.filesLower, strings.ToLower(k))
			parent := path.Dir(k)
			if list, ok := idx.dirs[parent]; ok {
				filtered := list[:0]
				for _, fi := range list {
					if fi.Name() != e.displayName {
						filtered = append(filtered, fi)
					}
				}
				idx.dirs[parent] = filtered
			}
		}
	}

	displayName := item.Name
	fullPath := dirPath + "/" + displayName
	if _, exists := idx.filesLower[strings.ToLower(fullPath)]; exists {
		displayName = disambiguateGalleryName(item.Name, item.ID)
		fullPath = dirPath + "/" + displayName
	}

	entry := galleryEntry{
		displayName: displayName,
		item:        item,
		modTime:     mt,
		year:        year,
		month:       month,
	}
	idx.files[fullPath] = entry
	idx.filesLower[strings.ToLower(fullPath)] = entry

	if idx.layout != "flat" {
		yPath := rootVirtual + "/" + year
		if _, ok := idx.dirs[yPath]; !ok {
			idx.dirs[yPath] = nil
			idx.dirModTimes[yPath] = mt
			idx.dirs[rootVirtual] = append(idx.dirs[rootVirtual], &VirtualFileInfo{
				name: year, mode: os.ModeDir | 0755, modTime: mt, isDir: true,
			})
		}
		if year != "unknown" {
			if _, ok := idx.dirs[dirPath]; !ok {
				idx.dirs[dirPath] = nil
				idx.dirModTimes[dirPath] = mt
				idx.dirs[yPath] = append(idx.dirs[yPath], &VirtualFileInfo{
					name: month, mode: os.ModeDir | 0755, modTime: mt, isDir: true,
				})
			}
		}
	}
	idx.dirs[dirPath] = append(idx.dirs[dirPath], &VirtualFileInfo{
		name: displayName, size: item.Size, mode: 0644, modTime: mt, isDir: false,
	})
	idx.dirModTimes[dirPath] = mt
	idx.dirModTimes[rootVirtual] = mt
}

func (mfs *MovistarFS) removeFromGalleryCache(id string) {
	if mfs == nil || id == "" {
		return
	}
	mfs.galleryMu.Lock()
	defer mfs.galleryMu.Unlock()
	idx := mfs.galleryCache
	if idx == nil {
		return
	}
	for k, e := range idx.files {
		if e.item.ID == id {
			delete(idx.files, k)
			delete(idx.filesLower, strings.ToLower(k))
			parent := path.Dir(k)
			if list, ok := idx.dirs[parent]; ok {
				filtered := make([]os.FileInfo, 0, len(list))
				for _, fi := range list {
					if fi.Name() != e.displayName {
						filtered = append(filtered, fi)
					}
				}
				idx.dirs[parent] = filtered
			}
		}
	}
}

func formatHumanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func encodePathSegments(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		return "/"
	}
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return "/" + strings.Join(parts, "/")
}

func (idx *galleryIndex) lookupCanonicalDir(cleanPath string, fold bool) (string, []os.FileInfo, bool) {
	if idx == nil {
		return "", nil, false
	}
	if entries, ok := idx.dirs[cleanPath]; ok {
		return cleanPath, entries, true
	}
	if fold {
		for k, entries := range idx.dirs {
			if strings.EqualFold(k, cleanPath) {
				return k, entries, true
			}
		}
	}
	return "", nil, false
}

type galleryHTMLRow struct {
	Icon string
	Href string
	Name string
	Size string
	Date string
}

type galleryHTMLView struct {
	Title string
	Icon  string
	Path  string
	Count int
	Rows  []galleryHTMLRow
}

var galleryBrowserTmpl = template.Must(template.New("gallery-browser").Parse(
	`<!DOCTYPE html><html lang="es"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>{{.Title}} - {{.Path}}</title>` +
		`<style>body{font-family:system-ui,-apple-system,sans-serif;background:#0f172a;color:#e2e8f0;margin:0;padding:24px}main{max-width:960px;margin:0 auto}h1{font-size:1.25rem;margin:0 0 16px;color:#38bdf8}table{width:100%;border-collapse:collapse;background:#1e293b;border-radius:8px;overflow:hidden}th,td{padding:10px 14px;text-align:left;border-bottom:1px solid #334155;font-size:.9rem}th{color:#94a3b8;font-weight:600}a{color:#7dd3fc;text-decoration:none}a:hover{text-decoration:underline}.num{text-align:right;white-space:nowrap;color:#cbd5e1}.dt{white-space:nowrap;color:#94a3b8}</style>` +
		`</head><body><main><h1>{{.Icon}} {{.Path}} ({{.Count}})</h1><table><thead><tr><th>Nombre</th><th class="num">Tamaño</th><th>Fecha</th></tr></thead><tbody>` +
		`{{range .Rows}}<tr><td>{{.Icon}} <a href="{{.Href}}">{{.Name}}</a></td><td class="num">{{.Size}}</td><td class="dt">{{.Date}}</td></tr>{{end}}` +
		`</tbody></table></main></body></html>`))

func (mfs *MovistarFS) resolveCanonicalFolder(cleanPath string) (string, int64, bool) {
	if mfs == nil {
		return "", 0, false
	}
	rootID, err := mfs.getRootFolderID()
	if err != nil {
		return "", 0, false
	}
	if cleanPath == "/" || cleanPath == "." {
		return "/", rootID, true
	}
	parts := strings.Split(strings.Trim(cleanPath, "/"), "/")
	currID := rootID
	fold := mfs.caseInsensitive()
	canonicalParts := make([]string, 0, len(parts))
	now := time.Now()

	for _, part := range parts {
		var matched *sapi.FolderItem
		mfs.cacheMu.RLock()
		for k, cf := range mfs.createdFolders {
			if exp, tomb := mfs.deletedTombstones[k]; tomb && now.Before(exp) {
				continue
			}
			if time.Since(cf.Time) < 60*time.Second && cf.Info.ParentID == currID {
				if cf.Info.Name == part || (fold && strings.EqualFold(cf.Info.Name, part)) {
					info := cf.Info
					matched = &info
					break
				}
			}
		}
		mfs.cacheMu.RUnlock()

		if matched == nil {
			subs, err := mfs.getSubfolders(currID)
			if err != nil {
				return "", 0, false
			}
			for i := range subs {
				if strings.EqualFold(subs[i].Name, ProxyInternalDir) {
					continue
				}
				if subs[i].Name == part {
					matched = &subs[i]
					break
				}
				if fold && matched == nil && strings.EqualFold(subs[i].Name, part) {
					matched = &subs[i]
				}
			}
		}
		if matched == nil {
			return "", 0, false
		}
		canonicalParts = append(canonicalParts, matched.Name)
		currID = matched.ID
	}
	return "/" + strings.Join(canonicalParts, "/"), currID, true
}

// serveGalleryBrowserHTML sirve una vista HTML navegable cuando se abre "/", una
// carpeta de «Mis Archivos» o una subcarpeta de /_gallery desde un navegador (GET / HEAD).
func (s *Server) serveGalleryBrowserHTML(w http.ResponseWriter, r *http.Request, urlPath string) bool {
	if s == nil || s.fs == nil {
		return false
	}
	cleanPath := path.Clean("/" + urlPath)

	var canonicalDir string
	var entries []os.FileInfo
	pageTitle := "Movistar Cloud"
	pageIcon := "📁"

	if s.fs.isGalleryPath(cleanPath) {
		if !s.fs.isGalleryVirtualDir(cleanPath) {
			return false
		}
		idx, err := s.fs.getGalleryIndex()
		if err != nil {
			http.Error(w, "Error cargando galería", http.StatusInternalServerError)
			return true
		}
		var ok bool
		canonicalDir, entries, ok = idx.lookupCanonicalDir(cleanPath, s.fs.caseInsensitive())
		if !ok {
			http.NotFound(w, r)
			return true
		}
		pageTitle = "Galería"
		pageIcon = "🖼️"
	} else {
		if cleanPath != "/" && cleanPath != "." {
			_, mediaInfo, isFolder, err := s.fs.resolvePath(cleanPath)
			if err != nil || !isFolder || mediaInfo != nil {
				return false
			}
		}
		var folderID int64
		var ok bool
		canonicalDir, folderID, ok = s.fs.resolveCanonicalFolder(cleanPath)
		if !ok {
			return false
		}
		dr := NewWebDAVDirReader(s.fs, canonicalDir, folderID)
		var err error
		entries, err = dr.Readdir(-1)
		if err != nil {
			http.Error(w, "Error listando directorio", http.StatusInternalServerError)
			return true
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			li := strings.ToLower(entries[i].Name())
			lj := strings.ToLower(entries[j].Name())
			if li != lj {
				return li < lj
			}
			return entries[i].Name() < entries[j].Name()
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return true
	}

	rows := make([]galleryHTMLRow, 0, len(entries)+1)
	parent := path.Dir(canonicalDir)
	if canonicalDir != "/" && canonicalDir != "." && parent != canonicalDir && parent != "." {
		rows = append(rows, galleryHTMLRow{
			Icon: "📁",
			Href: encodePathSegments(parent),
			Name: "..",
			Size: "-",
			Date: "-",
		})
	}

	for _, e := range entries {
		childPath := path.Clean(canonicalDir + "/" + e.Name())
		icon := "📄"
		sizeStr := formatHumanBytes(e.Size())
		if e.IsDir() {
			icon = "📁"
			sizeStr = "-"
		} else {
			switch strings.ToLower(path.Ext(e.Name())) {
			case ".mp4", ".mov", ".m4v", ".3gp", ".avi", ".mkv", ".webm":
				icon = "🎬"
			case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic", ".heif", ".bmp", ".tiff", ".tif", ".avif":
				icon = "🖼️"
			}
		}
		dateStr := "-"
		if !e.ModTime().IsZero() && e.ModTime().Year() > 1970 {
			dateStr = e.ModTime().Local().Format("2006-01-02 15:04")
		}
		rows = append(rows, galleryHTMLRow{
			Icon: icon,
			Href: encodePathSegments(childPath),
			Name: e.Name(),
			Size: sizeStr,
			Date: dateStr,
		})
	}

	w.WriteHeader(http.StatusOK)
	_ = galleryBrowserTmpl.Execute(w, galleryHTMLView{
		Title: pageTitle,
		Icon:  pageIcon,
		Path:  canonicalDir,
		Count: len(entries),
		Rows:  rows,
	})
	return true
}

// WebDAVGalleryDirReader implementa webdav.File para listar directorios virtuales de la galería.
type WebDAVGalleryDirReader struct {
	noopDeadProps
	mfs     *MovistarFS
	path    string
	idx     *galleryIndex
	entries []os.FileInfo
	readPos int
	loaded  bool
}

func NewWebDAVGalleryDirReader(mfs *MovistarFS, cleanPath string, idx *galleryIndex) *WebDAVGalleryDirReader {
	return &WebDAVGalleryDirReader{mfs: mfs, path: cleanPath, idx: idx}
}

func (d *WebDAVGalleryDirReader) Read(p []byte) (int, error)  { return 0, io.EOF }
func (d *WebDAVGalleryDirReader) Write(p []byte) (int, error) { return 0, fs.ErrPermission }
func (d *WebDAVGalleryDirReader) Close() error                { return nil }

func (d *WebDAVGalleryDirReader) Seek(offset int64, whence int) (int64, error) {
	if offset == 0 && whence == io.SeekStart {
		d.readPos = 0
		d.loaded = false
		return 0, nil
	}
	return 0, nil
}

func (d *WebDAVGalleryDirReader) Stat() (os.FileInfo, error) {
	fold := d.mfs != nil && d.mfs.caseInsensitive()
	return &VirtualFileInfo{
		name:    path.Base(d.path),
		size:    0,
		mode:    os.ModeDir | 0755,
		modTime: d.idx.lookupDirModTime(d.path, fold),
		isDir:   true,
	}, nil
}

func (d *WebDAVGalleryDirReader) Readdir(count int) ([]os.FileInfo, error) {
	if !d.loaded {
		if d.mfs != nil {
			d.mfs.MaybeTriggerSync()
		}
		fold := d.mfs != nil && d.mfs.caseInsensitive()
		if entries, ok := d.idx.lookupDir(d.path, fold); ok {
			d.entries = entries
		}
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
