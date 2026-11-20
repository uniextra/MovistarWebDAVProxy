package sapi

import (
	"strings"

	"movistar-webdav-proxy/internal/config"
)

// Nombres de archivo que O2 no admite tal cual (comprobado el 19 y 20/11/2026):
//
//   - los que empiezan por punto (.env, .gitignore, .DS_Store): COM-1011 «Missing
//     required parameter» al registrarlos;
//   - los que terminan en .tmp, sin distinguir mayúsculas: MED-1000;
//   - los que terminan en punto: O2 los guarda sin él («x.» queda «x»).
//
// Para poder guardarlos, el proxy escribe ese punto como «%2E» en la nube y lo
// recupera al leer: «.env» ↔ «%2Eenv», «x.» ↔ «x%2E», «x.tmp» ↔ «x%2Etmp». O2 admite
// esos nombres (comprobado el 20/11/2026). Solo se cambia ese punto concreto; el resto
// del nombre queda igual, así que en la web de O2 se reconoce el archivo. Las carpetas
// no lo necesitan: O2 admite «.git», «carpeta.» y «c.tmp».
//
// Un archivo que ya estuviera en la nube con «%2E» en esas posiciones se mostraría con
// el punto. Con SAPI_ESCAPE_NAMES=false no se traduce nada.

const escapedDot = "%2E"

// toCloudName traduce un nombre de archivo de WebDAV al que se guarda en la nube.
func toCloudName(name string) string {
	if name == "" || !config.EscapeNames() {
		return name
	}
	if strings.HasPrefix(name, ".") {
		name = escapedDot + name[1:]
	}
	if strings.HasSuffix(name, ".") {
		name = name[:len(name)-1] + escapedDot
	}
	if n := len(name); n >= 4 && strings.EqualFold(name[n-4:], ".tmp") {
		name = name[:n-4] + escapedDot + name[n-3:]
	}
	return name
}

// fromCloudName deshace toCloudName con un nombre de archivo leído de la nube.
func fromCloudName(name string) string {
	if !strings.Contains(name, escapedDot) || !config.EscapeNames() {
		return name
	}
	if strings.HasPrefix(name, escapedDot) {
		name = "." + name[len(escapedDot):]
	}
	if strings.HasSuffix(name, escapedDot) {
		name = name[:len(name)-len(escapedDot)] + "."
	}
	if n := len(name); n >= 6 && strings.EqualFold(name[n-3:], "tmp") && name[n-6:n-3] == escapedDot {
		name = name[:n-6] + "." + name[n-3:]
	}
	return name
}
