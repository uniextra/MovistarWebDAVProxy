package auth

import (
	"net/http"
	"testing"
)

// La app oficial negocia con upload.cloud.o2online.es con la misma huella que con
// cloud (JA3 57fbe0ae…); el perfil desktop debe hacer lo mismo y el browser no cambia.
func TestUploadHostUsesO2FingerprintInDesktop(t *testing.T) {
	for _, tc := range []struct {
		profile string
		want    bool
	}{{"desktop", true}, {"browser", false}} {
		t.Setenv("SAPI_CLIENT_PROFILE", tc.profile)
		tr, ok := NewFingerprintedTransport().(interface{ HTTP1Transport() *http.Transport })
		if !ok {
			t.Fatal("el transporte debe exponer HTTP1Transport()")
		}
		if got := tr.HTTP1Transport().DialTLSContext != nil; got != tc.want {
			t.Errorf("perfil %s: huella propia hacia upload.* = %v, se esperaba %v", tc.profile, got, tc.want)
		}
	}
}
