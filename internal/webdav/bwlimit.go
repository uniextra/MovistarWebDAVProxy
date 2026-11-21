package webdav

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Límite de ancho de banda común a todas las subidas a la nube, entren por donde
// entren (montaje, SMB, WebDAV directo, copia masiva). 0 = sin límite. Se fija con
// WEBDAV_UPLOAD_BWLIMIT al arrancar o desde el panel.
type bandwidthLimiter struct {
	mu     sync.Mutex
	rate   int64 // bytes/s; 0 = sin límite
	tokens float64
	last   time.Time
}

var uploadBandwidth = &bandwidthLimiter{}

// ParseBandwidth interpreta «off», «0» o un número con sufijo K/M/G (KiB/s, MiB/s,
// GiB/s, como el --bwlimit de rclone). Devuelve bytes/s.
func ParseBandwidth(v string) (int64, error) {
	v = strings.TrimSpace(strings.ToUpper(v))
	if v == "" || v == "OFF" || v == "0" {
		return 0, nil
	}
	mult := float64(1)
	switch v[len(v)-1] {
	case 'K':
		mult, v = 1<<10, v[:len(v)-1]
	case 'M':
		mult, v = 1<<20, v[:len(v)-1]
	case 'G':
		mult, v = 1<<30, v[:len(v)-1]
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("límite no válido: %q (usa off, 20M, 500K...)", v)
	}
	return int64(n * mult), nil
}

// SetUploadBandwidth fija el límite común de subida (bytes/s; 0 = sin límite).
func SetUploadBandwidth(bytesPerSec int64) {
	uploadBandwidth.mu.Lock()
	uploadBandwidth.rate = bytesPerSec
	uploadBandwidth.tokens = 0
	uploadBandwidth.last = time.Now()
	uploadBandwidth.mu.Unlock()
}

// UploadBandwidth devuelve el límite común de subida en bytes/s (0 = sin límite).
func UploadBandwidth() int64 {
	uploadBandwidth.mu.Lock()
	defer uploadBandwidth.mu.Unlock()
	return uploadBandwidth.rate
}

// wait reparte n bytes en el tiempo según el límite (cubo de fichas compartido por
// todas las subidas, con ráfaga de un segundo).
func (b *bandwidthLimiter) wait(n int) {
	for n > 0 {
		b.mu.Lock()
		rate := b.rate
		if rate <= 0 {
			b.mu.Unlock()
			return
		}
		now := time.Now()
		b.tokens += now.Sub(b.last).Seconds() * float64(rate)
		if b.tokens > float64(rate) {
			b.tokens = float64(rate)
		}
		b.last = now
		take := float64(n)
		if take > b.tokens {
			take = b.tokens
		}
		if take > 0 {
			b.tokens -= take
			n -= int(take)
		}
		var sleep time.Duration
		if n > 0 {
			sleep = time.Duration(float64(min(n, 64<<10)) / float64(rate) * float64(time.Second))
		}
		b.mu.Unlock()
		if sleep > 0 {
			time.Sleep(sleep)
		}
	}
}
