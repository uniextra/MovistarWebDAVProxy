package auth

import (
	"net/http"
	"testing"
	"time"
)

// Las subidas grandes necesitan esperar la respuesta de O2 más de 60 s (15/11/2026).
func TestUploadResponseTimeout_PorDefectoYConfigurable(t *testing.T) {
	t.Setenv("UPLOAD_RESPONSE_TIMEOUT_SEC", "")
	if got := UploadResponseTimeout(); got != 5*time.Minute {
		t.Fatalf("por defecto: esperado 5m, obtenido %s", got)
	}
	tr, ok := NewFingerprintedTransport().(interface{ HTTP1Transport() *http.Transport })
	if !ok {
		t.Fatal("el transporte debe exponer HTTP1Transport()")
	}
	if got := tr.HTTP1Transport().ResponseHeaderTimeout; got != 5*time.Minute {
		t.Fatalf("el transporte de subidas debe esperar 5m la respuesta, espera %s", got)
	}
	t.Setenv("UPLOAD_RESPONSE_TIMEOUT_SEC", "900")
	if got := UploadResponseTimeout(); got != 15*time.Minute {
		t.Fatalf("con UPLOAD_RESPONSE_TIMEOUT_SEC=900: esperado 15m, obtenido %s", got)
	}
	t.Setenv("UPLOAD_RESPONSE_TIMEOUT_SEC", "no-numero")
	if got := UploadResponseTimeout(); got != 5*time.Minute {
		t.Fatalf("con un valor inválido: esperado 5m, obtenido %s", got)
	}
}
