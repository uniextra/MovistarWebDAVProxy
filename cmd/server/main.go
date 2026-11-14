package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"movistar-webdav-proxy/internal/auth"
	"movistar-webdav-proxy/internal/cache"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"
	"movistar-webdav-proxy/internal/webclient"
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
		logger.Log("[Security] 🔒 Autenticación habilitada para WebDAV (Basic Auth: %s) y Panel Web (Formulario Web: %s)", logger.Mask(davAuth.Username), logger.Mask(uiAuth.Username))
	}

	cfg := config.LoadAccountConfig("")
	if config.WebSessionEnabled() {
		// El modo de autenticación se anuncia al iniciar la sesión web.
	} else if cfg.DeviceID != "" {
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

	// Modo web: Movistar Cloud se usa como la web oficial, con un Chromium sin
	// pantalla que guarda la sesión, en lugar de los tokens del cliente de escritorio.
	var webSess *webclient.Session
	if config.WebSessionEnabled() {
		webSess, err = webclient.NewSession(webclient.Options{
			ProfileDir: filepath.Join(config.TokensDir, "webprofile"),
			ChromePath: strings.TrimSpace(os.Getenv("CHROME_PATH")),
			NoSandbox:  strings.EqualFold(strings.TrimSpace(os.Getenv("CHROME_NO_SANDBOX")), "true"),
		})
		if err != nil {
			logger.Log("[FATAL] Error iniciando la sesión web (Chromium): %v", err)
			os.Exit(1)
		}
		defer webSess.Close()
		// Descargas firmadas y subidas directas con la huella TLS de Chrome (uTLS), a juego
		// con el User-Agent del Chromium de la sesión.
		client.EnableWebSession(webSess, &webclient.Transport{Session: webSess, Base: auth.NewFingerprintedTransport()})
		logger.Log("[WebDAV] 🌐 Modo Autenticación: SESIÓN WEB (navegador)")
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 45*time.Second)
		if !webSess.CheckLogin(checkCtx) {
			logger.Log("[!] ⚠️ Sesión web no iniciada. Abre el panel en /web-login/ para iniciar sesión en Movistar Cloud.")
		}
		checkCancel()
	}

	var metaStore *cache.MetadataStore
	var dbPath string
	if phoneClean := config.SanitizePhone(cfg.Phone); phoneClean != "" {
		dbPath = filepath.Join(config.TokensDir, fmt.Sprintf("metadata_%s.db", phoneClean))
	} else if webSess != nil {
		dbPath = filepath.Join(config.TokensDir, "metadata_web.db")
	} else if cfg.RefreshToken != "" || cfg.DeviceID != "" || cfg.JSessionID != "" {
		dbPath = filepath.Join(config.TokensDir, "metadata.db")
	}
	if dbPath != "" {
		var msErr error
		metaStore, msErr = cache.NewMetadataStore(dbPath)
		if msErr != nil {
			logger.Log("[WARN] Error inicializando SQLite MetadataStore en %s: %v", maskDBPath(dbPath), msErr)
		} else {
			logger.Log("[Metadata] 🗄️ SQLite MetadataStore inicializado en %s", maskDBPath(dbPath))
			defer metaStore.Close()
		}
	}

	maxDown, maxUp := config.TransferLimits(client.WebMode())
	webdav.SetTransferLimits(maxDown, maxUp)
	if client.WebMode() && strings.TrimSpace(os.Getenv("SAPI_MAX_CONCURRENT")) == "" {
		// En modo web las llamadas a la API ya las limita el navegador; aquí solo hay
		// que dejar sitio a cada transferencia para que una subida no frene las lecturas.
		client.SetMaxConcurrentRequests(maxDown + maxUp + 2)
	}
	logger.Log("[WebDAV] 🚦 Transferencias simultáneas: %d descargas, %d subidas", maxDown, maxUp)

	// 1. Servidor WebDAV (sin ReadTimeout/WriteTimeout globales para no cortar transferencias grandes)
	webdavServer := webdav.NewServerWithAuth(client, davAuth, metaStore)

	syncInterval := config.GetSyncInterval()
	logger.Log("[Metadata] ⏱️ Intervalo de sincronización periódica: %v (SYNC_INTERVAL_SEC)", syncInterval)
	if fs := webdavServer.FS(); fs != nil {
		fs.SetSyncInterval(syncInterval)
	}

	// 2. Sincronización inicial de metadatos en segundo plano si hay credenciales activas
	if metaStore != nil && !client.IsSessionExpired() && (client.WebMode() || cfg.RefreshToken != "" || cfg.DeviceID != "" || cfg.JSessionID != "") {
		go func() {
			logger.Log("[Metadata] 🔄 Iniciando sincronización de metadatos en segundo plano...")
			if _, syncErr := metaStore.SyncChanges(client, false); syncErr != nil {
				logger.Log("[Metadata] ⚠️ Error en sincronización inicial de metadatos: %v", syncErr)
			} else {
				logger.Log("[Metadata] ✔ Sincronización inicial de metadatos completada.")
				if fs := webdavServer.FS(); fs != nil {
					fs.RecordSyncTime(time.Now())
				}
			}
		}()
	}

	// 3. Workers KeepAlive y DeltaSync en segundo plano con Context para shutdown limpio
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runKeepAlive(ctx, client, webdavServer.IsRunning)
	go runDeltaSync(ctx, client, webdavServer.IsRunning, webdavServer.MetadataStore, webdavServer.FS, syncInterval)

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
	if webSess != nil {
		view := webSess.LoginView()
		uiServer.SetWebLogin(view.Handler("/web-login/"))
		acct := &accountState{}
		go runWebHousekeeping(ctx, client, webSess.LoggedIn, acct)
		uiServer.SetWebSessionStatus(func() map[string]interface{} { return acct.status(webSess.Status()) })
	}
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
			if client.WebMode() {
				// En modo web la comprobación también mantiene viva la sesión de la web.
				if client.RefreshSession() {
					logger.Log("[KeepAlive] 💓 Sesión web verificada en Movistar Cloud.")
				}
				timer.Reset(5 * time.Minute)
				continue
			}
			if cfg.RefreshToken == "" && cfg.AccessToken == "" && cfg.ValidationKey == "" {
				timer.Reset(5 * time.Minute)
				continue
			}
			if client.IsSessionExpired() {
				timer.Reset(5 * time.Minute)
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
				if client.IsSessionExpired() {
					consecutiveFailures = 0
					logger.Log("[KeepAlive] ⏸️ Sesión expirada pendiente de revinculación en el panel web. Pausando comprobaciones periódicas.")
				} else if strings.Contains(errStr, "401") || strings.Contains(errStr, "sesión expirada") || strings.Contains(errStr, "no autorizada") {
					// GetStorageSpace -> DoRequest ya ejecutó RefreshSession() internamente al recibir 401.
					consecutiveFailures++
					logger.Log("[KeepAlive] ❌ Fallo al verificar/renovar sesión (intento consecutivo #%d): %v", consecutiveFailures, err)
				} else {
					logger.Log("[KeepAlive] ⚠️ Comprobación de cuota omitida por error transitorio o rate limit (%v). La sesión sigue activa.", err)
				}
			}

			nextInterval := computeKeepAliveInterval(client.GetConfig().ExpiresIn, consecutiveFailures)
			timer.Reset(nextInterval)
		}
	}
}

func runDeltaSync(ctx context.Context, client *sapi.Client, isRunning func() bool, getMetaStore func() *cache.MetadataStore, getFS func() *webdav.MovistarFS, interval time.Duration) {
	timer := time.NewTimer(interval)
	defer timer.Stop()

	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if isRunning != nil && !isRunning() {
				timer.Reset(interval)
				continue
			}
			if client == nil || client.IsSessionExpired() {
				timer.Reset(interval)
				continue
			}
			cfg := client.GetConfig()
			if cfg.RefreshToken == "" && cfg.AccessToken == "" && cfg.ValidationKey == "" {
				timer.Reset(interval)
				continue
			}

			nextInterval := interval
			if getMetaStore != nil {
				if ms := getMetaStore(); ms != nil {
					changed, err := ms.SyncChanges(client, false)
					if err != nil {
						consecutiveFailures++
						backoffFactor := 1 << consecutiveFailures
						if backoffFactor > 15 {
							backoffFactor = 15
						}
						nextInterval = interval * time.Duration(backoffFactor)
						logger.Log("[DeltaSync] ⚠️ Sincronización periódica de metadatos falló: %v. Reintentando en %v (%d fallos consecutivos)", err, nextInterval, consecutiveFailures)
					} else {
						consecutiveFailures = 0
						if getFS != nil {
							if fs := getFS(); fs != nil {
								fs.RecordSyncTime(time.Now())
								if changed > 0 {
									fs.InvalidateCache(0)
								}
							}
						}
						if changed > 0 {
							logger.Log("[DeltaSync] 🔄 Sincronizados %d cambios en segundo plano.", changed)
						}
					}
				}
			}
			timer.Reset(nextInterval)
		}
	}
}

func maskDBPath(p string) string {
	base := filepath.Base(p)
	dir := filepath.Dir(p)
	if strings.HasPrefix(base, "metadata_") && strings.HasSuffix(base, ".db") {
		phone := strings.TrimSuffix(strings.TrimPrefix(base, "metadata_"), ".db")
		return filepath.Join(dir, fmt.Sprintf("metadata_%s.db", logger.Mask(phone)))
	}
	return p
}
