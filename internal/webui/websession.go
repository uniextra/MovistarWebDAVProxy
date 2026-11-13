package webui

import (
	"net/http"

	"movistar-webdav-proxy/internal/webdav"
)

// handleWebSession devuelve el estado de la sesión web (modo web) para el panel.
func (s *Server) handleWebSession(w http.ResponseWriter, r *http.Request) {
	if s.webStatus == nil {
		jsonResponse(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	out := s.webStatus()
	out["enabled"] = true
	jsonResponse(w, http.StatusOK, out)
}

// handleTransfers devuelve las subidas y descargas en curso del proxy.
func (s *Server) handleTransfers(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{"transfers": webdav.Transfers.Snapshot()})
}
