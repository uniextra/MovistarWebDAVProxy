package webdav

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestTransfers_UploadProgressAndRemoval(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	tt := NewTransferTracker(5 * time.Minute)
	tt.now = clk.now
	h := tt.Start("upload", "/a.bin", 100, 0)
	for i := 0; i < 4; i++ {
		clk.t = clk.t.Add(time.Second)
		h.Add(10)
	}
	got := tt.Snapshot()
	if len(got) != 1 || got[0].Percentage != 40 || got[0].Speed < 9 || got[0].Speed > 11 || got[0].ETA == nil || *got[0].ETA != 6 {
		t.Fatalf("snapshot = %+v", got)
	}
	h.Done()
	if got := tt.Snapshot(); len(got) != 0 {
		t.Errorf("la subida terminada debería desaparecer: %+v", got)
	}
}

// Solo se ve lo que se está pidiendo: una descarga terminada o parada (el cliente
// tira de su búfer) no aparece.
func TestTransfers_OnlyActiveDownloads(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	tt := NewTransferTracker(5 * time.Minute)
	tt.now = clk.now
	a := tt.Start("download", "/peli.mkv", 1000, 0)
	b := tt.Start("download", "/peli.mkv", 1000, 500) // otro rango del mismo archivo
	b.Add(100)
	if got := tt.Snapshot(); len(got) != 1 || got[0].Bytes != 600 || got[0].Percentage != 60 {
		t.Fatalf("snapshot = %+v", got)
	}
	clk.t = clk.t.Add(31 * time.Second)
	if got := tt.Snapshot(); len(got) != 0 {
		t.Fatalf("una descarga abierta sin mover datos no debe verse: %+v", got)
	}
	b.Add(10)
	if got := tt.Snapshot(); len(got) != 1 {
		t.Fatalf("al volver a leer debe verse: %+v", got)
	}
	a.Done()
	b.Done()
	if got := tt.Snapshot(); len(got) != 0 {
		t.Errorf("terminada no debe verse: %+v", got)
	}
}

func TestTransferWriter_RangeOffsetAndSize(t *testing.T) {
	old := Transfers
	Transfers = NewTransferTracker(time.Minute)
	t.Cleanup(func() { Transfers = old })
	rec := httptest.NewRecorder()
	tw := &transferWriter{ResponseWriter: rec, path: "/v.mkv"}
	tw.Header().Set("Content-Range", "bytes 200-299/1000")
	tw.WriteHeader(http.StatusPartialContent)
	_, _ = tw.Write(make([]byte, 100))
	got := Transfers.Snapshot()
	if len(got) != 1 || got[0].Size != 1000 || got[0].Bytes != 300 || got[0].Percentage != 30 {
		t.Fatalf("snapshot = %+v", got)
	}
	tw.done()
}

func TestTrackUpload_KeepsSeekAndResets(t *testing.T) {
	old := Transfers
	Transfers = NewTransferTracker(time.Minute)
	t.Cleanup(func() { Transfers = old })
	body, h := trackUpload("/x.bin", 10, bytes.NewReader(make([]byte, 10)))
	rs, ok := body.(io.ReadSeeker)
	if !ok {
		t.Fatal("el cuerpo debe seguir siendo io.ReadSeeker para poder reintentar")
	}
	_, _ = io.ReadAll(rs)
	_, _ = rs.Seek(0, io.SeekStart)
	if got := Transfers.Snapshot(); len(got) != 1 || got[0].Bytes != 0 {
		t.Fatalf("tras rebobinar la posición debe volver a 0: %+v", got)
	}
	h.Done()
}

// Una subida por la pasarela se ve desde que llega (nombre en claro y tamaño real) y
// su trozo cifrado no aparece como otra subida.
func TestPublicUpload_ShownOnceFromArrival(t *testing.T) {
	old := Transfers
	Transfers = NewTransferTracker(time.Minute)
	t.Cleanup(func() { Transfers = old })
	body, done := TrackPublicUpload("/datos/Vídeos/Serie/E01.mkv", 1000, io.NopCloser(bytes.NewReader(make([]byte, 1000))))
	uncover := CoverUploads("datos/abc/def")
	got := Transfers.Snapshot()
	if len(got) != 1 || got[0].Path != "/datos/Vídeos/Serie/E01.mkv" || got[0].Size != 1000 {
		t.Fatalf("debe verse desde que llega: %+v", got)
	}
	_, _ = io.CopyN(io.Discard, body, 400)
	_, h := trackUpload("/datos/abc/def-chunk001_x1", 1048, bytes.NewReader(nil))
	if got := Transfers.Snapshot(); len(got) != 1 || got[0].Bytes != 400 {
		t.Fatalf("el trozo cifrado no debe contarse aparte: %+v", got)
	}
	h.Done()
	uncover()
	done()
	if got := Transfers.Snapshot(); len(got) != 0 {
		t.Fatalf("terminada no debe verse: %+v", got)
	}
}

// Una lectura por la pasarela se ve como un solo archivo en claro; sus trozos
// cifrados no aparecen aparte.
func TestPublicDownload_ShownOnce(t *testing.T) {
	old := Transfers
	Transfers = NewTransferTracker(time.Minute)
	t.Cleanup(func() { Transfers = old })
	uncover := CoverUploads("datos/abc/def")
	rec := httptest.NewRecorder()
	pw, done := TrackPublicDownload(rec, "/datos/Vídeos/Serie/E01.mkv")
	pw.Header().Set("Content-Range", "bytes 0-99/1000")
	pw.WriteHeader(http.StatusPartialContent)
	_, _ = pw.Write(make([]byte, 100))
	raw := &transferWriter{ResponseWriter: httptest.NewRecorder(), path: "/datos/abc/def-chunk001_x1"}
	raw.Header().Set("Content-Length", "1048")
	raw.WriteHeader(http.StatusOK)
	_, _ = raw.Write(make([]byte, 100))
	got := Transfers.Snapshot()
	if len(got) != 1 || got[0].Path != "/datos/Vídeos/Serie/E01.mkv" || got[0].Size != 1000 {
		t.Fatalf("debe verse una sola lectura, la del archivo en claro: %+v", got)
	}
	raw.done()
	uncover()
	done()
}
