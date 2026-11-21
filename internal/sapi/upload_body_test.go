package sapi

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"testing"
)

// Con el tamaño conocido, la subida lleva Content-Length exacto (como la web) y el
// formulario es el mismo: «data» con los metadatos y el archivo.
func TestUploadBody_ContentLength(t *testing.T) {
	for _, size := range []int64{0, 1, 100000} {
		data := bytes.Repeat([]byte{7}, int(size))
		body, length, ctype, done := uploadBody([]byte(`{"data":{"name":"a"}}`), "a.bin", bytes.NewReader(data), size)
		raw, _ := io.ReadAll(body)
		done()
		if int64(len(raw)) != length {
			t.Fatalf("tamaño %d: anunciado %d, enviado %d", size, length, len(raw))
		}
		_, params, _ := mime.ParseMediaType(ctype)
		mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		p, err := mr.NextPart()
		if err != nil || p.FormName() != "data" {
			t.Fatalf("primera parte: %v %v", p, err)
		}
		p, err = mr.NextPart()
		if err != nil || p.FormName() != "file" || p.FileName() != "a.bin" {
			t.Fatalf("segunda parte: %v", err)
		}
		got, _ := io.ReadAll(p)
		if !bytes.Equal(got, data) {
			t.Fatalf("archivo distinto (%d bytes)", len(got))
		}
		if _, err := mr.NextPart(); err != io.EOF {
			t.Fatalf("cierre del formulario: %v", err)
		}
	}
}
