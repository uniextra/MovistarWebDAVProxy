package notifier

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/config"
)

// fakeTelegram imita la API de Telegram y guarda lo que recibe.
type fakeTelegram struct {
	mu   sync.Mutex
	sent []map[string]interface{}
}

func (f *fakeTelegram) start(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&m)
		f.mu.Lock()
		f.sent = append(f.sent, m)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	old, oldDefault := apiBase, Default
	apiBase = srv.URL
	Default = &TelegramNotifier{client: srv.Client(), lastAlerts: map[string]time.Time{}}
	t.Cleanup(func() { srv.Close(); apiBase, Default = old, oldDefault })
}

func TestAlertIsEscapedHTML(t *testing.T) {
	f := &fakeTelegram{}
	f.start(t)
	Default.UpdateCredentials("123:abc", "42")
	if err := Alert("k", "⚠️", "Título <x>", "desc & más", "a_b*c"); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0]["parse_mode"] != "HTML" {
		t.Fatalf("enviado: %v", f.sent)
	}
	text := f.sent[0]["text"].(string)
	if !strings.Contains(text, "<b>Título &lt;x&gt;</b>") || !strings.Contains(text, "desc &amp; más") || !strings.Contains(text, "a_b*c") {
		t.Fatalf("texto: %q", text)
	}
	if err := Alert("k", "", "otra", "x", ""); err != nil || len(f.sent) != 1 {
		t.Fatal("la misma clave en menos de una hora no debe reenviarse")
	}
}

func TestSendTestWithKeepsActiveCredentials(t *testing.T) {
	f := &fakeTelegram{}
	f.start(t)
	Default.UpdateCredentials("111:activo", "1")
	if err := SendTestWith("222:prueba", "2", "movistar"); err != nil {
		t.Fatal(err)
	}
	if tok, chat := Default.Credentials(); tok != "111:activo" || chat != "1" {
		t.Fatalf("la prueba cambió las credenciales activas: %s %s", tok, chat)
	}
	if len(f.sent) != 1 || f.sent[0]["chat_id"] != "2" {
		t.Fatalf("enviado: %v", f.sent)
	}
}

func TestStoredRoundTrip(t *testing.T) {
	f := &fakeTelegram{}
	f.start(t)
	old := config.TokensDir
	config.TokensDir = t.TempDir()
	t.Cleanup(func() { config.TokensDir = old })
	if LoadStored() {
		t.Fatal("sin archivo no hay configuración")
	}
	if err := SaveStored("555:guardado", "77"); err != nil {
		t.Fatal(err)
	}
	Default.UpdateCredentials("", "")
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("TELEGRAM_CHAT_ID", "")
	if !LoadStored() {
		t.Fatal("no se cargó lo guardado")
	}
	if tok, chat := Default.Credentials(); tok != "555:guardado" || chat != "77" {
		t.Fatalf("%s %s", tok, chat)
	}
}
