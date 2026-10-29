package logger

import (
	"fmt"
	"os"
	"sync"
	"time"
)

type RingBuffer struct {
	mu       sync.RWMutex
	capacity int
	entries  []string
}

var DefaultLogger = NewRingBuffer(500)

func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 500
	}
	return &RingBuffer{
		capacity: capacity,
		entries:  make([]string, 0, capacity),
	}
}

func (r *RingBuffer) Append(format string, args ...interface{}) {
	nowStr := time.Now().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%s] %s", nowStr, msg)

	fmt.Fprintln(os.Stdout, line)

	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.entries) >= r.capacity {
		r.entries = r.entries[1:]
	}
	r.entries = append(r.entries, line)
}

func (r *RingBuffer) GetLines() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res := make([]string, len(r.entries))
	copy(res, r.entries)
	return res
}

func (r *RingBuffer) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = make([]string, 0, r.capacity)
}

func Log(format string, args ...interface{}) {
	DefaultLogger.Append(format, args...)
}

func GetLogs() []string {
	return DefaultLogger.GetLines()
}

func ClearLogs() {
	DefaultLogger.Clear()
}
