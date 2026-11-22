package webdav

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/notifier"
)

// ClientCopy es el estado que un cliente (p. ej. un script de rclone) manda
// de su copia con rclone: el proxy solo ve lo que le llega, el cliente sabe el total.
type ClientCopy struct {
	ID         string    `json:"id"`
	Dest       string    `json:"dest"`        // carpeta de destino en claro, p. ej. «Fotos/2024»
	Bytes      int64     `json:"bytes"`       // copiado
	TotalBytes int64     `json:"total_bytes"` // total conocido hasta ahora
	Speed      float64   `json:"speed"`       // bytes/s
	ETA        *int64    `json:"eta"`         // segundos, si rclone lo sabe
	Errors     int       `json:"errors"`
	Done       bool      `json:"done"`
	Updated    time.Time `json:"-"`
}

// ClientCopyPath recibe (POST, con las credenciales del WebDAV) el estado que manda
// un cliente de su copia con rclone. Ver docs/web-mode.md.
const ClientCopyPath = "/.nube-estado"

// handleClientCopyReport guarda el estado de una copia de un cliente (JSON
// ClientCopy). Las credenciales ya se han comprobado.
func handleClientCopyReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	var c ClientCopy
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&c); err != nil || c.ID == "" || len(c.ID) > 100 || len(c.Dest) > 1000 {
		http.Error(w, "estado no válido", http.StatusBadRequest)
		return
	}
	ReportClientCopy(c)
	w.WriteHeader(http.StatusNoContent)
}

// clientCopyStaleAfter: una copia que deja de informar este tiempo se da por cortada.
const clientCopyStaleAfter = 10 * time.Minute

// copyRun acumula una copia entera: un envío puede tener varias carpetas y rclone
// empieza sus cuentas de cero en cada una.
type copyRun struct {
	last    ClientCopy
	started time.Time
	dests   []string
	perDest map[string]destTotals
}

type destTotals struct {
	bytes       int64
	errors      int
	first, last time.Time // primer y último informe de esa carpeta (su duración)
	failed      []string  // subidas que fallaron en el proxy: «ruta: motivo»
	failedMore  int       // fallos que no caben en failed
}

// copyFailedMax: fallos que se listan por carpeta en el aviso.
const copyFailedMax = 10

// RecordUploadFailure apunta una subida fallida en la carpeta de la copia en curso a la
// que pertenece (la ruta contiene «/<carpeta enviada>/»). rclone solo da el número de
// errores; así el aviso dice cuáles fueron.
func RecordUploadFailure(path, reason string) {
	p := "/" + strings.Trim(path, "/")
	clientCopies.Lock()
	defer clientCopies.Unlock()
	for _, run := range clientCopies.m {
		for _, d := range run.dests {
			i := strings.Index(p, "/"+strings.Trim(d, "/")+"/")
			if d == "" || i < 0 {
				continue
			}
			t := run.perDest[d]
			rel := p[i+len(d)+2:]
			if len(t.failed) < copyFailedMax {
				t.failed = append(t.failed, rel+": "+reason)
			} else {
				t.failedMore++
			}
			run.perDest[d] = t
			return
		}
	}
}

var clientCopies struct {
	sync.Mutex
	m map[string]*copyRun
}

// ReportClientCopy guarda el último estado de una copia. Con Done la da por terminada
// y avisa por Telegram con el resumen.
func ReportClientCopy(c ClientCopy) {
	clientCopies.Lock()
	if clientCopies.m == nil {
		clientCopies.m = map[string]*copyRun{}
	}
	run := clientCopies.m[c.ID]
	if c.Done {
		delete(clientCopies.m, c.ID)
		clientCopies.Unlock()
		if run != nil {
			go notifyCopy(run, true, time.Now())
		}
		return
	}
	now := time.Now()
	if run == nil {
		run = &copyRun{started: now, perDest: map[string]destTotals{}}
		clientCopies.m[c.ID] = run
	}
	c.Updated = now
	run.last = c
	if _, seen := run.perDest[c.Dest]; !seen {
		run.dests = append(run.dests, c.Dest)
	}
	t := run.perDest[c.Dest]
	if t.first.IsZero() {
		t.first = now
	}
	t.bytes, t.errors, t.last = c.Bytes, c.Errors, now
	run.perDest[c.Dest] = t
	clientCopies.Unlock()
}

// SweepClientCopies da por cortadas las copias que llevan clientCopyStaleAfter sin
// informar (ventana cerrada, PC apagado...) y avisa. Se llama periódicamente.
func SweepClientCopies(now time.Time) {
	clientCopies.Lock()
	var stale []*copyRun
	for id, run := range clientCopies.m {
		if now.Sub(run.last.Updated) > clientCopyStaleAfter {
			stale = append(stale, run)
			delete(clientCopies.m, id)
		}
	}
	clientCopies.Unlock()
	for _, run := range stale {
		notifyCopy(run, false, now)
	}
}

// ClientCopies devuelve las copias con estado reciente (de hace menos de maxAge).
func ClientCopies(maxAge time.Duration) []ClientCopy {
	clientCopies.Lock()
	defer clientCopies.Unlock()
	var out []ClientCopy
	for _, run := range clientCopies.m {
		if time.Since(run.last.Updated) <= maxAge {
			out = append(out, run.last)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// copySummary es el texto del aviso de fin (o de corte) de una copia: un bloque por
// cada carpeta enviada y, si hay varias, el total.
func copySummary(run *copyRun, finished bool, now time.Time) (title, desc string) {
	var b strings.Builder
	var bytes int64
	var errs int
	for i, d := range run.dests {
		t := run.perDest[d]
		bytes += t.bytes
		errs += t.errors
		name := d
		if name == "" {
			name = "(raíz)"
		}
		end := t.last
		if i == len(run.dests)-1 {
			end = now // la última carpeta acaba con el aviso
		}
		dur := end.Sub(t.first)
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%s\n%s en %s", name, copySize(float64(t.bytes)), copyDuration(dur))
		if secs := dur.Seconds(); secs >= 1 && t.bytes > 0 {
			fmt.Fprintf(&b, " · media %s/s", copySize(float64(t.bytes)/secs))
		}
		fmt.Fprintf(&b, " · %d errores", t.errors)
		for _, f := range t.failed {
			fmt.Fprintf(&b, "\n⚠️ %s", f)
		}
		if t.failedMore > 0 {
			fmt.Fprintf(&b, "\n… y %d fallos más", t.failedMore)
		}
	}
	if len(run.dests) > 1 {
		fmt.Fprintf(&b, "\n\nTotal: %s en %s", copySize(float64(bytes)), copyDuration(now.Sub(run.started)))
	}
	if finished {
		if errs > 0 {
			b.WriteString("\n\nVuelve a enviar lo mismo: se salta lo ya subido y reintenta lo que falló.")
		}
		return "Copia terminada", b.String()
	}
	if last := run.last; last.TotalBytes > 0 {
		fmt.Fprintf(&b, "\n\nSe quedó en %d%% de «%s».", int(last.Bytes*100/last.TotalBytes), last.Dest)
	}
	b.WriteString("\nDejó de informar hace más de 10 minutos (¿ventana cerrada o PC apagado?). Al volver a enviarla se salta lo ya subido.")
	return "Copia interrumpida", b.String()
}

func notifyCopy(run *copyRun, finished bool, now time.Time) {
	title, desc := copySummary(run, finished, now)
	icon := "✅"
	if !finished {
		icon = "⚠️"
	}
	_ = notifier.Alert("", icon, title, desc, "")
}

func copySize(n float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}

func copyDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h >= 24:
		return fmt.Sprintf("%d d %d h", h/24, h%24)
	case h > 0:
		return fmt.Sprintf("%d h %d min", h, m)
	case m > 0:
		return fmt.Sprintf("%d min", m)
	}
	return "menos de un minuto"
}
