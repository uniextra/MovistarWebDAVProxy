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

func TestDNSCacheTTL(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 60 * time.Second, "0": 0, "30": 30 * time.Second, "-5": 60 * time.Second, "x": 60 * time.Second} {
		t.Setenv("SAPI_DNS_CACHE_SEC", v)
		if got := DNSCacheTTL(); got != want {
			t.Errorf("SAPI_DNS_CACHE_SEC=%q: %v, quería %v", v, got, want)
		}
	}
}

func TestGalleryOptions(t *testing.T) {
	t.Setenv("GALLERY_ENABLED", "")
	t.Setenv("GALLERY_DIR", "")
	t.Setenv("GALLERY_LAYOUT", "")
	t.Setenv("GALLERY_UPLOAD", "true")
	t.Setenv("GALLERY_DELETE", "true")

	if GalleryEnabled() {
		t.Error("GALLERY_ENABLED debe ser false por defecto")
	}
	if GalleryUpload() || GalleryDelete() {
		t.Error("GALLERY_UPLOAD y GALLERY_DELETE deben ser false si GALLERY_ENABLED=false")
	}
	if got := GalleryDir(); got != "_gallery" {
		t.Errorf("GalleryDir()=%q, esperado _gallery", got)
	}
	if got := GalleryLayout(); got != "year-month" {
		t.Errorf("GalleryLayout()=%q, esperado year-month", got)
	}

	t.Setenv("GALLERY_ENABLED", "true")
	if !GalleryEnabled() || !GalleryUpload() || !GalleryDelete() {
		t.Error("con GALLERY_ENABLED=true, GalleryUpload y GalleryDelete deben activarse")
	}

	for raw, want := range map[string]string{
		"Galeria":     "Galeria",
		"/Galeria/":   "Galeria",
		"":            "_gallery",
		".":           "_gallery",
		"..":          "_gallery",
		"sub/dir":     "_gallery",
		"sub\\dir":    "_gallery",
		"  MisFotos ": "MisFotos",
	} {
		t.Setenv("GALLERY_DIR", raw)
		if got := GalleryDir(); got != want {
			t.Errorf("GALLERY_DIR=%q -> %q, esperado %q", raw, got, want)
		}
	}

	for raw, want := range map[string]string{
		"flat":       "flat",
		" FLAT ":     "flat",
		"year-month": "year-month",
		"ym":         "year-month",
		"date":       "year-month",
		"other":      "year-month",
	} {
		t.Setenv("GALLERY_LAYOUT", raw)
		if got := GalleryLayout(); got != want {
			t.Errorf("GALLERY_LAYOUT=%q -> %q, esperado %q", raw, got, want)
		}
	}
}
