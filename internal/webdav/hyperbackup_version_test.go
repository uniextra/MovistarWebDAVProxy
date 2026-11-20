package webdav

import "testing"

func TestHyperBackupStateSeq(t *testing.T) {
	cases := map[string]int64{
		"v1.5.-1.0.0.none.none.cinfo":  5,
		"v1.24.-1.0.0.none.none.cinfo": 24,
		"V1.3.-1.0.0.NONE.NONE.CINFO":  3,
	}
	for name, want := range cases {
		if got, ok := hbStateSeq(name); !ok || got != want {
			t.Errorf("hbStateSeq(%q) = %d, %v; esperado %d", name, got, ok, want)
		}
	}
	for _, name := range []string{"v1.4.5.1.0.backup.chunking.cinfo", "v1.cformat", "v1.0.-1.0.0.none.none.cinfo", "x.cinfo"} {
		if _, ok := hbStateSeq(name); ok {
			t.Errorf("hbStateSeq(%q) no es un .cinfo de estado", name)
		}
	}
}

// La versión confirmada es la N de Config/version_info.db.N; con varias (copia cortada
// entre subir la nueva y borrar la anterior), la menor.
func TestHyperBackupVersionFromConfigNames(t *testing.T) {
	if _, ok := hbVersionFromNames([]string{"1.db.5", "virtual_file.index"}); ok {
		t.Fatal("sin version_info.db.N no hay versión")
	}
	if n, ok := hbVersionFromNames([]string{"version_info.db.5", "4.db.5"}); !ok || n != 5 {
		t.Fatalf("una sola: %d, %v", n, ok)
	}
	if n, ok := hbVersionFromNames([]string{"version_info.db.24", "version_info.db.25"}); !ok || n != 24 {
		t.Fatalf("varias: %d, %v (esperado 24)", n, ok)
	}
}
