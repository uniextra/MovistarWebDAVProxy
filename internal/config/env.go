package config

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const (
	Version          = "v2.2.14"
	encryptedPrefix  = "enc:v1:"
	tokensEncryptEnv = "TOKENS_ENCRYPTION_KEY"
)

var (
	TokensDir      = "/app/tokens"
	fileLock       sync.Mutex
	phoneNonDigits = regexp.MustCompile(`[^\d]`)
	botTokenRegex  = regexp.MustCompile(`^\d+:[A-Za-z0-9_-]+$`)
	chatIDRegex    = regexp.MustCompile(`^-?\d+$|^@[A-Za-z0-9_]{5,}$`)
)

func init() {
	if custom := os.Getenv("TOKENS_DIR"); custom != "" {
		TokensDir = custom
	} else if _, err := os.Stat("/app/tokens"); err == nil {
		TokensDir = "/app/tokens"
	} else if _, err := os.Stat("tokens"); err == nil {
		TokensDir = "tokens"
	} else if _, err := os.Stat("/.dockerenv"); err == nil {
		TokensDir = "/app/tokens"
	} else {
		TokensDir = "tokens"
	}
}

type AccountConfig struct {
	Provider         string `json:"provider"`
	Phone            string `json:"phone"`
	JSessionID       string `json:"jsessionid"`
	ValidationKey    string `json:"validationkey"`
	DeviceID         string `json:"device_id"`
	DeviceName       string `json:"device_name"`
	RefreshToken     string `json:"refresh_token"`
	AccessToken      string `json:"access_token"`
	LastRefreshDate  int64  `json:"last_refresh_date"`
	ExpiresIn        string `json:"expires_in"`
	TokenPath        string `json:"token_path"`
	TelegramBotToken string `json:"telegram_bot_token"`
	TelegramChatID   string `json:"telegram_chat_id"`
}

func SanitizePhone(phone string) string {
	cleaned := phoneNonDigits.ReplaceAllString(phone, "")
	if len(cleaned) > 9 && strings.HasPrefix(cleaned, "34") {
		cleaned = cleaned[2:]
	}
	return cleaned
}

func ValidateBotToken(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return true
	}
	return botTokenRegex.MatchString(token)
}

func ValidateChatID(chatID string) bool {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return true
	}
	return chatIDRegex.MatchString(chatID)
}

func sanitizeEnvValue(val string) (string, error) {
	if strings.ContainsAny(val, "\n\r\x00") {
		return "", fmt.Errorf("valor inválido: contiene saltos de línea o caracteres de control")
	}
	escaped := strings.ReplaceAll(val, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return escaped, nil
}

func getEncryptionKey() ([]byte, bool) {
	rawKey := strings.TrimSpace(os.Getenv(tokensEncryptEnv))
	if rawKey == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(rawKey))
	return sum[:], true
}

func encryptValueIfNeeded(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	key, enabled := getEncryptionKey()
	if !enabled {
		return plain, nil
	}
	if strings.HasPrefix(plain, encryptedPrefix) {
		return plain, nil
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return encryptedPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

func decryptValueIfNeeded(val string) string {
	if !strings.HasPrefix(val, encryptedPrefix) {
		return val
	}
	key, enabled := getEncryptionKey()
	if !enabled {
		return ""
	}
	rawB64 := strings.TrimPrefix(val, encryptedPrefix)
	data, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil {
		return ""
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return ""
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ""
	}
	if len(data) < gcm.NonceSize() {
		return ""
	}
	nonce := data[:gcm.NonceSize()]
	ciphertext := data[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return ""
	}
	return string(plain)
}

func unescapeEnvValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
		quote := v[0]
		v = v[1 : len(v)-1]
		if quote == '"' {
			v = strings.ReplaceAll(v, `\"`, `"`)
			v = strings.ReplaceAll(v, `\\`, `\`)
		}
	} else {
		v = strings.Trim(v, `"'`)
	}
	return decryptValueIfNeeded(v)
}

func parseEnvFile(path string) (map[string]string, error) {
	vars := make(map[string]string)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		k := strings.TrimSpace(parts[0])
		v := unescapeEnvValue(parts[1])
		vars[k] = v
	}
	return vars, scanner.Err()
}

func LoadAccountConfig(phone string) AccountConfig {
	fileLock.Lock()
	defer fileLock.Unlock()

	cfg := AccountConfig{
		Provider:   "movistar",
		DeviceName: "PC Windows",
		ExpiresIn:  "3600",
	}

	phoneClean := SanitizePhone(phone)
	var candidates []string

	if phoneClean != "" {
		candidates = append(candidates,
			filepath.Join(TokensDir, fmt.Sprintf("account_%s.env", phoneClean)),
			filepath.Join(TokensDir, fmt.Sprintf("%s.env", phoneClean)),
		)
	}

	// Buscar cualquier *.env en TokensDir (priorizando account_*.env sobre otros .env)
	if entries, err := os.ReadDir(TokensDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasPrefix(e.Name(), "account_") && strings.HasSuffix(e.Name(), ".env") {
				candidates = append(candidates, filepath.Join(TokensDir, e.Name()))
			}
		}
		for _, e := range entries {
			if !e.IsDir() && !strings.HasPrefix(e.Name(), "account_") && strings.HasSuffix(e.Name(), ".env") && e.Name() != ".env" {
				candidates = append(candidates, filepath.Join(TokensDir, e.Name()))
			}
		}
	}

	candidates = append(candidates,
		filepath.Join(TokensDir, ".env"),
		".env",
	)

	for _, cand := range candidates {
		if vars, err := parseEnvFile(cand); err == nil && len(vars) > 0 {
			cfg.TokenPath = cand
			if prov, ok := vars["MOVISTAR_PROVIDER"]; ok && strings.EqualFold(prov, "o2") {
				cfg.Provider = "o2"
			} else if prov, ok := vars["PROVIDER"]; ok && strings.EqualFold(prov, "o2") {
				cfg.Provider = "o2"
			} else {
				cfg.Provider = "movistar"
			}

			if p, ok := vars["MOVISTAR_PHONE"]; ok && p != "" {
				cfg.Phone = SanitizePhone(p)
			}
			if cfg.Phone == "" && phoneClean != "" {
				cfg.Phone = phoneClean
			}
			cfg.JSessionID = vars["MOVISTAR_JSESSIONID"]
			cfg.ValidationKey = vars["MOVISTAR_VALIDATIONKEY"]
			cfg.DeviceID = vars["MOVISTAR_DEVICE_ID"]
			if name, ok := vars["MOVISTAR_DEVICE_NAME"]; ok && name != "" {
				cfg.DeviceName = name
			}
			cfg.RefreshToken = vars["MOVISTAR_REFRESH_TOKEN"]
			cfg.AccessToken = vars["MOVISTAR_ACCESS_TOKEN"]
			if exp, ok := vars["MOVISTAR_EXPIRES_IN"]; ok && exp != "" {
				cfg.ExpiresIn = exp
			}
			if lrd, ok := vars["MOVISTAR_LAST_REFRESH_DATE"]; ok && lrd != "" {
				if parsedLrd, err := strconv.ParseInt(lrd, 10, 64); err == nil {
					cfg.LastRefreshDate = parsedLrd
				}
			}

			if tgToken, ok := vars["MOVISTAR_TELEGRAM_BOT_TOKEN"]; ok && tgToken != "" {
				cfg.TelegramBotToken = tgToken
			} else if tgToken, ok := vars["TELEGRAM_BOT_TOKEN"]; ok && tgToken != "" {
				cfg.TelegramBotToken = tgToken
			} else if envTg := os.Getenv("TELEGRAM_BOT_TOKEN"); envTg != "" {
				cfg.TelegramBotToken = envTg
			}

			if tgChat, ok := vars["MOVISTAR_TELEGRAM_CHAT_ID"]; ok && tgChat != "" {
				cfg.TelegramChatID = tgChat
			} else if tgChat, ok := vars["TELEGRAM_CHAT_ID"]; ok && tgChat != "" {
				cfg.TelegramChatID = tgChat
			} else if envChat := os.Getenv("TELEGRAM_CHAT_ID"); envChat != "" {
				cfg.TelegramChatID = envChat
			}

			if cfg.RefreshToken != "" || cfg.AccessToken != "" || (cfg.JSessionID != "" && cfg.ValidationKey != "") {
				return cfg
			}
		}
	}

	return cfg
}

func SaveAccountConfig(cfg AccountConfig) error {
	fileLock.Lock()
	defer fileLock.Unlock()

	if !ValidateBotToken(cfg.TelegramBotToken) {
		return fmt.Errorf("formato de token de Telegram inválido")
	}
	if !ValidateChatID(cfg.TelegramChatID) {
		return fmt.Errorf("formato de Chat ID de Telegram inválido")
	}

	if err := os.MkdirAll(TokensDir, 0700); err != nil {
		return err
	}

	phoneClean := SanitizePhone(cfg.Phone)
	prov := strings.ToLower(cfg.Provider)
	if prov != "o2" {
		prov = "movistar"
	}

	var lines []string
	addLine := func(key, val string) error {
		esc, err := sanitizeEnvValue(val)
		if err != nil {
			return fmt.Errorf("campo %s: %w", key, err)
		}
		lines = append(lines, fmt.Sprintf(`%s="%s"`, key, esc))
		return nil
	}
	addSecretLine := func(key, val string) error {
		if _, err := sanitizeEnvValue(val); err != nil {
			return fmt.Errorf("campo %s: %w", key, err)
		}
		enc, err := encryptValueIfNeeded(val)
		if err != nil {
			return fmt.Errorf("campo %s (cifrado): %w", key, err)
		}
		return addLine(key, enc)
	}

	if err := addLine("MOVISTAR_PROVIDER", prov); err != nil {
		return err
	}
	if err := addLine("MOVISTAR_PHONE", phoneClean); err != nil {
		return err
	}
	if err := addSecretLine("MOVISTAR_JSESSIONID", cfg.JSessionID); err != nil {
		return err
	}
	if err := addSecretLine("MOVISTAR_VALIDATIONKEY", cfg.ValidationKey); err != nil {
		return err
	}
	if cfg.DeviceID != "" {
		if err := addLine("MOVISTAR_DEVICE_ID", cfg.DeviceID); err != nil {
			return err
		}
	}
	if cfg.DeviceName != "" {
		if err := addLine("MOVISTAR_DEVICE_NAME", cfg.DeviceName); err != nil {
			return err
		}
	}
	if cfg.RefreshToken != "" {
		if err := addSecretLine("MOVISTAR_REFRESH_TOKEN", cfg.RefreshToken); err != nil {
			return err
		}
	}
	if cfg.AccessToken != "" {
		if err := addSecretLine("MOVISTAR_ACCESS_TOKEN", cfg.AccessToken); err != nil {
			return err
		}
	}
	lines = append(lines, fmt.Sprintf(`MOVISTAR_LAST_REFRESH_DATE="%d"`, cfg.LastRefreshDate))
	if cfg.ExpiresIn != "" {
		if err := addLine("MOVISTAR_EXPIRES_IN", cfg.ExpiresIn); err != nil {
			return err
		}
	} else {
		lines = append(lines, `MOVISTAR_EXPIRES_IN="3600"`)
	}
	if cfg.TelegramBotToken != "" {
		if err := addSecretLine("MOVISTAR_TELEGRAM_BOT_TOKEN", cfg.TelegramBotToken); err != nil {
			return err
		}
	}
	if cfg.TelegramChatID != "" {
		if err := addLine("MOVISTAR_TELEGRAM_CHAT_ID", cfg.TelegramChatID); err != nil {
			return err
		}
	}

	content := strings.Join(lines, "\n") + "\n"

	target := filepath.Join(TokensDir, ".env")
	if phoneClean != "" {
		target = filepath.Join(TokensDir, fmt.Sprintf("account_%s.env", phoneClean))
	}

	tmpPath := fmt.Sprintf("%s.tmp.%d", target, os.Getpid())
	if err := os.WriteFile(tmpPath, []byte(content), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		_ = os.Remove(target)
		if err2 := os.Rename(tmpPath, target); err2 != nil {
			if err3 := os.WriteFile(target, []byte(content), 0600); err3 != nil {
				_ = os.Remove(tmpPath)
				return err3
			}
			_ = os.Remove(tmpPath)
		}
	}

	// Si guardamos en account_<phone>.env, eliminar el .env duplicado legacy para mantener un único fichero por cuenta
	if phoneClean != "" {
		_ = os.Remove(filepath.Join(TokensDir, ".env"))
	}

	return nil
}

func DeleteAccountConfig(phone string) error {
	fileLock.Lock()
	defer fileLock.Unlock()

	phoneClean := SanitizePhone(phone)
	var targets []string
	if phoneClean != "" {
		targets = append(targets,
			filepath.Join(TokensDir, fmt.Sprintf("account_%s.env", phoneClean)),
			filepath.Join(TokensDir, fmt.Sprintf("%s.env", phoneClean)),
		)
	}
	targets = append(targets, filepath.Join(TokensDir, ".env"))

	if entries, err := os.ReadDir(TokensDir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "account_") && strings.HasSuffix(e.Name(), ".env") {
				if phoneClean == "" || strings.Contains(e.Name(), phoneClean) {
					targets = append(targets, filepath.Join(TokensDir, e.Name()))
				}
			}
		}
	}

	for _, target := range targets {
		_ = os.Remove(target)
	}
	return nil
}
