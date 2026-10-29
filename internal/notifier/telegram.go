package notifier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/logger"
)

type TelegramNotifier struct {
	botToken string
	chatID   string
	client   *http.Client
	mu       sync.Mutex

	lastNotificationAt time.Time
	lastError          string
}

func NewTelegramNotifier(botToken, chatID string) *TelegramNotifier {
	return &TelegramNotifier{
		botToken: strings.TrimSpace(botToken),
		chatID:   strings.TrimSpace(chatID),
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (t *TelegramNotifier) IsConfigured() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.botToken != "" && t.chatID != ""
}

func (t *TelegramNotifier) UpdateCredentials(botToken, chatID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.botToken = strings.TrimSpace(botToken)
	t.chatID = strings.TrimSpace(chatID)
}

func (t *TelegramNotifier) Status() (bool, string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	lastAt := ""
	if !t.lastNotificationAt.IsZero() {
		lastAt = t.lastNotificationAt.Format("2006-01-02 15:04:05")
	}
	return t.botToken != "" && t.chatID != "", lastAt, t.lastError
}

func (t *TelegramNotifier) SendMessage(text string) error {
	t.mu.Lock()
	token := t.botToken
	chatID := t.chatID
	t.mu.Unlock()

	if token == "" || chatID == "" {
		return fmt.Errorf("telegram not configured")
	}

	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
		"link_preview_options": map[string]bool{
			"is_disabled": true,
		},
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		t.mu.Lock()
		t.lastError = fmt.Sprintf("Error de red: %v", err)
		t.mu.Unlock()
		logger.Log("[Telegram] ❌ Error conectando con Telegram: %v", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		errMsg := fmt.Sprintf("Telegram respondió HTTP %d: %s", resp.StatusCode, string(respBody))
		t.mu.Lock()
		t.lastError = errMsg
		t.mu.Unlock()
		logger.Log("[Telegram] ❌ %s", errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	t.mu.Lock()
	t.lastNotificationAt = time.Now()
	t.lastError = ""
	t.mu.Unlock()

	logger.Log("[Telegram] ✉️ Notificación enviada con éxito a chat %s.", chatID)
	return nil
}

func (t *TelegramNotifier) SendSessionExpiredAlert(provider, phone, details string) error {
	providerName := "Movistar Cloud"
	if strings.EqualFold(provider, "o2") {
		providerName = "O2 Cloud"
	}

	msg := fmt.Sprintf(
		"⚠️ *Sesión de %s caducada*\n\n"+
			"El Proxy WebDAV no puede acceder a tus archivos porque la sesión para la línea *%s* ha expirado.\n\n"+
			"Detalle: %s\n"+
			"Hora: %s\n\n"+
			"Por favor, abre el panel Web para revincular tu dispositivo.",
		providerName, phone, details, time.Now().Format("2006-01-02 15:04:05"),
	)
	return t.SendMessage(msg)
}

func (t *TelegramNotifier) SendTestAlert(provider string) error {
	providerName := "Movistar Cloud"
	if strings.EqualFold(provider, "o2") {
		providerName = "O2 Cloud"
	}

	msg := fmt.Sprintf(
		"🔔 *Notificaciones WebDAV de %s activas*\n\n"+
			"El Gateway WebDAV está conectado con Telegram y listo para avisarte ante cualquier evento o expiración de sesión.",
		providerName,
	)
	return t.SendMessage(msg)
}
