package webclient

import (
	"testing"
	"time"
)

func TestShouldRenew(t *testing.T) {
	now := time.Date(2026, 11, 14, 19, 0, 0, 0, time.UTC)
	exp := now.Add(48 * time.Minute) // caduca dentro de 48 min
	cases := []struct {
		name     string
		expires  time.Time
		lastTry  time.Time
		remember bool
		want     bool
	}{
		{"dentro del margen", exp, time.Time{}, true, true},
		{"sin PLC no se puede", exp, time.Time{}, false, false},
		{"lejos de caducar", now.Add(5 * time.Hour), time.Time{}, true, false},
		{"caducidad desconocida", time.Time{}, time.Time{}, true, false},
		{"ya caducada", now.Add(-time.Minute), time.Time{}, true, true},
		{"intento reciente", exp, now.Add(-3 * time.Minute), true, false},
		{"intento antiguo", exp, now.Add(-11 * time.Minute), true, true},
	}
	for _, c := range cases {
		if got := shouldRenew(now, c.expires, c.lastTry, c.remember, time.Hour); got != c.want {
			t.Errorf("%s: %v, esperado %v", c.name, got, c.want)
		}
	}
}

func TestRenewBefore(t *testing.T) {
	t.Setenv("WEB_SESSION_RENEW_BEFORE", "")
	if d := renewBefore(); d != time.Hour {
		t.Errorf("por defecto %v, esperado 1h", d)
	}
	t.Setenv("WEB_SESSION_RENEW_BEFORE", "90m")
	if d := renewBefore(); d != 90*time.Minute {
		t.Errorf("%v, esperado 90m", d)
	}
	t.Setenv("WEB_SESSION_RENEW_BEFORE", "basura")
	if d := renewBefore(); d != time.Hour {
		t.Errorf("valor inválido: %v, esperado 1h", d)
	}
}
