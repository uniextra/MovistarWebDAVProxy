package webdav

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestIsExpectedNotFound(t *testing.T) {
	notFound := fmt.Errorf("stat: %w", os.ErrNotExist)
	for _, m := range []string{"PROPFIND", "HEAD", "GET", "MKCOL", "DELETE"} {
		if !isExpectedNotFound(m, notFound) {
			t.Errorf("%s con 'no existe' debería silenciarse", m)
		}
	}
	for _, m := range []string{"MOVE", "PUT"} {
		if isExpectedNotFound(m, notFound) {
			t.Errorf("%s con 'no existe' debe registrarse", m)
		}
	}
	if isExpectedNotFound("PROPFIND", errors.New("upstream 503")) {
		t.Error("otros errores en PROPFIND deben registrarse")
	}
}
