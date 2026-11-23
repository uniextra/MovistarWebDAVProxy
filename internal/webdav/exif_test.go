package webdav

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func buildTestJPEGWithEXIF(t *testing.T, dateOriginal, offsetOriginal string) []byte {
	t.Helper()
	var tiff bytes.Buffer
	// Header TIFF Little Endian ("II*\0", IFD0 @ 8)
	tiff.WriteString("II")
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x002A))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8))

	// IFD0: 1 entry (0x8769 ExifIFD pointer) -> offset 26
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(1))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x8769))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(4)) // LONG
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(1))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(26))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0)) // next IFD

	// ExifIFD @ 26
	dtBytes := append([]byte(dateOriginal), 0)
	var entries int = 1
	if offsetOriginal != "" {
		entries = 2
	}
	exifIFDSize := 2 + entries*12 + 4
	valOffset := uint32(26 + exifIFDSize)

	_ = binary.Write(&tiff, binary.LittleEndian, uint16(entries))
	// Entry 1: DateTimeOriginal (0x9003)
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x9003))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(2)) // ASCII
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(len(dtBytes)))
	_ = binary.Write(&tiff, binary.LittleEndian, valOffset)

	var offBytes []byte
	if offsetOriginal != "" {
		offBytes = append([]byte(offsetOriginal), 0)
		_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x9011))
		_ = binary.Write(&tiff, binary.LittleEndian, uint16(2))
		_ = binary.Write(&tiff, binary.LittleEndian, uint32(len(offBytes)))
		_ = binary.Write(&tiff, binary.LittleEndian, valOffset+uint32(len(dtBytes)))
	}
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0)) // next IFD
	tiff.Write(dtBytes)
	tiff.Write(offBytes)

	var jpg bytes.Buffer
	jpg.Write([]byte{0xFF, 0xD8}) // SOI
	// APP1 Exif
	app1Len := 2 + 6 + tiff.Len()
	jpg.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&jpg, binary.BigEndian, uint16(app1Len))
	jpg.WriteString("Exif\x00\x00")
	jpg.Write(tiff.Bytes())
	jpg.Write([]byte{0xFF, 0xDA, 0x00, 0x02, 0xFF, 0xD9}) // SOS + EOI
	return jpg.Bytes()
}

func TestExtractEXIFDate(t *testing.T) {
	jpg := buildTestJPEGWithEXIF(t, "2019:07:14 18:30:05", "")
	got, ok := extractEXIFDate(jpg)
	if !ok {
		t.Fatal("esperaba extraer fecha EXIF")
	}
	want := time.Date(2019, 7, 14, 18, 30, 5, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}

	jpgWithTZ := buildTestJPEGWithEXIF(t, "2021:08:20 20:00:00", "+02:00")
	gotTZ, ok := extractEXIFDate(jpgWithTZ)
	if !ok {
		t.Fatal("esperaba extraer fecha EXIF con zona horaria")
	}
	wantTZ := time.Date(2021, 8, 20, 18, 0, 0, 0, time.UTC)
	if !gotTZ.Equal(wantTZ) {
		t.Errorf("gotTZ %v, wantTZ %v", gotTZ, wantTZ)
	}

	if _, ok := extractEXIFDate([]byte("not-a-jpeg")); ok {
		t.Error("no debía extraer fecha de bytes no JPEG")
	}
}
