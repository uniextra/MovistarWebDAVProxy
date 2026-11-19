package webdav

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"
)

// waitForCloudCopy busca en la carpeta destino un archivo con ese nombre y tamaño
// hasta que pasa el tiempo razonable para la copia. Devuelve su ID o "".
func waitForCloudCopy(ctx context.Context, mfs *MovistarFS, folderID int64, name string, size int64) string {
	wait := config.CopyWaitBase() + time.Duration(size/config.CopyWaitBytesPerSec())*time.Second
	if max := config.CopyWaitMax(); wait > max {
		wait = max
	}
	deadline := time.Now().Add(wait)
	for {
		if items, err := mfs.client.ListAllMediaInFolder(folderID); err == nil {
			for _, it := range items {
				if it.Name == name && (it.Size == size || size == 0) {
					return it.ID
				}
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		select {
		case <-time.After(config.CopyPollInterval()):
		case <-ctx.Done():
			return "" // el cliente se fue: no se sigue esperando para nadie
		}
	}
}

// tryServerCopy atiende un COPY de un archivo con la copia de la web
// (/media/file?action=copy), sin descargarlo ni volver a subirlo. Devuelve false
// para dejar el COPY al manejador normal: carpetas, destino ya existente, bloqueos,
// archivos virtuales o cualquier fallo de la copia en la nube.
func (s *Server) tryServerCopy(w http.ResponseWriter, r *http.Request) bool {
	mfs := s.fs
	if mfs == nil || mfs.client == nil {
		return false
	}
	u, err := url.Parse(r.Header.Get("Destination"))
	if err != nil || u.Path == "" || (u.Host != "" && u.Host != r.Host) {
		return false
	}
	src := path.Clean("/" + r.URL.Path)
	dst := path.Clean("/" + u.Path)
	if src == dst || strings.HasSuffix(r.URL.Path, "/") || isControlOrLockFile(src) || isControlOrLockFile(dst) {
		return false
	}

	_, media, isFolder, err := mfs.resolvePath(src)
	if err != nil || isFolder || media == nil || media.ID == "" || media.Size == 0 {
		return false
	}
	if _, _, _, err := mfs.resolvePath(dst); !errors.Is(err, os.ErrNotExist) {
		return false // destino existente (sobrescritura) o desconocido: manejador normal
	}
	parent, _, parentIsFolder, err := mfs.resolvePath(path.Dir(dst))
	if err != nil || !parentIsFolder || parent == nil || parent.ID <= 0 {
		return false
	}
	if r.Header.Get("If") != "" {
		return false // con bloqueos del cliente, que los compruebe el manejador normal
	}
	if s.lockSys != nil {
		// Como el manejador sin cabecera If: bloqueo temporal del destino para no
		// pisar el de otro cliente.
		token, err := s.lockSys.Create(time.Now(), webdav.LockDetails{Root: dst, Duration: -1, ZeroDepth: true})
		if err != nil {
			return false
		}
		defer func() { _ = s.lockSys.Unlock(time.Now(), token) }()
	}

	name := path.Base(dst)
	newID, err := mfs.client.CopyFile(media.ID, parent.ID, name)
	if errors.Is(err, sapi.ErrCopyOutcomeUnknown) {
		// La copia pudo hacerse aunque no llegara la respuesta: descargar y volver a
		// subir dejaría dos archivos con el mismo nombre (visto el 18/11/2026 con 3 GB).
		// Se espera a que aparezca en la carpeta; si no aparece, 503 para que el
		// cliente reintente (y entonces ya encontrará el destino si llegó a crearse).
		logger.Log("[WebDAV] ⏳ Copia en la nube de '%s' sin respuesta (%v); esperando a que aparezca en el destino...", src, err)
		if found := waitForCloudCopy(r.Context(), mfs, parent.ID, name, media.Size); found != "" {
			newID, err = found, nil
		} else {
			logger.Log("[WebDAV] ⏳ La copia de '%s' no apareció a tiempo; se pide al cliente que reintente.", src)
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Cloud copy still in progress; retry later", http.StatusServiceUnavailable)
			return true
		}
	}
	if err != nil {
		logger.Log("[WebDAV] ↪ Copia en la nube de '%s' rechazada (%v); se copia descargando y subiendo.", src, err)
		return false
	}

	copied := *media
	copied.ID, copied.Name, copied.FolderID, copied.URL = newID, name, parent.ID, ""
	if ms := mfs.MetadataStore(); ms != nil {
		logMetaErr("UpsertMedia", ms.UpsertMedia(copied))
		logMetaErr("UpdateFolderModificationDate", ms.UpdateFolderModificationDate(parent.ID, time.Now().UTC().Format("20060102T150405Z")))
	}
	mfs.InvalidateCache(parent.ID)
	// como tras una subida: validación y relectura para tener la URL firmada
	mfs.client.FollowCopiedMedia(newID, parent.ID)
	logger.Log("[WebDAV] 📄 Copiado en la nube: '%s' -> '%s' (ID: %s)", src, dst, newID)
	w.WriteHeader(http.StatusCreated)
	return true
}

// tryCaseOnlyRename atiende un MOVE que solo cambia las mayúsculas del nombre.
// x/net/webdav comprueba antes si el destino existe, y la búsqueda sin distinguir
// mayúsculas encuentra el propio origen: sin Overwrite respondía 412 y con
// "Overwrite: T" borraba el origen antes de moverlo. Si hay de verdad otro
// elemento con ese nombre exacto, se deja al manejador normal.
func (s *Server) tryCaseOnlyRename(w http.ResponseWriter, r *http.Request) bool {
	mfs := s.fs
	if mfs == nil {
		return false
	}
	u, err := url.Parse(r.Header.Get("Destination"))
	if err != nil || u.Path == "" || (u.Host != "" && u.Host != r.Host) {
		return false
	}
	src := path.Clean("/" + r.URL.Path)
	dst := path.Clean("/" + u.Path)
	if src == dst || !strings.EqualFold(src, dst) || r.Header.Get("If") != "" {
		return false
	}
	srcFolder, srcMedia, srcIsFolder, err := mfs.resolvePath(src)
	if err != nil {
		return false
	}
	dstFolder, dstMedia, _, dstErr := mfs.resolvePath(dst)
	if dstErr == nil {
		sameFile := !srcIsFolder && srcMedia != nil && dstMedia != nil && dstMedia.ID == srcMedia.ID
		sameFolder := srcIsFolder && srcFolder != nil && dstFolder != nil && dstFolder.ID == srcFolder.ID
		if !sameFile && !sameFolder {
			return false // otro elemento con ese nombre exacto: manejador normal
		}
	} else if !errors.Is(dstErr, os.ErrNotExist) {
		return false
	}
	if s.lockSys != nil {
		token, err := s.lockSys.Create(time.Now(), webdav.LockDetails{Root: dst, Duration: -1, ZeroDepth: true})
		if err != nil {
			return false
		}
		defer func() { _ = s.lockSys.Unlock(time.Now(), token) }()
	}
	if err := mfs.Rename(r.Context(), src, dst); err != nil {
		logger.Log("[WebDAV] ❌ Cambio de mayúsculas '%s' -> '%s' fallido: %v", src, dst, err)
		if errors.Is(err, os.ErrPermission) {
			http.Error(w, "Forbidden", http.StatusForbidden)
		} else {
			// fallo de O2 (validación, red, sesión…): que el cliente lo reintente
			w.Header().Set("Retry-After", "30")
			http.Error(w, "Rename failed in O2 Cloud; retry later", http.StatusServiceUnavailable)
		}
		return true
	}
	w.WriteHeader(http.StatusCreated)
	return true
}
