package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"
	"movistar-webdav-proxy/internal/webdav"
	"movistar-webdav-proxy/internal/webui"
)

const AppVersion = "v2.0.3"

func main() {
	logger.Log("=====================================================")
	logger.Log("🚀 Movistar Cloud WebDAV Proxy %s (Nativo en Go)", AppVersion)
	logger.Log("=====================================================")

	cfg := config.LoadAccountConfig("")
	if cfg.DeviceID != "" {
		logger.Log("[WebDAV] 💻 Modo Autenticación: CLIENTE OFICIAL WINDOWS (%s)", cfg.DeviceID)
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

	// 1. Worker KeepAlive en segundo plano con Context para shutdown limpio
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runKeepAlive(ctx, client)

	// 2. Servidor WebDAV
	webdavServer := webdav.NewServer(client)
	webdavPort := "8080"
	if p := os.Getenv("WEBDAV_PORT"); p != "" {
		webdavPort = p
	}
	davHTTP := &http.Server{
		Addr:    ":" + webdavPort,
		Handler: webdavServer,
	}

	go func() {
		logger.Log("[WebDAV] ✔ Servidor WebDAV escuchando en :%s", webdavPort)
		if err := davHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Log("[FATAL] Error en servidor WebDAV: %v", err)
		}
	}()

	// 3. Servidor Web UI
	uiServer := webui.NewServer(client, webdavServer)
	webPort := "5000"
	if p := os.Getenv("PORT"); p != "" {
		webPort = p
	}
	uiHTTP := &http.Server{
		Addr:    ":" + webPort,
		Handler: uiServer.Routes(),
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

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := davHTTP.Shutdown(shutdownCtx); err != nil {
		logger.Log("[WebDAV] Error al apagar servidor WebDAV: %v", err)
	}
	if err := uiHTTP.Shutdown(shutdownCtx); err != nil {
		logger.Log("[WebUI] Error al apagar servidor WebUI: %v", err)
	}

	logger.Log("[System] ✔ Servidores cerrados correctamente. Hasta pronto.")
}

func runKeepAlive(ctx context.Context, client *sapi.Client) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cfg := client.GetConfig()
			if cfg.RefreshToken == "" && cfg.AccessToken == "" && cfg.ValidationKey == "" {
				continue
			}

			space, err := client.GetStorageSpace()
			if err == nil && space != nil {
				logger.Log("[KeepAlive] 💓 Sesión verificada en Movistar Cloud (HTTP 200).")
			} else {
				logger.Log("[KeepAlive] ⚠️ Sesión requiere actualización. Intentando renovar...")
				_ = client.RefreshSession()
			}
		}
	}
}
