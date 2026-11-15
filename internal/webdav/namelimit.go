package webdav

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"movistar-webdav-proxy/internal/logger"
)

// maxNameLength es la longitud máxima de un nombre de archivo o carpeta en
// Movistar Cloud. SAPI no rechaza los nombres más largos: los guarda recortados a
// 255 caracteres sin avisar, y el cliente cree que la subida fue bien. Con nombres
// cifrados (p. ej. rclone crypt) el recorte los deja indescifrables.
const maxNameLength = 255

// tooLongName devuelve el primer componente de la ruta que supera maxNameLength.
func tooLongName(p string) string {
	for _, part := range strings.Split(p, "/") {
		if utf8.RuneCountInString(part) > maxNameLength {
			return part
		}
	}
	return ""
}

// rejectLongNames responde 400 si la petición crearía un archivo o carpeta con un
// nombre que Movistar Cloud recortaría: la ruta en PUT y MKCOL, y el destino en
// MOVE y COPY. Devuelve true si la ha rechazado.
func rejectLongNames(w http.ResponseWriter, r *http.Request) bool {
	var target string
	switch r.Method {
	case http.MethodPut, "MKCOL":
		target = r.URL.Path
	case "MOVE", "COPY":
		u, err := url.Parse(r.Header.Get("Destination"))
		if err != nil {
			return false
		}
		target = u.Path
	default:
		return false
	}
	name := tooLongName(target)
	if name == "" {
		return false
	}
	logRejectedLongName(r.Method, target, utf8.RuneCountInString(name))
	http.Error(w, "Nombre de archivo o carpeta demasiado largo: Movistar Cloud admite hasta 255 caracteres", http.StatusBadRequest)
	return true
}

func logRejectedLongName(method, target string, n int) {
	logger.Log("[WebDAV] ❌ %s rechazado: un nombre de la ruta tiene %d caracteres (máximo %d en Movistar Cloud): '%s'", method, n, maxNameLength, target)
}
