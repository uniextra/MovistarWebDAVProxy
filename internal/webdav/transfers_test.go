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

func TestTransfers_DownloadsGroupAndStayIdle(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	tt := NewTransferTracker(5 * time.Minute)
	tt.now = clk.now
	a := tt.Start("download", "/peli.mkv", 1000, 0)
	b := tt.Start("download", "/peli.mkv", 1000, 500) // otro rango del mismo archivo
	b.Add(100)
	if got := tt.Snapshot(); len(got) != 1 || got[0].Bytes != 600 || got[0].Percentage != 60 {
		t.Fatalf("snapshot = %+v", got)
	}
	a.Done()
	b.Done()
	clk.t = clk.t.Add(time.Minute)
	got := tt.Snapshot()
	if len(got) != 1 || !got[0].Idle || got[0].IdleSeconds != 60 {
		t.Fatalf("debería seguir visible e inactiva: %+v", got)
	}
	clk.t = clk.t.Add(5 * time.Minute)
	if got := tt.Snapshot(); len(got) != 0 {
		t.Errorf("pasado el margen debería desaparecer: %+v", got)
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
