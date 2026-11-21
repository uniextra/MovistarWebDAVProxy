package webui

import (
	"fmt"
	"movistar-webdav-proxy/internal/webdav"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Actividad en curso del panel: subidas, lecturas y cola según el propio proxy (ve
// todas las vías: WebDAV en bruto, WebDAV descifrado, montaje) y, si un cliente
// informa de su copia (POST /.nube-estado), el progreso total de esa copia.

// copyTransfer es un archivo en curso (nombre legible, no cifrado).
type copyTransfer struct {
	Name       string  `json:"name"`
	Size       int64   `json:"size"`
	Bytes      int64   `json:"bytes"`
	Percentage int     `json:"percentage"`
	Speed      float64 `json:"speed"` // bytes/s
	ETA        *int64  `json:"eta"`   // segundos, null si se desconoce

	Idle        bool  `json:"idle,omitempty"`         // lectura en pausa (el reproductor tira de su búfer)
	IdleSeconds int64 `json:"idle_seconds,omitempty"` // desde la última ráfaga
}

type copyStatus struct {
	Enabled     bool           `json:"enabled"`
	Current     string         `json:"current,omitempty"` // destino de la copia que informa un cliente
	Transferred string         `json:"transferred,omitempty"`
	Total       string         `json:"total,omitempty"`
	Percent     string         `json:"percent,omitempty"`
	Speed       string         `json:"speed,omitempty"`
	ETA         string         `json:"eta,omitempty"`
	StatsAt     string         `json:"stats_at,omitempty"`
	ErrorCount  int            `json:"error_count"`
	BWLimit     string         `json:"bwlimit"`
	Uploading   []copyTransfer `json:"uploading"`
	Reading     []copyTransfer `json:"reading"`
	Queued      []copyTransfer `json:"queued"`       // subidas esperando hueco en el proxy
	UploadSpeed float64        `json:"upload_speed"` // bytes/s
	ReadSpeed   float64        `json:"read_speed"`
}

var bwlimitRe = regexp.MustCompile(`^(off|\d+(\.\d+)?[KMG]?)$`)

// humanBytes formatea en unidades binarias como rclone ("148.873 GiB").
func humanBytes(n float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	return fmt.Sprintf("%.3f %s", n, units[i])
}

func humanDuration(sec int64) string {
	d := time.Duration(sec) * time.Second
	h := int64(d.Hours())
	m := int64(d.Minutes()) % 60
	ss := int64(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm%ds", h, m, ss)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, ss)
	}
	return fmt.Sprintf("%ds", ss)
}

// readCopyStatus es el estado de la actividad en curso.
func readCopyStatus() copyStatus {
	st := copyStatus{Enabled: true, BWLimit: "off", Uploading: []copyTransfer{}, Reading: []copyTransfer{}, Queued: []copyTransfer{}}
	fillFromProxy(&st)
	applyClientCopies(&st)
	return st
}

// applyClientCopies pone en la cabecera el total de las copias que informan los
// clientes (POST /.nube-estado con el core/stats de rclone).
func applyClientCopies(st *copyStatus) {
	copies := webdav.ClientCopies(30 * time.Second)
	if st.Current != "" || len(copies) == 0 {
		return
	}
	var bytes, total int64
	var speed float64
	var eta int64
	var dests []string
	newest := copies[0].Updated
	for _, c := range copies {
		bytes += c.Bytes
		total += c.TotalBytes
		speed += c.Speed
		if c.ETA != nil && *c.ETA > eta {
			eta = *c.ETA
		}
		st.ErrorCount += c.Errors
		if c.Dest != "" {
			dests = append(dests, c.Dest)
		}
		if c.Updated.After(newest) {
			newest = c.Updated
		}
	}
	st.Current = strings.Join(dests, ", ")
	if st.Current == "" {
		st.Current = "copia desde un cliente"
	}
	if len(copies) > 1 {
		st.Current += fmt.Sprintf(" (%d envíos)", len(copies))
	}
	st.Transferred = humanBytes(float64(bytes))
	st.Total = humanBytes(float64(total))
	if total > 0 {
		st.Percent = strconv.Itoa(int(bytes * 100 / total))
	}
	if speed > 0 {
		st.Speed = humanBytes(speed) + "/s"
	}
	if eta > 0 {
		st.ETA = humanDuration(eta)
	} else if speed > 0 && total > bytes {
		st.ETA = humanDuration(int64(float64(total-bytes) / speed))
	}
	st.StatsAt = newest.Format("2006/01/02 15:04:05")
}

func (s *Server) handleCopyStatus(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, readCopyStatus())
}

// handleCopyBWLimit fija el límite de subida a la nube (todas las vías).
func (s *Server) handleCopyBWLimit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rate string `json:"rate"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	rate := strings.TrimSpace(req.Rate)
	bps, err := webdav.ParseBandwidth(rate)
	if err != nil || !bwlimitRe.MatchString(rate) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "Límite no válido (usa off, 20M, 500K...)"})
		return
	}
	webdav.SetUploadBandwidth(bps)
	saveUploadBandwidth(rate)
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok", "bwlimit": rate})
}

// fillFromProxy rellena la actividad con lo que ve el proxy: subidas, lecturas y cola.
func fillFromProxy(st *copyStatus) {
	st.BWLimit = currentUploadBandwidth()
	for _, t := range webdav.Transfers.Snapshot() {
		ct := copyTransfer{Name: panelName(t), Size: t.Size, Bytes: t.Bytes, Percentage: t.Percentage,
			Speed: t.Speed, ETA: t.ETA, Idle: t.Idle, IdleSeconds: t.IdleSeconds}
		switch t.Kind {
		case "upload":
			st.Uploading = append(st.Uploading, ct)
			st.UploadSpeed += t.Speed
		case "download":
			st.Reading = append(st.Reading, ct)
			if !t.Idle {
				st.ReadSpeed += t.Speed
			}
		case "queued":
			st.Queued = append(st.Queued, ct)
		}
	}
}

// panelName es el nombre legible de una transferencia (en claro si la carpeta está cifrada).
func panelName(t webdav.TransferInfo) string {
	name := t.Display
	if name == "" {
		name = t.Path
	}
	return strings.TrimPrefix(name, "/")
}
