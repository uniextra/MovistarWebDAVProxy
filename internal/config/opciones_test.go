package config

import (
	"testing"
	"time"
)

// Hablar como la app es lo normal con Movistar y con O2 (el mismo servicio): sin la
// variable o con «auto» (el valor de antes), sí; con false, nunca. Fuera del perfil
// desktop, nunca.
func TestOfficialClient(t *testing.T) {
	cases := []struct {
		profile, mode string
		want          bool
	}{
		{"desktop", "", true},
		{"desktop", "auto", true},
		{"desktop", "true", true},
		{"desktop", "false", false},
		{"desktop", "off", false},
		{"browser", "true", false},
		{"browser", "", false},
	}
	for _, c := range cases {
		t.Setenv("SAPI_CLIENT_PROFILE", c.profile)
		t.Setenv("SAPI_OFFICIAL_CLIENT", c.mode)
		if got := OfficialClient(); got != c.want {
			t.Errorf("perfil %s, SAPI_OFFICIAL_CLIENT=%q → %v, esperado %v", c.profile, c.mode, got, c.want)
		}
	}
}

func TestEnvHelpers(t *testing.T) {
	t.Setenv("X_SEC", "0.5")
	if got := envSeconds("X_SEC", time.Minute); got != 500*time.Millisecond {
		t.Errorf("envSeconds(0.5) = %v", got)
	}
	for _, bad := range []string{"", "abc", "-3", "0", "NaN", "Inf"} {
		t.Setenv("X_SEC", bad)
		if got := envSeconds("X_SEC", time.Minute); got != time.Minute {
			t.Errorf("envSeconds(%q) = %v, esperado el valor por defecto", bad, got)
		}
	}
	t.Setenv("X_INT", "12")
	if envInt("X_INT", 3) != 12 {
		t.Error("envInt(12)")
	}
	t.Setenv("X_INT", "-1")
	if envInt("X_INT", 3) != 3 {
		t.Error("envInt(-1) debía dar el valor por defecto")
	}
	t.Setenv("X_BOOL", "off")
	if envBool("X_BOOL", true) {
		t.Error("envBool(off)")
	}
	t.Setenv("X_BOOL", "quizá")
	if !envBool("X_BOOL", true) {
		t.Error("envBool(no reconocido) debía dar el valor por defecto")
	}
}

// Flujo directo y nombres exactos siguen a SAPI_OFFICIAL_CLIENT: por defecto sí; con
// SAPI_OFFICIAL_CLIENT=false, como antes (sin flujo, sin distinguir mayúsculas). Con
// la variable propia puesta, manda esa variable.
func TestStreamAndCaseDefaultsFollowOfficialClient(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	t.Setenv("SAPI_OFFICIAL_CLIENT", "")
	t.Setenv("WEBDAV_STREAM_UPLOAD", "")
	t.Setenv("WEBDAV_CASE_INSENSITIVE", "")
	if !StreamUploads() || CaseInsensitiveNames() {
		t.Error("por defecto: flujo directo y nombres exactos")
	}
	t.Setenv("SAPI_OFFICIAL_CLIENT", "false")
	if StreamUploads() || !CaseInsensitiveNames() {
		t.Error("SAPI_OFFICIAL_CLIENT=false: como antes, sin flujo y sin distinguir mayúsculas")
	}
	t.Setenv("WEBDAV_STREAM_UPLOAD", "true")
	t.Setenv("WEBDAV_CASE_INSENSITIVE", "false")
	if !StreamUploads() || CaseInsensitiveNames() {
		t.Error("con la variable puesta, manda la variable")
	}
	t.Setenv("SAPI_OFFICIAL_CLIENT", "")
	t.Setenv("WEBDAV_STREAM_UPLOAD", "off")
	t.Setenv("WEBDAV_CASE_INSENSITIVE", "on")
	if StreamUploads() || !CaseInsensitiveNames() {
		t.Error("con la variable puesta, manda la variable (como la app)")
	}
}

// Por defecto, como la app: una imagen que O2 rechaza por su contenido no se vuelve a
// subir como genérico.
func TestUnsupportedAsFileIsOptIn(t *testing.T) {
	t.Setenv("SAPI_UNSUPPORTED_AS_FILE", "")
	if UnsupportedAsFile() {
		t.Error("por defecto debe estar desactivado")
	}
	t.Setenv("SAPI_UNSUPPORTED_AS_FILE", "true")
	if !UnsupportedAsFile() {
		t.Error("con true debe activarse")
	}
}
