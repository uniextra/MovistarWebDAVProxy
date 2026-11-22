package webdav

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Un bloque por carpeta enviada (rclone empieza sus cuentas en cada una) y el total.
func TestCopySummary(t *testing.T) {
	t0 := time.Now().Add(-3 * time.Hour)
	run := &copyRun{started: t0, dests: []string{"Fotos", "Vídeos"}, perDest: map[string]destTotals{
		"Fotos":  {bytes: 2 << 30, errors: 0, first: t0, last: t0.Add(2 * time.Hour)},
		"Vídeos": {bytes: 1 << 30, errors: 1, first: t0.Add(2 * time.Hour), last: t0.Add(3 * time.Hour)},
	}}
	run.last = ClientCopy{Dest: "Vídeos", Bytes: 50, TotalBytes: 200}
	title, desc := copySummary(run, true, t0.Add(3*time.Hour))
	for _, want := range []string{"Fotos\n2.0 GB en 2 h 0 min", "0 errores", "Vídeos\n1.0 GB en 1 h 0 min", "1 errores", "Total: 3.0 GB en 3 h 0 min"} {
		if title != "Copia terminada" || !strings.Contains(desc, want) {
			t.Fatalf("falta %q en:\n%s", want, desc)
		}
	}
	solo := &copyRun{started: t0, dests: []string{"Fotos"}, perDest: map[string]destTotals{"Fotos": {bytes: 1 << 30, first: t0}}}
	if _, d := copySummary(solo, true, t0.Add(time.Hour)); strings.Contains(d, "Total") {
		t.Fatalf("con una sola carpeta no hace falta total:\n%s", d)
	}
	// Los fallos de subida van dentro del bloque de su carpeta.
	clientCopies.Lock()
	if clientCopies.m == nil {
		clientCopies.m = map[string]*copyRun{}
	}
	clientCopies.m["resumen"] = run
	clientCopies.Unlock()
	RecordUploadFailure("/datos/Vídeos/Serie/T1/cap03.mkv", "409 Conflict")
	clientCopies.Lock()
	delete(clientCopies.m, "resumen")
	clientCopies.Unlock()
	if _, d := copySummary(run, true, t0.Add(3*time.Hour)); !strings.Contains(d, "1 errores\n⚠️ Serie/T1/cap03.mkv: 409 Conflict") {
		t.Fatalf("el fallo debe ir en el bloque de Vídeos:\n%s", d)
	}
	title, desc = copySummary(run, false, t0.Add(3*time.Hour))
	if title != "Copia interrumpida" || !strings.Contains(desc, "25% de «Vídeos»") {
		t.Fatalf("%s | %s", title, desc)
	}
}

// Una copia que deja de informar se da por cortada y desaparece; la que termina, también.
func TestClientCopyLifecycle(t *testing.T) {
	ReportClientCopy(ClientCopy{ID: "ciclo-1", Dest: "X", Bytes: 10, TotalBytes: 100})
	if len(ClientCopies(time.Minute)) == 0 {
		t.Fatal("debería estar en curso")
	}
	SweepClientCopies(time.Now().Add(clientCopyStaleAfter + time.Minute))
	for _, c := range ClientCopies(time.Hour) {
		if c.ID == "ciclo-1" {
			t.Fatal("una copia sin informar debe darse por cortada")
		}
	}
	ReportClientCopy(ClientCopy{ID: "ciclo-2", Dest: "Y"})
	ReportClientCopy(ClientCopy{ID: "ciclo-2", Done: true})
	for _, c := range ClientCopies(time.Hour) {
		if c.ID == "ciclo-2" {
			t.Fatal("al terminar debe desaparecer")
		}
	}
}

// El informe se acepta por POST con JSON válido y se rechaza lo demás.
func TestHandleClientCopyReport(t *testing.T) {
	t.Cleanup(func() { ReportClientCopy(ClientCopy{ID: "r-1", Done: true}) })
	rec := httptest.NewRecorder()
	handleClientCopyReport(rec, httptest.NewRequest(http.MethodPost, ClientCopyPath, strings.NewReader(`{"id":"r-1","dest":"Fotos/2024","bytes":10,"total_bytes":40}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST válido: %d", rec.Code)
	}
	if cs := ClientCopies(time.Minute); len(cs) != 1 || cs[0].Dest != "Fotos/2024" || cs[0].TotalBytes != 40 {
		t.Fatalf("estado: %+v", cs)
	}
	for _, tc := range []struct{ method, body string }{{http.MethodGet, ""}, {http.MethodPost, `{"dest":"x"}`}, {http.MethodPost, `no es json`}} {
		rec := httptest.NewRecorder()
		handleClientCopyReport(rec, httptest.NewRequest(tc.method, ClientCopyPath, strings.NewReader(tc.body)))
		if rec.Code < 400 {
			t.Fatalf("%s %q debía rechazarse: %d", tc.method, tc.body, rec.Code)
		}
	}
}
