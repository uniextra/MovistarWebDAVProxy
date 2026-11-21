package notifier

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"movistar-webdav-proxy/internal/config"
)

// Configuración de Telegram propia del proxy (modo web): en el modo web no hay
// cuenta del cliente de escritorio (por teléfono) donde guardarla. El token va
// cifrado con TOKENS_ENCRYPTION_KEY, como las demás credenciales.
type storedTelegram struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

func storeFile() string { return filepath.Join(config.TokensDir, "telegram.json") }

// LoadStored aplica la configuración guardada en el proxy, si la hay. Devuelve si había.
func LoadStored() bool {
	b, err := os.ReadFile(storeFile())
	if err != nil {
		return false
	}
	var st storedTelegram
	if json.Unmarshal(b, &st) != nil {
		return false
	}
	token := config.DecryptSecret(st.BotToken)
	if token == "" || st.ChatID == "" {
		return false
	}
	Default.UpdateCredentials(token, st.ChatID)
	return true
}

// SaveStored guarda la configuración en el proxy y la activa.
func SaveStored(botToken, chatID string) error {
	if botToken == "" || chatID == "" {
		return errors.New("faltan el token o el chat")
	}
	enc, err := config.EncryptSecret(botToken)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(storedTelegram{BotToken: enc, ChatID: chatID}, "", "  ")
	if err := os.MkdirAll(config.TokensDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(storeFile()+".tmp", b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(storeFile()+".tmp", storeFile()); err != nil {
		return err
	}
	Default.UpdateCredentials(botToken, chatID)
	return nil
}
