package webdav

import (
	"path"
	"strings"
)

// isUnsupportedTempPath identifica extensiones rechazadas por SAPI (issue #13,
// MED-1000). Incluye la sonda duplicati-access-privileges-test.tmp: un test de
// conexión no debe simular que se ha almacenado un archivo únicamente en RAM.
func isUnsupportedTempPath(rawPath string) bool {
	base := strings.TrimSpace(path.Base(path.Clean("/" + rawPath)))
	return strings.HasSuffix(strings.ToLower(base), ".tmp")
}
