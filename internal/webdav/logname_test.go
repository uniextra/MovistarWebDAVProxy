package webdav

import "testing"

// Los registros enseñan la ruta descifrada si se sabe, y si no, tal cual.
func TestLogName(t *testing.T) {
	old, _ := displayName.Load().(func(string) string)
	t.Cleanup(func() { SetDisplayName(old) })
	SetDisplayName(func(p string) string {
		if p == "/datos/abc/def-chunk001" {
			return "datos/Serie/cap.mkv (chunk001)"
		}
		return ""
	})
	if got := logName("/datos/abc/def-chunk001"); got != "datos/Serie/cap.mkv (chunk001)" {
		t.Fatalf("descifrada: %q", got)
	}
	if got := logName("datos/abc/def-chunk001"); got != "datos/Serie/cap.mkv (chunk001)" {
		t.Fatalf("sin barra inicial: %q", got)
	}
	if got := logName("/Fotos/a.jpg"); got != "/Fotos/a.jpg" {
		t.Fatalf("sin cifrar: %q", got)
	}
}
