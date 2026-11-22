package main

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"movistar-webdav-proxy/internal/logger"
	"movistar-webdav-proxy/internal/sapi"
)

// accountState es lo que el modo web muestra en el panel sobre la cuenta: sus
// opciones (de /system/information) y el último vaciado automático de la papelera.
type accountState struct {
	mu             sync.RWMutex
	settings       map[string]interface{}
	trashEvery     time.Duration
	trashEmptiedAt time.Time
}

// trashEmptyInterval es cada cuánto se vacía la papelera (WEB_TRASH_EMPTY_EVERY,
// p. ej. "6h"; "0" lo desactiva). Por defecto, cada 6 h: lo que borra el proxy son
// sobre todo trozos temporales, y la papelera cuenta para el límite de carpetas.
func trashEmptyInterval() time.Duration {
	v := strings.TrimSpace(os.Getenv("WEB_TRASH_EMPTY_EVERY"))
	if v == "" {
		return 6 * time.Hour
	}
	if v == "0" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute {
		return 6 * time.Hour
	}
	return d
}

func (a *accountState) status(base map[string]interface{}) map[string]interface{} {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.settings != nil {
		base["account"] = a.settings
	}
	base["trash_empty_every"] = a.trashEvery.String()
	if !a.trashEmptiedAt.IsZero() {
		base["trash_emptied_at"] = a.trashEmptiedAt.Format(time.RFC3339)
	}
	return base
}

// runWebHousekeeping lee las opciones de la cuenta (cada 6 h) y vacía la papelera
// cada trashEvery, solo con la sesión web iniciada.
func runWebHousekeeping(ctx context.Context, client *sapi.Client, loggedIn func() bool, st *accountState) {
	var lastInfo time.Time
	st.mu.Lock()
	st.trashEvery = trashEmptyInterval()
	every := st.trashEvery
	st.mu.Unlock()
	nextTrash := time.Now().Add(every)

	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !loggedIn() {
			continue
		}
		if time.Since(lastInfo) > 6*time.Hour {
			if info, err := client.GetSystemInfo(); err == nil {
				settings := sapi.AccountSettings(info)
				st.mu.Lock()
				st.settings = settings
				st.mu.Unlock()
				lastInfo = time.Now()
				logger.Log("[Cuenta] Opciones de Movistar Cloud: %v", settings)
			}
		}
		if every > 0 && time.Now().After(nextTrash) {
			nextTrash = time.Now().Add(every)
			if err := client.EmptyTrash(); err != nil {
				logger.Log("[Cuenta] ❌ No se pudo vaciar la papelera: %v", err)
				continue
			}
			st.mu.Lock()
			st.trashEmptiedAt = time.Now()
			st.mu.Unlock()
			logger.Log("[Cuenta] 🗑 Papelera vaciada (automático, cada %s).", every)
		}
	}
}
