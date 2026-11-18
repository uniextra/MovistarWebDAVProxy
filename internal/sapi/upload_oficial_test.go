package sapi

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
)

// fakeOfficialSAPI reproduce la subida de la app oficial tal como se capturó con
// Fiddler (18/11/2026): save-metadata, contenido en bruto con x-funambol-*,
// consulta de offset (308 + Range) y continuación con content-range.
type fakeOfficialSAPI struct {
	t  *testing.T
	mu sync.Mutex

	nextID     int
	meta       map[string]map[string]interface{}
	data       map[string][]byte
	cutAfter   map[int]int64 // n.º de POST de contenido → bytes que se aceptan antes de cortar
	posts      int
	metaBodies []map[string]interface{}
	metaHeader []http.Header
	postHeader []http.Header
	postKinds  []string
	queries    []http.Header
	deleted    []string
	validating map[string]int // id → consultas que faltan para darlo por validado
	valBodies  []string
	med1017    map[string]int // id → save-metadata que se rechazan con MED-1017
}

func newFakeOfficialSAPI(t *testing.T) (*fakeOfficialSAPI, *httptest.Server) {
	f := &fakeOfficialSAPI{
		t: t, nextID: 1597720466,
		meta: map[string]map[string]interface{}{}, data: map[string][]byte{},
		cutAfter: map[int]int64{}, validating: map[string]int{}, med1017: map[string]int{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeOfficialSAPI) serve(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	switch {
	case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save-metadata":
		f.saveMetadata(w, r)
	case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save" && r.Method == http.MethodGet:
		f.queryOffset(w, r)
	case strings.HasPrefix(r.URL.Path, "/sapi/upload/") && action == "save" && r.Method == http.MethodPost:
		f.saveContent(w, r)
	case r.URL.Path == "/sapi/media" && action == "get-validation-status":
		f.validationStatus(w, r)
	case strings.HasPrefix(r.URL.Path, "/sapi/media") && (action == "delete" || action == "softdelete"):
		f.mu.Lock()
		id := r.URL.Query().Get("id")
		if id == "" {
			b, _ := io.ReadAll(r.Body)
			id = string(b)
		}
		f.deleted = append(f.deleted, id)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"success":"Item successfully moved to trash"}`))
	default:
		f.t.Errorf("petición inesperada: %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeOfficialSAPI) saveMetadata(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("save-metadata con cuerpo no JSON: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metaBodies = append(f.metaBodies, body.Data)
	f.metaHeader = append(f.metaHeader, r.Header.Clone())
	id := ""
	if v, ok := body.Data["id"]; ok {
		id = fmt.Sprint(v)
		if f.med1017[id] > 0 {
			f.med1017[id]--
			_, _ = w.Write([]byte(`{"error":{"code":"MED-1017","message":"Operation forbidden on a media not yet validated","parameters":[],"cause":""}}`))
			return
		}
	} else {
		id = strconv.Itoa(f.nextID)
		f.nextID++
	}
	f.meta[id] = body.Data
	f.data[id] = nil
	fmt.Fprintf(w, `{"success":"Media metadata saved successfully","id":"%s","lastupdate":1795019796220,"responsetime":1795019796227}`, id)
}

func (f *fakeOfficialSAPI) queryOffset(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, r.Header.Clone())
	id := r.Header.Get("x-funambol-id")
	got := len(f.data[id])
	if got > 0 {
		w.Header().Set("Range", fmt.Sprintf("0-%d", got-1))
	}
	w.WriteHeader(http.StatusPermanentRedirect)
	_, _ = w.Write([]byte("<!DOCTYPE html><html>…</html>"))
}

func (f *fakeOfficialSAPI) saveContent(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.posts++
	n := f.posts
	cut, hasCut := f.cutAfter[n]
	f.postHeader = append(f.postHeader, r.Header.Clone())
	f.postKinds = append(f.postKinds, strings.TrimPrefix(r.URL.Path, "/sapi/upload/"))
	id := r.Header.Get("x-funambol-id")
	start := int64(0)
	if cr := r.Header.Get("content-range"); cr != "" {
		var end, total int64
		if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total); err != nil {
			f.t.Errorf("content-range inválido %q: %v", cr, err)
		}
		if start != int64(len(f.data[id])) {
			f.t.Errorf("content-range empieza en %d pero O2 tiene %d", start, len(f.data[id]))
		}
	} else {
		f.data[id] = nil
	}
	f.mu.Unlock()

	if hasCut {
		buf := make([]byte, cut)
		k, _ := io.ReadFull(r.Body, buf)
		f.mu.Lock()
		f.data[id] = append(f.data[id], buf[:k]...)
		f.mu.Unlock()
		hj, _ := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.data[id] = append(f.data[id], b...)
	f.mu.Unlock()
	fmt.Fprintf(w, `{"success":"Media uploaded successfully","id":"%s","lastupdate":1795019796220,"status":"V","etag":"eDw2tSxQNqE2cGmZ"}`, id)
}

func (f *fakeOfficialSAPI) validationStatus(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valBodies = append(f.valBodies, string(b))
	var body struct {
		Data struct {
			IDs []map[string]interface{} `json:"ids"`
		} `json:"data"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	_ = dec.Decode(&body)
	var out []map[string]interface{}
	for _, it := range body.Data.IDs {
		id := fmt.Sprint(it["id"])
		st := "U"
		if f.validating[id] > 0 {
			f.validating[id]--
			st = "V"
		}
		out = append(out, map[string]interface{}{"id": id, "status": st})
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"ids": out}})
}

func newOfficialTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	prevBackoff, prevProbe, prevPoll := officialUploadBackoff, officialValidationProbe, mediaValidationPoll
	officialUploadBackoff = func(int) time.Duration { return 0 }
	officialValidationProbe = 0
	mediaValidationPoll = 5 * time.Millisecond
	t.Cleanup(func() {
		officialUploadBackoff, officialValidationProbe, mediaValidationPoll = prevBackoff, prevProbe, prevPoll
	})
	c, err := NewClient(config.AccountConfig{Provider: "o2", AccessToken: "at", RefreshToken: "rt", ValidationKey: "vk", JSessionID: "js", DeviceID: "fol-abc"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetBaseURLs(srv.URL+"/sapi", srv.URL)
	c.SetPacingInterval(0)
	return c
}

func randomPayload(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOfficialUpload_NewFileLikeTheApp(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	payload := randomPayload(t, 4096)
	mod := time.Date(2026, 11, 18, 16, 36, 28, 0, time.UTC)

	item, err := c.UploadFileToFolder("corte-subida-har.bin", bytes.NewReader(payload), int64(len(payload)), 37224270, "", "", mod)
	if err != nil {
		t.Fatalf("subida: %v", err)
	}
	if item.ID != "1597720466" {
		t.Fatalf("ID = %q", item.ID)
	}
	if !bytes.Equal(f.data[item.ID], payload) {
		t.Fatalf("O2 recibió %d bytes distintos del original", len(f.data[item.ID]))
	}

	meta := f.metaBodies[0]
	for _, k := range []string{"contenttype", "creationdate", "folderid", "modificationdate", "name", "size"} {
		if _, ok := meta[k]; !ok {
			t.Errorf("save-metadata sin %q: %v", k, meta)
		}
	}
	if _, ok := meta["id"]; ok {
		t.Errorf("save-metadata de un archivo nuevo no debe llevar id: %v", meta)
	}
	if meta["contenttype"] != "application/octet-stream" || meta["modificationdate"] != "20261118T163628Z" {
		t.Errorf("save-metadata = %v", meta)
	}
	if got := f.metaHeader[0].Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("content-type de save-metadata = %q", got)
	}

	h := f.postHeader[0]
	if h.Get("x-funambol-id") != item.ID || h.Get("x-funambol-file-size") != "4096" {
		t.Errorf("cabeceras x-funambol = %v", h)
	}
	if h.Get("content-range") != "" {
		t.Errorf("la primera subida no lleva content-range: %q", h.Get("content-range"))
	}
	if strings.Contains(h.Get("Content-Type"), "multipart") {
		t.Errorf("el contenido debe ir en bruto, no multipart: %q", h.Get("Content-Type"))
	}
	if f.postKinds[0] != "file" {
		t.Errorf("tipo de subida = %q", f.postKinds[0])
	}
}

func TestOfficialUpload_UsesPictureForHEIC(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	if _, err := c.UploadFileToFolder("IMG_7401.HEIC", bytes.NewReader([]byte("heic")), 4, 1, "image/heic", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if f.postKinds[0] != "picture" {
		t.Fatalf("HEIC subido como %q, la app usa picture", f.postKinds[0])
	}
}

func TestOfficialUpload_OverwriteKeepsID(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	item, err := c.UploadFileToFolder("_Syno_TaskConfig", bytes.NewReader([]byte("v2")), 2, 9, "", "1597568679", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != "1597568679" {
		t.Fatalf("ID = %q", item.ID)
	}
	meta := f.metaBodies[0]
	if fmt.Sprint(meta["id"]) != "1597568679" {
		t.Fatalf("save-metadata de sobrescritura sin id: %v", meta)
	}
	if _, ok := meta["creationdate"]; ok {
		t.Errorf("una sobrescritura no cambia la fecha de creación: %v", meta)
	}
}

// Caso de Hyper Backup (MED-1017): se sobrescribe un archivo subido hace segundos que
// O2 aún valida. En lugar de reintentar a ciegas y acabar en 405, se pregunta su
// estado como hace la app (con folder_id) y se sobrescribe en cuanto está validado.
func TestOfficialUpload_OverwriteWaitsValidationInsteadOfFailing(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	f.med1017["1597568679"] = 1
	f.validating["1597568679"] = 3

	if _, err := c.UploadFileToFolder("_Syno_TaskConfig", bytes.NewReader([]byte("v2")), 2, 37244653, "", "1597568679", time.Time{}); err != nil {
		t.Fatalf("la sobrescritura debía esperar a la validación: %v", err)
	}
	if len(f.valBodies) < 4 {
		t.Fatalf("consultas de validación = %d, se esperaban al menos 4", len(f.valBodies))
	}
	if !strings.Contains(f.valBodies[0], `"folder_id":37244653`) {
		t.Errorf("la consulta de validación debe llevar folder_id como la app: %s", f.valBodies[0])
	}
}

// Corte a mitad del envío con el archivo en disco: se pregunta a O2 cuánto tiene
// (308 + Range) y se manda solo el resto con content-range, como la app.
func TestOfficialUpload_ResumesAfterCut(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	payload := randomPayload(t, 1<<20)
	f.cutAfter[1] = 300000

	item, err := c.UploadFileToFolder("corte.bin", bytes.NewReader(payload), int64(len(payload)), 1, "", "", time.Time{})
	if err != nil {
		t.Fatalf("subida tras corte: %v", err)
	}
	if !bytes.Equal(f.data[item.ID], payload) {
		t.Fatalf("contenido final distinto (%d bytes)", len(f.data[item.ID]))
	}
	if len(f.queries) != 1 || f.queries[0].Get("content-range") != fmt.Sprintf("bytes */%d", len(payload)) {
		t.Fatalf("consulta de offset = %v", f.queries)
	}
	if got := f.postHeader[1].Get("content-range"); got != fmt.Sprintf("bytes 300000-%d/%d", len(payload)-1, len(payload)) {
		t.Fatalf("content-range de la continuación = %q", got)
	}
	if len(f.metaBodies) != 1 {
		t.Fatalf("save-metadata llamado %d veces; la continuación usa el mismo ID", len(f.metaBodies))
	}
}

// Subida en flujo (sin poder rebobinar) cortada: se devuelve el ID a medias para
// que el reintento desde disco continúe sobre él, sin crear otro elemento.
func TestOfficialUpload_StreamCutThenContinueFromDisk(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	payload := randomPayload(t, 1<<20)
	f.cutAfter[1] = 500000

	_, err := c.UploadFileToFolder("flujo.bin", io.MultiReader(bytes.NewReader(payload)), int64(len(payload)), 1, "", "", time.Time{})
	var ie *UploadInterruptedError
	if !errors.As(err, &ie) || ie.MediaID == "" || !ie.New {
		t.Fatalf("se esperaba UploadInterruptedError con ID nuevo, llegó %v", err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("no debe retirar el elemento mientras quede el reintento desde disco: %v", f.deleted)
	}

	item, err := c.UploadFileContinue(ie.MediaID, ie.New, "flujo.bin", bytes.NewReader(payload), int64(len(payload)), 1, "", time.Time{})
	if err != nil {
		t.Fatalf("continuación: %v", err)
	}
	if item.ID != ie.MediaID || !bytes.Equal(f.data[item.ID], payload) {
		t.Fatalf("la continuación no completó el mismo ID")
	}
	if len(f.metaBodies) != 1 {
		t.Fatalf("save-metadata llamado %d veces", len(f.metaBodies))
	}
	if len(f.postHeader) != 2 || f.postHeader[1].Get("content-range") != fmt.Sprintf("bytes 500000-%d/%d", len(payload)-1, len(payload)) {
		t.Fatalf("la continuación debe mandar solo el resto")
	}
}

// Si al final no se consigue, el elemento nuevo se retira: si se quedara, la nube
// mostraría un archivo con el tamaño completo y el contenido a medias.
func TestOfficialUpload_DiscardsNewItemWhenItFinallyFails(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	for i := 1; i <= officialUploadAttempts; i++ {
		f.cutAfter[i] = 10
	}
	_, err := c.UploadFileToFolder("nunca.bin", bytes.NewReader(randomPayload(t, 100000)), 100000, 1, "", "", time.Time{})
	if err == nil {
		t.Fatal("debía fallar")
	}
	if len(f.deleted) != 1 {
		t.Fatalf("elementos retirados = %v, se esperaba el creado", f.deleted)
	}
}

func TestOfficialUpload_FailedOverwriteNeverDeletesExistingFile(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	for i := 1; i <= officialUploadAttempts; i++ {
		f.cutAfter[i] = 10
	}
	f.meta["1597568679"] = map[string]interface{}{}
	_, err := c.UploadFileToFolder("existe.bin", bytes.NewReader(randomPayload(t, 100000)), 100000, 1, "", "1597568679", time.Time{})
	if err == nil {
		t.Fatal("debía fallar")
	}
	if len(f.deleted) != 0 {
		t.Fatalf("nunca se borra un archivo que ya existía: %v", f.deleted)
	}
}

func TestParseReceivedRange(t *testing.T) {
	cases := map[string]int64{"0-220391819": 220391820, "bytes=0-9": 10, "": 0, "basura": 0, "0-999": 0}
	for in, want := range cases {
		if got := parseReceivedRange(in, 500); in == "0-220391819" {
			if got := parseReceivedRange(in, 419430400); got != want {
				t.Errorf("%q → %d, esperado %d", in, got, want)
			}
		} else if got != want {
			t.Errorf("%q → %d, esperado %d", in, got, want)
		}
	}
}

// Releer tras subir es opcional (SAPI_REFRESH_AFTER_UPLOAD): por defecto, ninguna
// llamada extra; activado, una media?action=get como la app y la URL firmada.
func TestOfficialUpload_RefreshAfterUploadIsOptional(t *testing.T) {
	for _, on := range []bool{false, true} {
		var gets int
		var mu sync.Mutex
		f, srv := newFakeOfficialSAPI(t)
		_ = f
		wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/sapi/media" && r.URL.Query().Get("action") == "get" {
				mu.Lock()
				gets++
				mu.Unlock()
				_, _ = w.Write([]byte(`{"data":{"media":[{"id":"1597720466","name":"x.bin","url":"https://cloud/firmada"}]}}`))
				return
			}
			f.serve(w, r)
		}))
		t.Cleanup(wrapped.Close)
		_ = srv
		c := newOfficialTestClient(t, wrapped)
		if on {
			t.Setenv("SAPI_REFRESH_AFTER_UPLOAD", "true")
		} else {
			t.Setenv("SAPI_REFRESH_AFTER_UPLOAD", "")
		}
		item, err := c.UploadFileToFolder("x.bin", bytes.NewReader([]byte("abc")), 3, 1, "", "", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		n := gets
		mu.Unlock()
		if on && (n != 1 || item.URL != "https://cloud/firmada") {
			t.Errorf("activado: relecturas=%d url=%q", n, item.URL)
		}
		if !on && n != 0 {
			t.Errorf("desactivado: %d relecturas, se esperaba ninguna", n)
		}
	}
}
