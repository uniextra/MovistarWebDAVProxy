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

// Si la sesión deja de valer antes de su caducidad (Movistar la invalida en el
// servidor), se renueva con la PLC en el acto, sin esperar a la caducidad.
func TestRenewDue_EarlyInvalidation(t *testing.T) {
	now := time.Date(2026, 11, 16, 8, 15, 0, 0, time.UTC)
	exp := now.Add(23 * time.Hour) // la cookie aún «vale» casi un día
	if renewDue(now, exp, time.Time{}, true, true, time.Hour) {
		t.Error("con sesión válida y lejos de caducar no toca renovar")
	}
	if !renewDue(now, exp, time.Time{}, true, false, time.Hour) {
		t.Error("sin sesión (401) y con PLC debe renovar ya")
	}
	if renewDue(now, exp, time.Time{}, false, false, time.Hour) {
		t.Error("sin PLC no se puede renovar")
	}
	if renewDue(now, exp, now.Add(-2*time.Minute), true, false, time.Hour) {
		t.Error("tras un intento reciente hay que esperar")
	}
	if !renewDue(now, time.Time{}, time.Time{}, true, false, time.Hour) {
		t.Error("sin cookie de sesión y con PLC debe renovar")
	}
}
