package sapi

import (
	"bytes"
	"context"
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
	"sync/atomic"
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
	med1017    map[string]int    // id → save-metadata que se rechazan con MED-1017
	reject     map[string]string // tipo (upload/<tipo>) → código con el que O2 rechaza el contenido
	rejectN    map[string]int    // tipo → rechazos que faltan (sin entrada: siempre)
}

func newFakeOfficialSAPI(t *testing.T) (*fakeOfficialSAPI, *httptest.Server) {
	f := &fakeOfficialSAPI{
		t: t, nextID: 1597720466,
		meta: map[string]map[string]interface{}{}, data: map[string][]byte{},
		cutAfter: map[int]int64{}, validating: map[string]int{}, med1017: map[string]int{},
		reject: map[string]string{}, rejectN: map[string]int{},
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
	case r.URL.Path == "/sapi/media" && action == "get":
		// relectura tras subir, como la app (sin URL: el proxy conserva lo que tenía)
		_, _ = w.Write([]byte(`{"data":{"media":[]}}`))
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
	kind := strings.TrimPrefix(r.URL.Path, "/sapi/upload/")
	code := f.reject[kind]
	if n, limited := f.rejectN[kind]; code != "" && limited {
		if n <= 1 {
			delete(f.reject, kind)
		}
		f.rejectN[kind] = n - 1
	}
	f.mu.Unlock()
	if code != "" {
		// Como O2 con un .jpg que no es una imagen: recibe todo y rechaza el contenido.
		fmt.Fprintf(w, `{"error":{"code":"%s","message":"The file extension or the format is not supported.","parameters":[],"cause":""}}`, code)
		return
	}
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
	prevBackoff, prevPoll := officialUploadBackoff, mediaValidationPoll
	officialUploadBackoff = func(int) time.Duration { return 0 }
	mediaValidationPoll = 5 * time.Millisecond
	t.Cleanup(func() {
		officialUploadBackoff, mediaValidationPoll = prevBackoff, prevPoll
	})
	c, err := NewClient(config.AccountConfig{Provider: "o2", AccessToken: "at", RefreshToken: "rt", ValidationKey: "vk", JSessionID: "js", DeviceID: "fol-abc"},
		WithValidationPolling(5*time.Millisecond, 5*time.Millisecond))
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
	for i := 1; i <= config.UploadAttempts(); i++ {
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
	for i := 1; i <= config.UploadAttempts(); i++ {
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
	cases := []struct {
		in   string
		size int64
		want int64
	}{
		{"0-220391819", 419430400, 220391820},
		{"bytes=0-9", 500, 10},
		{"", 500, 0},
		{"basura", 500, 0},
		{"0-999", 500, 0},   // más de lo que mide: no se fía
		{"0-499", 500, 499}, // todo recibido sin cerrar: se reenvía el último byte (rango válido)
	}
	for _, c := range cases {
		if got := parseReceivedRange(c.in, c.size); got != c.want {
			t.Errorf("%q (tamaño %d) → %d, esperado %d", c.in, c.size, got, c.want)
		}
	}
}

// Releer tras subir, como la app (captura del 18/11/2026): con el archivo en
// validación (V), primero get-validation-status hasta que termina y después
// media?action=get sin «origin», con los campos de la app y el ID como número; la
// URL firmada llega por el hook. Con SAPI_REFRESH_AFTER_UPLOAD=false, sin relectura.
func TestOfficialUpload_RefreshAfterValidationLikeTheApp(t *testing.T) {
	for _, on := range []bool{true, false} {
		f, _ := newFakeOfficialSAPI(t)
		var mu sync.Mutex
		var order []string
		var getBody, getQuery, getCT string
		wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			action := r.URL.Query().Get("action")
			if r.URL.Path == "/sapi/media" && (action == "get" || action == "get-validation-status") {
				mu.Lock()
				order = append(order, action)
				mu.Unlock()
			}
			if r.URL.Path == "/sapi/media" && action == "get" {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				getBody, getQuery, getCT = string(b), r.URL.RawQuery, r.Header.Get("Content-Type")
				mu.Unlock()
				_, _ = w.Write([]byte(`{"data":{"media":[{"id":"1597720466","name":"x.bin","url":"https://cloud/firmada","etag":"e2"}]}}`))
				return
			}
			f.serve(w, r)
		}))
		t.Cleanup(wrapped.Close)
		c := newOfficialTestClient(t, wrapped)
		if on {
			t.Setenv("SAPI_REFRESH_AFTER_UPLOAD", "")
		} else {
			t.Setenv("SAPI_REFRESH_AFTER_UPLOAD", "false")
		}
		f.validating["1597720466"] = 2
		got := make(chan MediaRefresh, 1)
		c.SetMediaRefreshHook(func(r MediaRefresh) { got <- r })

		if _, err := c.UploadFileToFolder("x.bin", bytes.NewReader([]byte("abc")), 3, 1, "", "", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if !on {
			time.Sleep(200 * time.Millisecond)
			mu.Lock()
			for _, o := range order {
				if o == "get" {
					t.Errorf("desactivado: hubo relectura (%v)", order)
				}
			}
			mu.Unlock()
			continue
		}
		select {
		case r := <-got:
			if r.URL != "https://cloud/firmada" || r.ID != "1597720466" || r.ETag != "e2" {
				t.Errorf("relectura = %+v", r)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no llegó la relectura")
		}
		mu.Lock()
		want := []string{"get-validation-status", "get-validation-status", "get-validation-status", "get"}
		if fmt.Sprint(order) != fmt.Sprint(want) {
			t.Errorf("orden = %v; la app relee cuando termina la validación: %v", order, want)
		}
		if getBody != `{"data":{"fields":["name","url","creationdate","modificationdate","size","etag","folderid"],"ids":[1597720466]}}` {
			t.Errorf("cuerpo de la relectura = %s", getBody)
		}
		if strings.Contains(getQuery, "origin") || getCT != "application/json" {
			t.Errorf("relectura: query=%q content-type=%q", getQuery, getCT)
		}
		mu.Unlock()
	}
}

// Sobrescritura como la app (captura del 18/11/2026): id, size, modificationdate
// nuevos y la creationdate ORIGINAL del archivo.
func TestOfficialUpload_ReplacingSendsOriginalCreationDate(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	existing := MediaItem{ID: "1598146690", Name: "sobrescritura.txt", CreationDate: "20261118T193902Z"}
	if _, err := c.UploadFileReplacing(existing, "sobrescritura.txt", bytes.NewReader([]byte("version 2")), 9, 37224270, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	meta := f.metaBodies[0]
	for _, k := range []string{"contenttype", "creationdate", "folderid", "id", "modificationdate", "name", "size"} {
		if _, ok := meta[k]; !ok {
			t.Errorf("falta %q: %v", k, meta)
		}
	}
	if meta["creationdate"] != "20261118T193902Z" || fmt.Sprint(meta["id"]) != "1598146690" {
		t.Errorf("save-metadata = %v", meta)
	}
}

// Si el cliente se va (rclone corta la espera a los 5 min), la subida a O2 se
// aborta enseguida y el elemento nuevo a medias se retira. En la prueba real el
// proxy siguió subiendo 6 minutos para nadie y se cruzó con el DELETE del cliente.
func TestOfficialUpload_ClientGoneAbortsAndDiscards(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	officialUploadBackoff = func(int) time.Duration { return 5 * time.Second }
	f.cutAfter[1] = 1000

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := c.UploadFileToFolderCtx(ctx, "abandonado.bin", bytes.NewReader(randomPayload(t, 100000)), 100000, 1, "", "", time.Time{})
	if err == nil {
		t.Fatal("debía fallar al irse el cliente")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("tardó %v en abortar; debía cortar la espera al irse el cliente", d)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deleted) != 1 {
		t.Fatalf("el elemento a medias debe retirarse: %v", f.deleted)
	}
}

// El seguimiento de la validación no tiene límite propio: sigue hasta que O2 termina
// (aquí, muchas más consultas que las que cabían en los 2 min de antes, a escala) o
// deja de conocer el archivo. Mientras dura, WaitUploadSettled espera; si el que
// espera se va (ctx), deja de esperar sin cortar el seguimiento.
func TestOfficialUpload_ValidationFollowUpHasNoDeadline(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	t.Setenv("SAPI_REFRESH_AFTER_UPLOAD", "false")
	f.validating["1597720466"] = 40 // ~200 ms en «V»: muchas más consultas que la espera límite de antes, a escala

	if _, err := c.UploadFileToFolder("x.bin", bytes.NewReader([]byte("abc")), 3, 1, "", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !c.UploadSettling("1597720466") {
		t.Fatal("recién subido y en validación: debía constar como pendiente")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if c.WaitUploadSettled(ctx, "1597720466") {
		t.Fatal("el que espera se fue: no debía darse por validado")
	}
	if !c.WaitUploadSettled(context.Background(), "1597720466") {
		t.Fatal("debía esperar hasta el final de la validación")
	}
	f.mu.Lock()
	left := f.validating["1597720466"]
	f.mu.Unlock()
	if left != 0 || c.UploadSettling("1597720466") {
		t.Fatalf("el seguimiento acabó antes de validar (quedaban %d)", left)
	}
}

// Si el mismo archivo se vuelve a subir mientras se sigue su validación, el
// seguimiento anterior se retira (no hay dos consultando el mismo ID) y quien
// esperaba la descarga pasa a esperar al nuevo.
func TestOfficialUpload_FollowUpSupersededByNewUpload(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	t.Setenv("SAPI_REFRESH_AFTER_UPLOAD", "false")
	f.validating["1597720466"] = 20

	if _, err := c.UploadFileToFolder("x.bin", bytes.NewReader([]byte("abc")), 3, 1, "", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	first := func() chan struct{} { c.hookMu.Lock(); defer c.hookMu.Unlock(); return c.settling["1597720466"] }()
	c.followNewMedia("1597720466", 1, true, 5*time.Millisecond, "")
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("el seguimiento anterior no se retiró al empezar el nuevo")
	}
	if !c.UploadSettling("1597720466") {
		t.Fatal("el seguimiento nuevo debía seguir en marcha")
	}
	if !c.WaitUploadSettled(context.Background(), "1597720466") {
		t.Fatal("debía esperar al seguimiento nuevo")
	}
}

// Un seguimiento cuya consulta falla siempre no sigue para siempre.
func TestOfficialUpload_FollowUpStopsAfterPersistentFailures(t *testing.T) {
	_, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	t.Setenv("SAPI_REFRESH_AFTER_UPLOAD", "false")
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	c.SetBaseURLs(failing.URL+"/sapi", failing.URL)
	c.followNewMedia("42", 1, true, time.Millisecond, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !c.WaitUploadSettled(ctx, "42") {
		t.Fatal("con la consulta fallando siempre, el seguimiento debía terminar")
	}
}

// Con Movistar también se sube como la app (el mismo servicio que O2). Con
// SAPI_OFFICIAL_CLIENT=false, como antes (multipart, una petición).
func TestOfficialUpload_MovistarLikeO2(t *testing.T) {
	for _, off := range []bool{false, true} {
		f, _ := newFakeOfficialSAPI(t)
		var legacy atomic.Int32
		wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/sapi/upload" { // la subida multipart de siempre
				legacy.Add(1)
				_, _ = w.Write([]byte(`{"success":"ok","id":"1597720466","status":"U"}`))
				return
			}
			f.serve(w, r)
		}))
		t.Cleanup(wrapped.Close)
		c := newOfficialTestClient(t, wrapped)
		c.mu.Lock()
		c.cfg.Provider = "movistar"
		c.mu.Unlock()
		if off {
			t.Setenv("SAPI_OFFICIAL_CLIENT", "false")
		} else {
			t.Setenv("SAPI_OFFICIAL_CLIENT", "")
		}
		if _, err := c.UploadFileToFolder("m.bin", bytes.NewReader([]byte("abc")), 3, 1, "", "", time.Time{}); err != nil {
			t.Fatalf("desactivado=%v: %v", off, err)
		}
		f.mu.Lock()
		official := len(f.meta) > 0
		f.mu.Unlock()
		if official == off || (legacy.Load() > 0) != off {
			t.Errorf("Movistar, SAPI_OFFICIAL_CLIENT desactivado=%v: como la app=%v, multipart=%d", off, official, legacy.Load())
		}
	}
}

// O2 rechaza una imagen cuyo contenido no lo es (MED-1050). La app lo da por
// definitivo (MediaUnsupported, sin reintentos): ni se reanuda sobre el ID ni se
// vuelve a intentar, y el elemento creado se retira.
func TestOfficialUpload_UnsupportedIsFinalLikeTheApp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader func([]byte) io.Reader
	}{
		{"flujo", func(b []byte) io.Reader { return io.MultiReader(bytes.NewReader(b)) }},
		{"disco", func(b []byte) io.Reader { return bytes.NewReader(b) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeOfficialSAPI(t)
			c := newOfficialTestClient(t, srv)
			f.reject["picture"] = "MED-1050"
			payload := randomPayload(t, 300000)
			_, err := c.UploadFileToFolder("falsa.jpg", tc.reader(payload), int64(len(payload)), 1, "", "", time.Time{})
			if !errors.Is(err, ErrMediaUnsupported) {
				t.Fatalf("se esperaba ErrMediaUnsupported, llegó %v", err)
			}
			var ie *UploadInterruptedError
			if errors.As(err, &ie) {
				t.Fatalf("un rechazo de O2 no es una subida interrumpida: %v", err)
			}
			if f.posts != 1 || len(f.queries) != 0 {
				t.Fatalf("no debe reintentar ni reanudar: %d envíos, %d consultas de offset", f.posts, len(f.queries))
			}
			if len(f.deleted) != 1 {
				t.Fatalf("debe retirar el elemento creado: %v", f.deleted)
			}
		})
	}
}

// Sobrescribir con 0 bytes se rechaza sin llamar a O2: O2 respondería MED-1049 y el
// original desaparecería. Por las dos vías (app y web).
func TestUpload_EmptyOverwriteRefusedBeforeO2(t *testing.T) {
	for _, web := range []bool{false, true} {
		f, srv := newFakeOfficialSAPI(t)
		c := newOfficialTestClient(t, srv)
		if web {
			t.Setenv("SAPI_OFFICIAL_CLIENT", "false")
		} else {
			t.Setenv("SAPI_OFFICIAL_CLIENT", "")
		}
		_, err := c.UploadFileReplacing(MediaItem{ID: "500", Name: "a.txt"}, "a.txt", bytes.NewReader(nil), 0, 1, "", time.Time{})
		if !errors.Is(err, ErrEmptyOverwrite) {
			t.Fatalf("web=%v: se esperaba ErrEmptyOverwrite, llegó %v", web, err)
		}
		_, err = c.UploadFileToFolder("a.txt", bytes.NewReader(nil), 0, 1, "", "500", time.Time{})
		if !errors.Is(err, ErrEmptyOverwrite) {
			t.Fatalf("web=%v (con ID): se esperaba ErrEmptyOverwrite, llegó %v", web, err)
		}
		if f.posts != 0 {
			t.Fatalf("web=%v: no debe llegar nada a O2 (%d envíos)", web, f.posts)
		}
	}
}

// Un HEIC que O2 no puede procesar: MED-1000 al contenido, y siempre igual. Se
// reintenta una vez (MED-1000 es también un fallo general de O2) y, si se repite, es
// un archivo no admitido (415), sin reanudar y retirando lo creado.
func TestOfficialUpload_DamagedHEICIsUnsupportedAfterOneRetry(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	f.reject["picture"] = "MED-1000"
	payload := randomPayload(t, 300000)
	_, err := c.UploadFileToFolder("rota.heic", bytes.NewReader(payload), int64(len(payload)), 1, "", "", time.Time{})
	if !errors.Is(err, ErrMediaUnsupported) {
		t.Fatalf("se esperaba ErrMediaUnsupported, llegó %v", err)
	}
	if f.posts != 2 || len(f.queries) != 0 {
		t.Fatalf("debe intentarlo dos veces, sin reanudar: %d envíos, %d consultas de offset", f.posts, len(f.queries))
	}
	if len(f.deleted) != 2 {
		t.Fatalf("debe retirar los dos elementos creados: %v", f.deleted)
	}
}

// Si el MED-1000 no se repite (fallo pasajero de O2), el segundo intento sube el HEIC.
func TestOfficialUpload_HEICTransientMED1000Retried(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	f.reject["picture"], f.rejectN["picture"] = "MED-1000", 1
	payload := randomPayload(t, 300000)
	item, err := c.UploadFileToFolder("foto.heic", bytes.NewReader(payload), int64(len(payload)), 1, "", "", time.Time{})
	if err != nil {
		t.Fatalf("el segundo intento debía subirlo: %v", err)
	}
	if !bytes.Equal(f.data[item.ID], payload) {
		t.Fatal("contenido distinto")
	}
}

// El MED-1000 de otros archivos sigue como antes: un rechazo de O2, sin el reintento
// propio del HEIC.
func TestOfficialUpload_MED1000NotHEICUnchanged(t *testing.T) {
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	f.reject["picture"] = "MED-1000"
	payload := randomPayload(t, 3000)
	_, err := c.UploadFileToFolder("foto.jpg", bytes.NewReader(payload), int64(len(payload)), 1, "", "", time.Time{})
	if err == nil || errors.Is(err, ErrMediaUnsupported) || f.posts != 1 {
		t.Fatalf("err=%v, envíos=%d", err, f.posts)
	}
}

// Con SAPI_UNSUPPORTED_AS_FILE=true, la imagen rechazada se sube como genérico
// (upload/file) para no perderla.
func TestOfficialUpload_UnsupportedAsFileOption(t *testing.T) {
	t.Setenv("SAPI_UNSUPPORTED_AS_FILE", "true")
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	f.reject["picture"] = "MED-1050"
	payload := randomPayload(t, 300000)
	item, err := c.UploadFileToFolder("falsa.jpg", bytes.NewReader(payload), int64(len(payload)), 1, "", "", time.Time{})
	if err != nil {
		t.Fatalf("debía subirse como genérico: %v", err)
	}
	if strings.Join(f.postKinds, ",") != "picture,file" {
		t.Fatalf("tipos usados = %v", f.postKinds)
	}
	if len(f.deleted) != 1 || f.deleted[0] == item.ID {
		t.Fatalf("debe retirar solo el elemento rechazado: %v (nuevo %s)", f.deleted, item.ID)
	}
	if ct := f.metaBodies[1]["contenttype"]; ct != "application/octet-stream" {
		t.Fatalf("contenttype del genérico = %v", ct)
	}
	if !bytes.Equal(f.data[item.ID], payload) {
		t.Fatal("contenido distinto")
	}
}

// La opción no se aplica a una sobrescritura: el archivo que ya existe conserva su
// tipo y nunca se borra.
func TestOfficialUpload_UnsupportedAsFileNeverOnOverwrite(t *testing.T) {
	t.Setenv("SAPI_UNSUPPORTED_AS_FILE", "true")
	f, srv := newFakeOfficialSAPI(t)
	c := newOfficialTestClient(t, srv)
	f.reject["picture"] = "MED-1050"
	f.meta["1597568679"] = map[string]interface{}{}
	_, err := c.UploadFileToFolder("foto.jpg", bytes.NewReader(randomPayload(t, 1000)), 1000, 1, "", "1597568679", time.Time{})
	if !errors.Is(err, ErrMediaUnsupported) {
		t.Fatalf("se esperaba ErrMediaUnsupported, llegó %v", err)
	}
	if f.posts != 1 || len(f.deleted) != 0 {
		t.Fatalf("sin reintentos ni borrados: %d envíos, retirados %v", f.posts, f.deleted)
	}
}
