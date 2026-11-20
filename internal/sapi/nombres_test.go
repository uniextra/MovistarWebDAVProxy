package sapi

import "testing"

func TestCloudNames_RoundTrip(t *testing.T) {
	t.Setenv("SAPI_ESCAPE_NAMES", "")
	cases := map[string]string{
		".env":        "%2Eenv",
		".DS_Store":   "%2EDS_Store",
		"x.":          "x%2E",
		"x.tmp":       "x%2Etmp",
		"x.TMP":       "x%2ETMP",
		"x.txt.tmp":   "x.txt%2Etmp",
		".tmp":        "%2Etmp",
		"x.tmp.":      "x.tmp%2E",
		".config.tmp": "%2Econfig%2Etmp",
		// sin cambios
		"x.txt":     "x.txt",
		"x.tmp.txt": "x.tmp.txt",
		"xtmp":      "xtmp",
		"x%20.txt":  "x%20.txt",
		"a.b.c":     "a.b.c",
	}
	for local, cloud := range cases {
		if got := toCloudName(local); got != cloud {
			t.Errorf("toCloudName(%q) = %q, esperado %q", local, got, cloud)
		}
		if got := fromCloudName(cloud); got != local {
			t.Errorf("fromCloudName(%q) = %q, esperado %q", cloud, got, local)
		}
	}
}

func TestCloudNames_OptOut(t *testing.T) {
	t.Setenv("SAPI_ESCAPE_NAMES", "false")
	if toCloudName(".env") != ".env" || fromCloudName("%2Eenv") != "%2Eenv" {
		t.Fatal("con SAPI_ESCAPE_NAMES=false no se traduce nada")
	}
}

// Los nombres leídos de la nube se traducen al decodificar el elemento.
func TestCloudNames_MediaItemDecodes(t *testing.T) {
	t.Setenv("SAPI_ESCAPE_NAMES", "")
	var m MediaItem
	if err := m.UnmarshalJSON([]byte(`{"id":"1","name":"%2Eenv","size":4}`)); err != nil {
		t.Fatal(err)
	}
	if m.Name != ".env" {
		t.Fatalf("nombre: %q", m.Name)
	}
}
