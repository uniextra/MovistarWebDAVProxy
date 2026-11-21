package webui

import (
	"testing"

	"movistar-webdav-proxy/internal/webdav"
)

// La cabecera de la actividad enseña el total de la copia que informa el cliente.
func TestApplyClientCopies(t *testing.T) {
	eta := int64(120)
	webdav.ReportClientCopy(webdav.ClientCopy{ID: "t-1", Dest: "Fotos/2024", Bytes: 1 << 30, TotalBytes: 4 << 30, Speed: 20 << 20, ETA: &eta, Errors: 1})
	t.Cleanup(func() { webdav.ReportClientCopy(webdav.ClientCopy{ID: "t-1", Done: true}) })
	var st copyStatus
	applyClientCopies(&st)
	if st.Current != "Fotos/2024" || st.Percent != "25" || st.ETA == "" || st.ErrorCount != 1 || st.Total == "" {
		t.Fatalf("cabecera: %+v", st)
	}
	if st2 := readCopyStatus(); st2.Current != "Fotos/2024" {
		t.Fatalf("readCopyStatus: %+v", st2.Current)
	}
}
