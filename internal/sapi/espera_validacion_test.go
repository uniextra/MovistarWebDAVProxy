package sapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func validationServer(t *testing.T, statusFor func(n int32) string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") != "get-validation-status" {
			http.NotFound(w, r)
			return
		}
		st := statusFor(n.Add(1))
		w.Header().Set("Content-Type", "application/json")
		if st == "" { // O2 ya no conoce el ID
			_, _ = w.Write([]byte(`{"data":{"ids":[]}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ids": []map[string]any{{"id": 77, "status": st}}}})
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestWaitMediaValidated_WaitsUntilValidated(t *testing.T) {
	srv, n := validationServer(t, func(n int32) string {
		if n < 3 {
			return "V"
		}
		return "U"
	})
	c := newOfficialTestClient(t, srv)
	if !c.WaitMediaValidated(context.Background(), "77", 5, time.Second) {
		t.Fatal("debería haber quedado validado")
	}
	if n.Load() != 3 {
		t.Fatalf("consultas = %d, quería 3", n.Load())
	}
}

func TestWaitMediaValidated_StopsOnTimeoutCancelOrUnknown(t *testing.T) {
	srv, _ := validationServer(t, func(int32) string { return "V" })
	c := newOfficialTestClient(t, srv)
	if c.WaitMediaValidated(context.Background(), "77", 5, 30*time.Millisecond) {
		t.Fatal("siempre en validación: no debería darlo por validado")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // el cliente se fue
	t0 := time.Now()
	if c.WaitMediaValidated(ctx, "77", 5, time.Minute) || time.Since(t0) > 5*time.Second {
		t.Fatal("con el contexto cancelado debe parar enseguida")
	}
	srv2, _ := validationServer(t, func(int32) string { return "" })
	c2 := newOfficialTestClient(t, srv2)
	if c2.WaitMediaValidated(context.Background(), "77", 5, time.Minute) {
		t.Fatal("un ID que O2 ya no conoce no está validado")
	}
}
