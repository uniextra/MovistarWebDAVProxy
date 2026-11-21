package sapi

import (
	"sync"
	"time"

	"movistar-webdav-proxy/internal/logger"
)

// Una subida parada (Movistar deja de leer) solo cuenta como rechazo por tamaño si es
// la única: cuando se paran varias a la vez es la red, no el tamaño, y bajar el trozo
// sería un error.
const stallWindow = 90 * time.Second

var stallLog struct {
	sync.Mutex
	at []time.Time
}

// stallSleep espera antes de decidir (variable para los tests).
var stallSleep = time.Sleep

func noteUploadStall(size int64, now time.Time) {
	stallLog.Lock()
	others := countStallsLocked(now.Add(-stallWindow), now)
	stallLog.at = append(stallLog.at, now)
	stallLog.Unlock()
	if others > 0 {
		logger.Log("[SAPI] 🔌 Varias subidas paradas a la vez: es la red, no cuenta como rechazo por tamaño.")
		return
	}
	go func() {
		stallSleep(stallWindow)
		stallLog.Lock()
		others := countStallsLocked(now.Add(time.Nanosecond), now.Add(stallWindow))
		stallLog.Unlock()
		if others > 0 {
			logger.Log("[SAPI] 🔌 La subida parada coincidió con otras: no cuenta como rechazo por tamaño.")
			return
		}
		if UploadRejected != nil && size > 0 {
			UploadRejected(size)
		}
	}()
}

// countStallsLocked cuenta las subidas paradas en [from, to] y olvida las antiguas.
func countStallsLocked(from, to time.Time) int {
	keep := stallLog.at[:0]
	n := 0
	for _, t := range stallLog.at {
		if time.Since(t) < 10*time.Minute {
			keep = append(keep, t)
		}
		if !t.Before(from) && !t.After(to) {
			n++
		}
	}
	stallLog.at = keep
	return n
}
