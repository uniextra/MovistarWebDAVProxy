package webdav

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"movistar-webdav-proxy/internal/logger"
)

// tryServerCopy atiende un COPY de un archivo con la copia de la web
// (/media/file?action=copy), sin descargarlo ni volver a subirlo. Devuelve false
// para dejar el COPY al manejador normal: carpetas, destino ya existente, bloqueos,
// archivos virtuales o cualquier fallo de la copia en la nube.
func (s *Server) tryServerCopy(w http.ResponseWriter, r *http.Request) bool {
	mfs := s.fs
	if mfs == nil || mfs.client == nil || !mfs.client.WebMode() {
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
	logger.Log("[WebDAV] 📄 Copiado en la nube: '%s' -> '%s' (ID: %s)", src, dst, newID)
	w.WriteHeader(http.StatusCreated)
	return true
}
