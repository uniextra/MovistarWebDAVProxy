package webdav

import (
	"bytes"
	"encoding/binary"
	"strings"
	"time"
)

const exifHeaderReadLimit = 64 * 1024

// extractEXIFDate extrae la fecha original de captura (DateTimeOriginal,
// DateTimeDigitized o DateTime) de los primeros bytes de un JPEG o TIFF sin
// dependencias externas.
func extractEXIFDate(data []byte) (time.Time, bool) {
	if len(data) < 12 {
		return time.Time{}, false
	}
	// TIFF directo (II*\0 o MM\0*)
	if (data[0] == 'I' && data[1] == 'I' && data[2] == 0x2A && data[3] == 0x00) ||
		(data[0] == 'M' && data[1] == 'M' && data[2] == 0x00 && data[3] == 0x2A) {
		return parseTIFFExifDate(data)
	}
	// JPEG (FF D8 ...)
	if data[0] != 0xFF || data[1] != 0xD8 {
		return time.Time{}, false
	}

	pos := 2
	for pos < len(data) {
		for pos < len(data) && data[pos] != 0xFF {
			pos++
		}
		for pos < len(data) && data[pos] == 0xFF {
			pos++
		}
		if pos >= len(data) {
			break
		}
		marker := data[pos]
		pos++

		// Fin de cabeceras (EOI o SOS)
		if marker == 0xD9 || marker == 0xDA {
			break
		}
		// Marcadores sin longitud
		if marker == 0x00 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}
		if pos+2 > len(data) {
			break
		}
		segLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
		if segLen < 2 || pos+segLen-2 > len(data) {
			break
		}
		payload := data[pos : pos+segLen-2]
		pos += segLen - 2

		if marker == 0xE1 && len(payload) >= 6 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
			if t, ok := parseTIFFExifDate(payload[6:]); ok {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func parseTIFFExifDate(tiff []byte) (time.Time, bool) {
	if len(tiff) < 8 {
		return time.Time{}, false
	}
	var bo binary.ByteOrder
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
		bo = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		bo = binary.BigEndian
	default:
		return time.Time{}, false
	}
	if bo.Uint16(tiff[2:4]) != 0x002A {
		return time.Time{}, false
	}
	ifd0Offset := bo.Uint32(tiff[4:8])
	ifd0Tags, exifIFDOffset := readTIFFIFD(tiff, bo, ifd0Offset)

	var exifTags map[uint16]string
	if exifIFDOffset > 0 {
		exifTags, _ = readTIFFIFD(tiff, bo, exifIFDOffset)
	}

	candidates := []struct {
		dt  string
		off string
	}{
		{exifTags[0x9003], exifTags[0x9011]}, // DateTimeOriginal + OffsetTimeOriginal
		{exifTags[0x9004], exifTags[0x9012]}, // DateTimeDigitized + OffsetTimeDigitized
		{ifd0Tags[0x9003], ifd0Tags[0x9011]},
		{ifd0Tags[0x9004], ifd0Tags[0x9012]},
		{ifd0Tags[0x0132], exifTags[0x9010]}, // DateTime + OffsetTime
		{ifd0Tags[0x0132], ifd0Tags[0x9010]},
	}
	for _, c := range candidates {
		if t, ok := parseEXIFTimestamp(c.dt, c.off); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func readTIFFIFD(tiff []byte, bo binary.ByteOrder, offset uint32) (map[uint16]string, uint32) {
	tags := make(map[uint16]string)
	var exifIFDOffset uint32
	if int(offset)+2 > len(tiff) {
		return tags, 0
	}
	count := int(bo.Uint16(tiff[offset : offset+2]))
	entryStart := int(offset) + 2
	if count < 0 || entryStart+count*12 > len(tiff) {
		return tags, 0
	}
	for i := 0; i < count; i++ {
		e := tiff[entryStart+i*12 : entryStart+(i+1)*12]
		tag := bo.Uint16(e[0:2])
		typ := bo.Uint16(e[2:4])
		num := bo.Uint32(e[4:8])

		if tag == 0x8769 && (typ == 4 || typ == 3) && num == 1 {
			if typ == 3 {
				exifIFDOffset = uint32(bo.Uint16(e[8:10]))
			} else {
				exifIFDOffset = bo.Uint32(e[8:12])
			}
			continue
		}

		// Solo nos interesan cadenas ASCII (tipo 2) de fecha/zona horaria.
		if typ == 2 && num > 0 && num <= 64 {
			switch tag {
			case 0x0132, 0x9003, 0x9004, 0x9010, 0x9011, 0x9012:
				var raw []byte
				if num <= 4 {
					raw = e[8 : 8+num]
				} else {
					valOff := bo.Uint32(e[8:12])
					if int(valOff)+int(num) <= len(tiff) {
						raw = tiff[valOff : int(valOff)+int(num)]
					}
				}
				if len(raw) > 0 {
					tags[tag] = strings.TrimSpace(strings.TrimRight(string(raw), "\x00 "))
				}
			}
		}
	}
	return tags, exifIFDOffset
}

func parseEXIFTimestamp(raw, offsetStr string) (time.Time, bool) {
	raw = strings.TrimSpace(strings.TrimRight(raw, "\x00 "))
	if raw == "" || strings.HasPrefix(raw, "0000:00:00") {
		return time.Time{}, false
	}
	loc := time.UTC
	if offsetStr != "" {
		if parsedOff, err := time.Parse("-07:00", offsetStr); err == nil {
			loc = parsedOff.Location()
		} else if parsedOff, err := time.Parse("-0700", offsetStr); err == nil {
			loc = parsedOff.Location()
		}
	}
	t, err := time.ParseInLocation("2006:01:02 15:04:05", raw, loc)
	if err != nil {
		return time.Time{}, false
	}
	u := t.UTC()
	if u.Year() < 1970 || u.Year() > 2100 {
		return time.Time{}, false
	}
	return u, true
}
