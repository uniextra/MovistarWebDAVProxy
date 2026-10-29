package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var (
	TokensDir = "/app/tokens"
	fileLock  sync.Mutex
)

func init() {
	if custom := os.Getenv("TOKENS_DIR"); custom != "" {
		TokensDir = custom
	} else if _, err := os.Stat("tokens"); err == nil {
		TokensDir = "tokens"
	}
}

type AccountConfig struct {
	Phone           string `json:"phone"`
	JSessionID      string `json:"jsessionid"`
	ValidationKey   string `json:"validationkey"`
	DeviceID        string `json:"device_id"`
	DeviceName      string `json:"device_name"`
	RefreshToken    string `json:"refresh_token"`
	AccessToken     string `json:"access_token"`
	LastRefreshDate int64  `json:"last_refresh_date"`
	ExpiresIn       string `json:"expires_in"`
	TokenPath       string `json:"token_path"`
}

func SanitizePhone(phone string) string {
	reg := regexp.MustCompile(`[^\d]`)
	cleaned := reg.ReplaceAllString(phone, "")
	if len(cleaned) > 9 && strings.HasPrefix(cleaned, "34") {
		cleaned = cleaned[2:]
	}
	return cleaned
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
		v := strings.TrimSpace(parts[1])
		v = strings.Trim(v, `"'`)
		vars[k] = v
	}
	return vars, scanner.Err()
}

func LoadAccountConfig(phone string) AccountConfig {
	fileLock.Lock()
	defer fileLock.Unlock()

	cfg := AccountConfig{
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

	candidates = append(candidates,
		filepath.Join(TokensDir, ".env"),
	)

	// Buscar cualquier account_*.env si no se ha encontrado ninguno
	if entries, err := os.ReadDir(TokensDir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "account_") && strings.HasSuffix(e.Name(), ".env") {
				candidates = append(candidates, filepath.Join(TokensDir, e.Name()))
			}
		}
	}

	for _, cand := range candidates {
		if vars, err := parseEnvFile(cand); err == nil && len(vars) > 0 {
			cfg.TokenPath = cand
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

			if cfg.RefreshToken != "" || (cfg.JSessionID != "" && cfg.ValidationKey != "") {
				return cfg
			}
		}
	}

	return cfg
}

func SaveAccountConfig(cfg AccountConfig) error {
	fileLock.Lock()
	defer fileLock.Unlock()

	if err := os.MkdirAll(TokensDir, 0755); err != nil {
		return err
	}

	phoneClean := SanitizePhone(cfg.Phone)
	lines := []string{
		fmt.Sprintf(`MOVISTAR_PHONE="%s"`, phoneClean),
		fmt.Sprintf(`MOVISTAR_JSESSIONID="%s"`, cfg.JSessionID),
		fmt.Sprintf(`MOVISTAR_VALIDATIONKEY="%s"`, cfg.ValidationKey),
	}

	if cfg.DeviceID != "" {
		lines = append(lines, fmt.Sprintf(`MOVISTAR_DEVICE_ID="%s"`, cfg.DeviceID))
	}
	if cfg.DeviceName != "" {
		lines = append(lines, fmt.Sprintf(`MOVISTAR_DEVICE_NAME="%s"`, cfg.DeviceName))
	}
	if cfg.RefreshToken != "" {
		lines = append(lines, fmt.Sprintf(`MOVISTAR_REFRESH_TOKEN="%s"`, cfg.RefreshToken))
	}
	if cfg.AccessToken != "" {
		lines = append(lines, fmt.Sprintf(`MOVISTAR_ACCESS_TOKEN="%s"`, cfg.AccessToken))
	}
	lines = append(lines, fmt.Sprintf(`MOVISTAR_LAST_REFRESH_DATE="%d"`, cfg.LastRefreshDate))
	if cfg.ExpiresIn != "" {
		lines = append(lines, fmt.Sprintf(`MOVISTAR_EXPIRES_IN="%s"`, cfg.ExpiresIn))
	} else {
		lines = append(lines, `MOVISTAR_EXPIRES_IN="3600"`)
	}

	content := strings.Join(lines, "\n") + "\n"

	var targets []string
	if phoneClean != "" {
		targets = append(targets, filepath.Join(TokensDir, fmt.Sprintf("account_%s.env", phoneClean)))
	}
	targets = append(targets, filepath.Join(TokensDir, ".env"))

	for _, target := range targets {
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
	}
	return nil
}
