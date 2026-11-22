package webui

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/webdav"
)

// Límite común de subida: se guarda en TOKENS_DIR/upload_bwlimit para que
// sobreviva a los reinicios; WEBDAV_UPLOAD_BWLIMIT lo fija si no hay uno guardado.
var uploadBWLimit atomic.Value // string: «off», «20M»...

func bwLimitFile() string { return filepath.Join(config.TokensDir, "upload_bwlimit") }

func saveUploadBandwidth(rate string) {
	uploadBWLimit.Store(rate)
	_ = os.WriteFile(bwLimitFile(), []byte(rate+"\n"), 0o600)
}

func currentUploadBandwidth() string {
	if v, ok := uploadBWLimit.Load().(string); ok && v != "" {
		return v
	}
	return "off"
}

// LoadUploadBandwidth aplica al arrancar el límite guardado o el de
// WEBDAV_UPLOAD_BWLIMIT.
func LoadUploadBandwidth() {
	rate := strings.TrimSpace(os.Getenv("WEBDAV_UPLOAD_BWLIMIT"))
	if b, err := os.ReadFile(bwLimitFile()); err == nil && strings.TrimSpace(string(b)) != "" {
		rate = strings.TrimSpace(string(b))
	}
	if rate == "" {
		return
	}
	bps, err := webdav.ParseBandwidth(rate)
	if err != nil {
		logger.Log("[WebDAV] ⚠️ Límite de subida no válido %q: %v", rate, err)
		return
	}
	webdav.SetUploadBandwidth(bps)
	uploadBWLimit.Store(rate)
	if bps > 0 {
		logger.Log("[WebDAV] 🚦 Límite de subida común a todas las subidas: %s/s", rate)
	}
}
