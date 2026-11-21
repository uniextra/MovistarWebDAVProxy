package main

import (
	"strings"
	"testing"
	"time"
)

// Un problema avisa solo si dura más de healthAlertAfter, una vez, y avisa al recuperarse.
func TestHealthCheckWaitsAndRecovers(t *testing.T) {
	ok := false
	c := &healthCheck{key: "prueba_salud", title: "mal", okTitle: "bien", ok: func() bool { return ok }}
	t0 := time.Now()
	c.step(t0)
	if c.alerted {
		t.Fatal("no debe avisar al momento")
	}
	c.step(t0.Add(healthAlertAfter - time.Second))
	if c.alerted {
		t.Fatal("no debe avisar antes de healthAlertAfter")
	}
	c.step(t0.Add(healthAlertAfter))
	if !c.alerted {
		t.Fatal("debe avisar pasado healthAlertAfter")
	}
	ok = true
	c.step(t0.Add(healthAlertAfter + time.Minute))
	if c.alerted || !c.badSince.IsZero() {
		t.Fatal("al recuperarse debe quedar limpio")
	}
	ok = false
	c.step(t0.Add(2 * healthAlertAfter))
	c.step(t0.Add(2*healthAlertAfter + 30*time.Second))
	if c.alerted {
		t.Fatal("un corte breve tras recuperarse no debe avisar")
	}
}

// El aviso de límite de tamaño acota el límite con lo rechazado y lo aceptado.
func TestSizeLimitText(t *testing.T) {
	if got := sizeLimitText(100<<20, 0); !strings.Contains(got, "por debajo de 100 MiB") {
		t.Fatalf("sin aceptadas: %s", got)
	}
	if got := sizeLimitText(100<<20, 95<<20); !strings.Contains(got, "entre 95 y 100 MiB") {
		t.Fatalf("con aceptadas: %s", got)
	}
}
