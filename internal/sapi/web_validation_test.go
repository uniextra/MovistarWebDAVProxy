package sapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Como la web: solo los archivos de más de 200 MiB se suben con validación asíncrona.
func TestUploadAcceptsAsync(t *testing.T) {
	cases := []struct {
		web  bool
		size int64
		want bool
	}{
		{true, 256 * 1024, false},
		{true, webAsyncUploadMin, false},
		{true, webAsyncUploadMin + 1, true},
		{true, 1 << 30, true},
		{false, 256 * 1024, true}, // el modo normal no cambia
	}
	for _, c := range cases {
		if got := uploadAcceptsAsync(c.web, c.size); got != c.want {
			t.Errorf("web=%v size=%d: %v, esperado %v", c.web, c.size, got, c.want)
		}
	}
}

// En modo web, ante MED-1017 se consulta el estado de validación hasta que deja de
// estar en "V" y se reintenta el movimiento sin esperas a ciegas.
func TestUpdateMedia_WebModeWaitsForValidationStatus(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	moves, polls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/sapi/media" && r.URL.Query().Get("action") == "get-validation-status":
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), `"id":"42"`) {
				t.Errorf("cuerpo inesperado: %s", b)
			}
			polls++
			calls = append(calls, "status")
			st := "V"
			if polls >= 3 {
				st = "U"
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"ids": []map[string]interface{}{{"id": 42, "status": st}}}})
		case strings.HasPrefix(r.URL.Path, "/sapi/upload/"):
			moves++
			calls = append(calls, "move")
			if moves == 1 {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]interface{}{"code": "MED-1017", "message": "Operation forbidden on a media not yet validated"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "42"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	oldPoll, oldDelay := mediaValidationPoll, mediaMoveRetryDelay
	mediaValidationPoll, mediaMoveRetryDelay = time.Millisecond, time.Hour // una espera a ciegas colgaría el test
	t.Cleanup(func() { mediaValidationPoll, mediaMoveRetryDelay = oldPoll, oldDelay })

	c := newWebTestClient(t, srv.URL)
	if err := c.UpdateMedia("42", "a.log", "a.log", 200); err != nil {
		t.Fatalf("esperado éxito: %v", err)
	}
	if got := strings.Join(calls, ","); got != "move,status,status,status,move" {
		t.Errorf("llamadas = %s", got)
	}
}
