package webui

import (
	"encoding/json"

	"fmt"
	"movistar-webdav-proxy/internal/webdav"
	"net/http"
	"strings"
	"time"
)

// handleCopyEvents envía el estado de la copia en cuanto cambia (Server-Sent
// Events), para que la tarjeta se actualice en tiempo real sin consultar cada X
// segundos. Vigila cada segundo la actividad del proxy y las fechas y tamaños de los
// archivos de la copia; si nada cambia, manda un latido cada 15 s.
func (s *Server) handleCopyEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	send := func(payload string) bool {
		// El panel tiene WriteTimeout de 30 s: se alarga en cada envío. Si no se
		// puede, el flujo se corta y el navegador se reconecta solo.
		_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := fmt.Fprint(w, payload); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send("retry: 1000\n\n") {
		return
	}

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var lastSig string
	lastSent := time.Time{}
	for {
		sig := s.copySignature()
		if sig != lastSig || time.Since(lastSent) > 15*time.Second {
			if sig != lastSig {
				b, err := json.Marshal(readCopyStatus())
				if err == nil && !send("data: "+string(b)+"\n\n") {
					return
				}
			} else if !send(": latido\n\n") {
				return
			}
			lastSig, lastSent = sig, time.Now()
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

// copySignature resume la actividad en curso: si cambia, hay algo nuevo que enviar.
func (s *Server) copySignature() string {
	var b strings.Builder
	b.WriteString("#") // nunca vacía: el primer estado se envía siempre
	// Cualquier cambio de lo que hay en curso o de su avance (por MB).
	for _, t := range webdav.Transfers.Snapshot() {
		fmt.Fprintf(&b, "%s|%s|%d;", t.Kind, t.Path, t.Bytes>>20)
	}
	for _, c := range webdav.ClientCopies(30 * time.Second) {
		fmt.Fprintf(&b, "c%s|%d;", c.ID, c.Bytes>>20)
	}
	return b.String()
}
