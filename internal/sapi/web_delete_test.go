package sapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// En modo web, el borrado de un archivo es el de la web: a la papelera con
// /media/<tipo>?action=delete&softdelete=true y {"data":{"files":[id]}}.
func TestDeleteFile_WebModeUsesWebCall(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery+" "+string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":"Media deleted"}`)
	}))
	t.Cleanup(srv.Close)
	c := newWebTestClient(t, srv.URL)
	if err := c.DeleteFile("1234567890", "archivo-chunk001_tmp000"); err != nil {
		t.Fatal(err)
	}
	want := `/sapi/media/file?action=delete&softdelete=true {"data":{"files":[1234567890]}}`
	if len(got) != 1 || got[0] != want {
		t.Errorf("llamadas = %q, esperado %q", got, want)
	}
}

// Si la web rechaza la operación del tipo, se usa la genérica de siempre; un 404 de la
// operación por tipo no se toma como "ya borrado".
func TestDeleteFile_WebModeFallsBackToGeneric(t *testing.T) {
	for _, typed := range []string{"error", "404"} {
		var mu sync.Mutex
		var got []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			got = append(got, r.URL.Path+"?"+r.URL.RawQuery)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if strings.HasPrefix(r.URL.Path, "/sapi/media/") {
				if typed == "404" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = io.WriteString(w, `{"error":{"code":"MED-1005","message":"Unable to retrieve media with given id"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"success":"Media soft deleted successfully"}`)
		}))
		c := newWebTestClient(t, srv.URL)
		if err := c.DeleteFile("42", "a.mp4"); err != nil {
			t.Fatalf("%s: %v", typed, err)
		}
		if len(got) != 2 || got[0] != "/sapi/media/video?action=delete&softdelete=true" || got[1] != "/sapi/media?action=softdelete&id=42" {
			t.Errorf("%s: llamadas = %q", typed, got)
		}
		srv.Close()
	}
}

// Carpetas en modo web: a la papelera con {"data":{"ids":[id]}}; FOL-1036 se
// devuelve como ErrFolderUpdatesLimit sin probar la llamada genérica.
func TestDeleteFolder_WebMode(t *testing.T) {
	for _, c := range []struct {
		reply string
		calls int
		want  error
	}{{`{"success":"ok"}`, 1, nil}, {`{"error":{"code":"FOL-1036","message":"limit"}}`, 1, ErrFolderUpdatesLimit}} {
		var got []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = append(got, r.URL.Path+"?"+r.URL.RawQuery+" "+string(b))
			_, _ = io.WriteString(w, c.reply)
		}))
		cl := newWebTestClient(t, srv.URL)
		if err := cl.DeleteFolder(77, false); err != c.want {
			t.Errorf("%s: err=%v", c.reply, err)
		}
		if len(got) != c.calls || got[0] != `/sapi/media/folder?action=softdelete {"data":{"ids":[77]}}` {
			t.Errorf("%s: llamadas %q", c.reply, got)
		}
		srv.Close()
	}
}
