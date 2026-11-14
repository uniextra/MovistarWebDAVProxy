package webdav

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"movistar-webdav-proxy/internal/config"
)

func TestTooLongName(t *testing.T) {
	ok := strings.Repeat("a", 255)
	long := strings.Repeat("a", 256)
	accents := strings.Repeat("á", 255) // 255 caracteres (510 bytes)
	cases := map[string]string{
		"/x/" + ok + "/y.bin":   "",
		"/x/" + long + "/y.bin": long,
		"/x/y/" + long:          long,
		"/" + accents:           "",
		"/" + accents + "é":     accents + "é",
	}
	for p, want := range cases {
		if got := tooLongName(p); got != want {
			t.Errorf("%d caracteres: got %d, esperado %d", len([]rune(p)), len([]rune(got)), len([]rune(want)))
		}
	}
}

// PUT, MKCOL y el destino de MOVE/COPY con un nombre de más de 255 caracteres se
// rechazan con 400 antes de llegar a SAPI, que los recortaría sin avisar.
func TestRejectLongNames_BeforeSAPI(t *testing.T) {
	srv := NewServerWithAuth(nil, config.AuthConfig{AuthDisabled: true})
	long := strings.Repeat("x", 256)
	reqs := []*http.Request{
		httptest.NewRequest("PUT", "/datos/"+long, strings.NewReader("hola")),
		httptest.NewRequest("MKCOL", "/datos/"+long+"/", nil),
	}
	mv := httptest.NewRequest("MOVE", "/datos/a.bin", nil)
	mv.Header.Set("Destination", "http://example.com/datos/"+long)
	cp := httptest.NewRequest("COPY", "/datos/a.bin", nil)
	cp.Header.Set("Destination", "http://example.com/datos/"+long)
	reqs = append(reqs, mv, cp)
	for _, r := range reqs {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, esperado 400", r.Method, rec.Code)
		}
	}
}
