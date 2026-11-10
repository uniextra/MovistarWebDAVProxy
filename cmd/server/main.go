package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"
	"movistar-webdav-proxy/internal/webdav"
	"movistar-webdav-proxy/internal/webui"
)

const AppVersion = config.Version

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-healthcheck" || os.Args[1] == "--healthcheck") {
		os.Exit(runHealthcheck())
	}

	logger.Log("=====================================================")
	logger.Log("🚀 Movistar Cloud WebDAV Proxy %s (Nativo en Go)", AppVersion)
	logger.Log("=====================================================")

	davAuth := config.LoadWebDAVAuthConfig()
	uiAuth := config.LoadWebUIAuthConfig()

	if davAuth.AuthDisabled {
		logger.Log("=====================================================")
		logger.Log("[WARNING] ⚠️ AUTH_DISABLED=true: Servidor WebDAV y Panel Web expuestos SIN autenticación.")
		logger.Log("=====================================================")
	} else if !davAuth.HasCredentials() {
		if davAuth.RequireAuth {
			logger.Log("[FATAL] ❌ REQUIRE_AUTH=true pero no se han configurado WEBDAV_USER y WEBDAV_PASSWORD. Abortando arranque.")
			os.Exit(1)
		}
		logger.Log("=====================================================")
		logger.Log("[WARNING] ⚠️ No se han definido WEBDAV_USER / WEBDAV_PASSWORD.")
		logger.Log("[WARNING] ⚠️ Arrancando en modo retrocompatible sin Basic Auth para no interrumpir contenedores existentes.")
		logger.Log("[WARNING] ⚠️ Configura WEBDAV_USER y WEBDAV_PASSWORD en tu entorno para proteger el acceso.")
		logger.Log("=====================================================")
	} else {
		logger.Log("[Security] 🔒 Basic Auth habilitado para WebDAV (usuario: %s) y Panel Web (usuario: %s)", logger.Mask(davAuth.Username), logger.Mask(uiAuth.Username))
	}

	cfg := config.LoadAccountConfig("")
	if cfg.DeviceID != "" {
		logger.Log("[WebDAV] 💻 Modo Autenticación: CLIENTE OFICIAL WINDOWS (%s)", logger.Mask(cfg.DeviceID))
	} else if cfg.RefreshToken != "" {
		logger.Log("[WebDAV] 📱 Modo Autenticación: Tokens OAuth2 Activos")
	} else if cfg.JSessionID != "" {
		logger.Log("[WebDAV] 🌐 Modo Autenticación: Sesión Web (Fallback)")
	} else {
		logger.Log("[!] ⚠️ No se encontraron credenciales. Abre el panel web para vincular.")
	}

	client, err := sapi.NewClient(cfg)
	if err != nil {
		logger.Log("[FATAL] Error inicializando cliente SAPI: %v", err)
		os.Exit(1)
	}

	// 1. Servidor WebDAV (sin ReadTimeout/WriteTimeout globales para no cortar transferencias grandes)
	webdavServer := webdav.NewServerWithAuth(client, davAuth)

	// 2. Worker KeepAlive en segundo plano con Context para shutdown limpio
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runKeepAlive(ctx, client, webdavServer.IsRunning)

	webdavPort := "8080"
	if p := strings.TrimSpace(os.Getenv("WEBDAV_PORT")); p != "" {
		webdavPort = p
	}
	davHTTP := &http.Server{
		Addr:              ":" + webdavPort,
		Handler:           webdavServer,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ConnContext:       webdavServer.ConnContext,
		ConnState:         webdavServer.ConnState,
	}

	go func() {
		logger.Log("[WebDAV] ✔ Servidor WebDAV escuchando en :%s", webdavPort)
		if err := davHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Log("[FATAL] Error en servidor WebDAV: %v", err)
		}
	}()

	// 3. Servidor Web UI
	uiServer := webui.NewServerWithAuth(client, webdavServer, uiAuth)
	webPort := "5000"
	if p := strings.TrimSpace(os.Getenv("PORT")); p != "" {
		webPort = p
	}
	uiHTTP := &http.Server{
		Addr:              ":" + webPort,
		Handler:           uiServer.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.Log("[WebUI] ✔ Panel de Control escuchando en :%s", webPort)
		if err := uiHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Log("[FATAL] Error en servidor Web UI: %v", err)
		}
	}()

	// Manejo de señales de terminación
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan

	logger.Log("[System] Recibida señal %v. Apagando proxy de forma ordenada...", sig)
	cancel()

	shutdownTimeout := 30 * time.Second
	if envSec := strings.TrimSpace(os.Getenv("SHUTDOWN_TIMEOUT_SEC")); envSec != "" {
		if sec, err := strconv.Atoi(envSec); err == nil && sec > 0 {
			shutdownTimeout = time.Duration(sec) * time.Second
		}
	}

	if !webdavServer.WaitActiveUploads(shutdownTimeout / 2) {
		logger.Log("[WebDAV] ⚠️ Tiempo de espera agotado para subidas en curso; procediendo con cierre.")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	if err := davHTTP.Shutdown(shutdownCtx); err != nil {
		logger.Log("[WebDAV] Error al apagar servidor WebDAV: %v", err)
	}
	if err := uiHTTP.Shutdown(shutdownCtx); err != nil {
		logger.Log("[WebUI] Error al apagar servidor WebUI: %v", err)
	}

	logger.Log("[System] ✔ Servidores cerrados correctamente. Hasta pronto.")
}

func runHealthcheck() int {
	webPort := "5000"
	if p := strings.TrimSpace(os.Getenv("PORT")); p != "" {
		webPort = p
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + webPort + "/health")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}

func computeKeepAliveInterval(expiresInStr string, consecutiveFailures int) time.Duration {
	if consecutiveFailures > 0 {
		backoff := 30 * time.Second * time.Duration(1<<min(consecutiveFailures-1, 4))
		if backoff > 10*time.Minute {
			backoff = 10 * time.Minute
		}
		return backoff
	}
	if sec, err := strconv.Atoi(strings.TrimSpace(expiresInStr)); err == nil && sec > 120 {
		half := time.Duration(sec/2) * time.Second
		if half < 5*time.Minute {
			return half
		}
	}
	return 5 * time.Minute
}

func runKeepAlive(ctx context.Context, client *sapi.Client, isRunning func() bool) {
	consecutiveFailures := 0
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if isRunning != nil && !isRunning() {
				timer.Reset(5 * time.Minute)
				continue
			}
			cfg := client.GetConfig()
			if cfg.RefreshToken == "" && cfg.AccessToken == "" && cfg.ValidationKey == "" {
				timer.Reset(5 * time.Minute)
				continue
			}
			if client.GetWAFCooldownRemaining() > 0 {
				timer.Reset(computeKeepAliveInterval(cfg.ExpiresIn, consecutiveFailures))
				continue
			}

			space, err := client.GetStorageSpace()
			if err == nil && space != nil {
				consecutiveFailures = 0
				logger.Log("[KeepAlive] 💓 Sesión verificada en Movistar Cloud (HTTP 200).")
			} else {
				errStr := ""
				if err != nil {
					errStr = err.Error()
				}
				if strings.Contains(errStr, "401") || strings.Contains(errStr, "sesión expirada") || strings.Contains(errStr, "no autorizada") {
					logger.Log("[KeepAlive] ⚠️ Sesión no autorizada (%v). Intentando renovar...", err)
					if client.RefreshSession() {
						consecutiveFailures = 0
					} else {
						consecutiveFailures++
						logger.Log("[KeepAlive] ❌ Fallo al renovar sesión (intento consecutivo #%d).", consecutiveFailures)
					}
				} else {
					logger.Log("[KeepAlive] ⚠️ Comprobación de cuota omitida por error transitorio o rate limit (%v). La sesión sigue activa.", err)
				}
			}

			nextInterval := computeKeepAliveInterval(client.GetConfig().ExpiresIn, consecutiveFailures)
			timer.Reset(nextInterval)
		}
	}
}
