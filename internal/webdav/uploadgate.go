package webdav

import (
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// uploadGate reparte los huecos de subida a la nube (WEBDAV_MAX_UPLOADS) entre todas
// las subidas, entren por donde entren (montaje, SMB sobre el montaje, WebDAV
// directo, copia masiva). Las subidas marcadas como masivas (cabecera
// X-Upload-Priority: bulk, p. ej. la de un script de migración) solo usan los huecos
// que no esperan las normales y nunca ocupan el último: así lo que sube una persona
// empieza enseguida aunque la copia masiva esté llenando la cola.
type uploadGate struct {
	mu        sync.Mutex
	capacity  int
	used      int
	waiting   []chan struct{} // subidas normales en espera (primero en llegar, primero en salir)
	waitingLo []chan struct{} // subidas masivas en espera
}

func newUploadGate(capacity int) *uploadGate {
	if capacity < 1 {
		capacity = 1
	}
	return &uploadGate{capacity: capacity}
}

// bulkLimit es cuántos huecos pueden ocupar a la vez las subidas masivas.
func (g *uploadGate) bulkLimit() int {
	if g.capacity > 1 {
		return g.capacity - 1
	}
	return 1
}

func (g *uploadGate) canTake(bulk bool) bool {
	if g.used >= g.capacity {
		return false
	}
	if bulk {
		return len(g.waiting) == 0 && g.used < g.bulkLimit()
	}
	return true
}

// tryAcquire toma un hueco si lo hay ahora mismo.
func (g *uploadGate) tryAcquire(bulk bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.canTake(bulk) && (bulk || len(g.waiting) == 0) {
		g.used++
		return true
	}
	return false
}

// acquire espera un hueco respetando la prioridad.
func (g *uploadGate) acquire(bulk bool) {
	g.mu.Lock()
	if g.canTake(bulk) && (bulk || len(g.waiting) == 0) {
		g.used++
		g.mu.Unlock()
		return
	}
	ch := make(chan struct{})
	if bulk {
		g.waitingLo = append(g.waitingLo, ch)
	} else {
		g.waiting = append(g.waiting, ch)
	}
	g.mu.Unlock()
	<-ch
}

// release libera un hueco y se lo da primero a las subidas normales en espera.
func (g *uploadGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.used--
	for g.used < g.capacity {
		switch {
		case len(g.waiting) > 0:
			ch := g.waiting[0]
			g.waiting = g.waiting[1:]
			g.used++
			close(ch)
		case len(g.waitingLo) > 0 && g.used < g.bulkLimit():
			ch := g.waitingLo[0]
			g.waitingLo = g.waitingLo[1:]
			g.used++
			close(ch)
		default:
			return
		}
	}
}

// queueRAMBudget es la RAM total que pueden ocupar los cuerpos de subida en memoria
// (búfer inicial, archivos pequeños y cola). Es dinámico: la mitad de la memoria
// disponible de la máquina (MemAvailable) por encima de una reserva para el sistema
// (1/8 de la RAM, entre 256 MB y 1 GiB), más lo que ya ocupan; se recalcula como mucho cada 2 s. El máximo es
// WEBDAV_QUEUE_RAM_MB o, por defecto, solo lo de las subidas en curso (huecos ×
// búfer inicial): sin cola en memoria, el cliente espera (velocidad 0).
func queueRAMBudget() int64 {
	limit := int64(uploads.capacityNow()) * int64(GetUploadRAMBufferMaxBytes())
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("WEBDAV_QUEUE_RAM_MB"))); err == nil && v >= 0 {
		limit = int64(v) << 20
	}
	budget := dynamicRAMBudget()
	if limit >= 0 && (budget < 0 || budget > limit) {
		budget = limit
	}
	if budget < 0 {
		budget = 512 << 20 // sin /proc/meminfo (p. ej. macOS): valor fijo
	}
	// Siempre cabe al menos el búfer de una subida: si no, con poca memoria libre y
	// sin cola en disco, ninguna subida podría empezar nunca.
	if one := int64(GetUploadRAMBufferMaxBytes()); budget < one {
		budget = one
	}
	return budget
}

var ramProbe struct {
	sync.Mutex
	at        time.Time
	available int64 // bytes; -1 si no se puede leer
	total     int64 // MemTotal en bytes; -1 si no se puede leer
}

// dynamicRAMBudget devuelve el presupuesto según la memoria libre, o -1 si no se sabe.
func dynamicRAMBudget() int64 {
	ramProbe.Lock()
	if time.Since(ramProbe.at) > 2*time.Second {
		ramProbe.available = memAvailable()
		ramProbe.total = memInfo("MemTotal:")
		ramProbe.at = time.Now()
	}
	avail, total := ramProbe.available, ramProbe.total
	ramProbe.Unlock()
	if avail < 0 {
		return -1
	}
	// Reserva para el sistema: 1/8 de la RAM total, entre 256 MB y 1 GiB (512 MB en
	// una Raspberry Pi de 4 GB). Con 1 GiB fijo, una máquina pequeña se quedaba sin
	// presupuesto.
	reserve := int64(1 << 30)
	if total > 0 {
		reserve = total / 8
		if reserve < 256<<20 {
			reserve = 256 << 20
		}
		if reserve > 1<<30 {
			reserve = 1 << 30
		}
	}
	free := (avail - reserve) / 2
	if free < 0 {
		free = 0
	}
	return queuedRAM.Load() + free
}

// memAvailable lee MemAvailable de /proc/meminfo (bytes), o -1.
func memAvailable() int64 { return memInfo("MemAvailable:") }

// memInfo lee un campo de /proc/meminfo (bytes), o -1.
func memInfo(field string) int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, field) {
			f := strings.Fields(line)
			if len(f) >= 2 {
				if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					return kb << 10
				}
			}
		}
	}
	return -1
}

var queuedRAM atomic.Int64

// reserveQueueRAM reserva n bytes del presupuesto de RAM de la cola.
func reserveQueueRAM(n int64) bool {
	for {
		cur := queuedRAM.Load()
		if cur+n > queueRAMBudget() {
			return false
		}
		if queuedRAM.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

func releaseQueueRAM(n int64) {
	if n > 0 {
		queuedRAM.Add(-n)
	}
}

// uploadGateSet cambia el número de huecos (al arrancar, con WEBDAV_MAX_UPLOADS).
func uploadGateSet(capacity int) {
	if capacity < 1 {
		capacity = 1
	}
	uploads.mu.Lock()
	uploads.capacity = capacity
	uploads.mu.Unlock()
}

func (g *uploadGate) capacityNow() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.capacity
}

// queueDiskBudget es el disco total que pueden ocupar las subidas en cola
// (WEBDAV_QUEUE_DISK_MB). Con la RAM y el disco de la cola llenos, el proxy deja de
// leer del cliente (le llega velocidad 0) hasta que haya hueco de subida.
// Sin la variable: con copia local de las subidas (WEBDAV_UPLOAD_SPOOL, por defecto),
// sin límite (un cuerpo que no puede subir en flujo va entero al spool y nunca se hace
// esperar al cliente, que puede depender de otra petición para seguir enviando:
// interbloqueo restic -> rclone); sin copia local, 0 (nada en disco).
func queueDiskBudget() int64 {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("WEBDAV_QUEUE_DISK_MB"))); err == nil && v >= 0 {
		return int64(v) << 20
	}
	if uploadSpoolEnabled() {
		return math.MaxInt64 / 2
	}
	return 0
}

var queuedDisk atomic.Int64

func reserveQueueDisk(n int64) bool {
	for {
		cur := queuedDisk.Load()
		if cur+n > queueDiskBudget() {
			return false
		}
		if queuedDisk.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

func releaseQueueDisk(n int64) {
	if n > 0 {
		queuedDisk.Add(-n)
	}
}
