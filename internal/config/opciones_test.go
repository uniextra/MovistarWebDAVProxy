package config

import (
	"testing"
	"time"
)

// Hablar como la app solo está probado con O2: por defecto, solo con O2; con
// SAPI_OFFICIAL_CLIENT=true también con Movistar (para probarlo) y con false nunca.
// Fuera del perfil desktop, nunca.
func TestOfficialClientFor(t *testing.T) {
	cases := []struct {
		profile, mode, provider string
		want                    bool
	}{
		{"desktop", "", "o2", true},
		{"desktop", "", "O2", true},
		{"desktop", "", "movistar", false},
		{"desktop", "", "", false},
		{"desktop", "true", "movistar", true},
		{"desktop", "false", "o2", false},
		{"browser", "true", "o2", false},
	}
	for _, c := range cases {
		t.Setenv("SAPI_CLIENT_PROFILE", c.profile)
		t.Setenv("SAPI_OFFICIAL_CLIENT", c.mode)
		if got := OfficialClientFor(c.provider); got != c.want {
			t.Errorf("perfil %s, SAPI_OFFICIAL_CLIENT=%q, %s → %v, esperado %v", c.profile, c.mode, c.provider, got, c.want)
		}
	}
}

func TestOfficialClientForHost(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	t.Setenv("SAPI_OFFICIAL_CLIENT", "")
	for host, want := range map[string]bool{
		"upload.cloud.o2online.es":   true,
		"cloud.o2online.es.":         true,
		"o2online.es":                true,
		"upload.micloud.movistar.es": false,
		"evil-o2online.es":           false,
	} {
		if got := OfficialClientForHost(host); got != want {
			t.Errorf("%s → %v, esperado %v", host, got, want)
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

// Flujo directo y nombres exactos son lo nuevo y solo se han probado con O2: por
// defecto, solo con O2; con Movistar, como antes (sin flujo, sin distinguir
// mayúsculas). Con la variable puesta, manda la variable.
func TestStreamAndCaseDefaultsFollowProvider(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	t.Setenv("SAPI_OFFICIAL_CLIENT", "")
	t.Setenv("WEBDAV_STREAM_UPLOAD", "")
	t.Setenv("WEBDAV_CASE_INSENSITIVE", "")
	if !StreamUploadsFor("o2") || CaseInsensitiveNamesFor("o2") {
		t.Error("O2: flujo directo y nombres exactos por defecto")
	}
	if StreamUploadsFor("movistar") || !CaseInsensitiveNamesFor("movistar") {
		t.Error("Movistar: como antes, sin flujo y sin distinguir mayúsculas")
	}
	t.Setenv("WEBDAV_STREAM_UPLOAD", "true")
	t.Setenv("WEBDAV_CASE_INSENSITIVE", "false")
	if !StreamUploadsFor("movistar") || CaseInsensitiveNamesFor("movistar") {
		t.Error("con la variable puesta, manda la variable")
	}
	t.Setenv("WEBDAV_STREAM_UPLOAD", "off")
	t.Setenv("WEBDAV_CASE_INSENSITIVE", "on")
	if StreamUploadsFor("o2") || !CaseInsensitiveNamesFor("o2") {
		t.Error("con la variable puesta, manda la variable (O2)")
	}
}
