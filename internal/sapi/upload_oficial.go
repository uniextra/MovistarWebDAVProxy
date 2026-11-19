package sapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"movistar-webdav-proxy/internal/auth"
	"movistar-webdav-proxy/internal/logger"
)

// Subida como el cliente oficial de Windows (O2 Cloud 32.1.3 / motor 4.0.1),
// capturada con Fiddler el 18/11/2026:
//
//  1. POST cloud…/sapi/upload/<tipo>?action=save-metadata&responsetime=true&lastupdate=true
//     content-type: application/octet-stream
//     {"data":{"contenttype","creationdate","folderid","modificationdate","name","size"}}
//     → {"success":"Media metadata saved successfully","id":"…","lastupdate":…}
//  2. POST upload…/sapi/upload/<tipo>?action=save&lastupdate=true&acceptasynchronous=true
//     cuerpo = el archivo en bruto (sin multipart), content-type = tipo del archivo,
//     x-funambol-id: <id del paso 1>, x-funambol-file-size: <tamaño>
//     → {"success":"Media uploaded successfully","id","lastupdate","status","etag",…}
//     status "V" (validando) para los genéricos; la app consulta
//     media?action=get-validation-status ~10 s después.
//
// Si el envío se corta, la app no vuelve a empezar: pregunta cuánto tiene ya el
// servidor y manda solo el resto, sobre el mismo ID.
//
//  3. GET cloud…/sapi/upload/<tipo>?action=save&lastupdate=true&acceptasynchronous=true
//     content-range: bytes */<total>, x-funambol-id, x-funambol-file-size
//     → 308 con la cabecera "Range: 0-<último byte recibido>"
//  4. POST upload…/sapi/upload/<tipo>?action=save&lastupdate=true&acceptasynchronous=true
//     content-range: bytes <inicio>-<total-1>/<total>, Content-Length = lo que falta
//     → 200
//
// El modo reanudado va lento en O2 (~1–2 MB/s frente a ~30–40 MB/s), pero es lo que
// hace la app oficial y lo único observado sobre un ID con contenido a medias.

// UploadInterruptedError indica que el contenido de una subida no llegó entero.
// MediaID es el ID creado (o reutilizado) en save-metadata: un reintento posterior
// debe continuar sobre él (UploadFileContinue) en lugar de crear otro.
type UploadInterruptedError struct {
	MediaID string
	New     bool // el ID lo creó esta subida (no era un archivo que ya existía)
	Err     error
}

func (e *UploadInterruptedError) Error() string {
	return fmt.Sprintf("subida interrumpida (ID %s): %v", e.MediaID, e.Err)
}

func (e *UploadInterruptedError) Unwrap() error { return e.Err }

// officialUploadEnabled indica si se sube como el cliente oficial (perfil desktop
// con sesión OAuth propia). El perfil browser y el modo web siguen subiendo como la web.
func (c *Client) officialUploadEnabled() bool {
	return auth.IsDesktopProfile() && !c.WebMode()
}

var (
	// Reintentos del envío tras un corte: unos 3 minutos en total, para aguantar un
	// corte de red real (la app reintenta en cada pasada, cada 60–70 s, sin límite).
	// Con 5 intentos y esperas de 1–7 s se agotaban en ~16 s y un corte de 30 s
	// tumbaba la subida.
	officialUploadAttempts = 8
	officialUploadBackoff  = func(attempt int) time.Duration {
		steps := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 45 * time.Second, 60 * time.Second}
		if attempt < len(steps) {
			return steps[attempt]
		}
		return steps[len(steps)-1]
	}
	// La app relee el elemento (media?action=get) tras cada subida y el proxy
	// también: así guarda la URL de descarga firmada y la primera descarga no pasa
	// por la URL por defecto, que O2 rechaza con COM-1005. Con
	// SAPI_REFRESH_AFTER_UPLOAD=false se omite esa llamada.
	officialRefreshAfterSave = func() bool {
		switch strings.ToLower(strings.TrimSpace(os.Getenv("SAPI_REFRESH_AFTER_UPLOAD"))) {
		case "0", "false", "no", "off":
			return false
		}
		return true
	}
)

// officialDatesLocal: la app manda las fechas en hora local y sin zona
// ("20261118T173627"), pero O2 las guarda como si fueran UTC: un archivo que la app
// subió con fecha local 17:36:28 aparece en la nube a las 18:36:28 (comprobado el
// 18/11/2026 con corte-subida-har.bin). Imitarlo desplazaría una hora (dos en
// verano) todas las fechas y rclone volvería a subirlo todo, así que el proxy
// sigue mandando UTC con "Z", que O2 guarda tal cual. Es la única diferencia
// deliberada con la app en los datos de las peticiones.
var officialDatesLocal = false

func officialDate(t time.Time) string {
	if officialDatesLocal {
		return t.Local().Format("20060102T150405")
	}
	return t.UTC().Format("20060102T150405Z")
}

type officialUpload struct {
	name       string
	reader     io.Reader
	size       int64
	folderID   int64
	mime       string
	existing   string          // archivo que se sobrescribe (su ID se conserva)
	created    time.Time       // fecha de creación del archivo que se sobrescribe (la app la reenvía)
	pendingID  string          // ID de un intento anterior cuyo contenido quedó a medias
	pendingNew bool            // pendingID lo creó ese intento (no es un archivo que ya existía)
	ctx        context.Context // la petición del cliente: si se va, la subida se aborta
	modTime    time.Time
}

func (p officialUpload) context() context.Context {
	if p.ctx != nil {
		return p.ctx
	}
	return context.Background()
}

// sleepCtx espera d o hasta que ctx se cancele (devuelve false en ese caso).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// UploadFileToFolderCtx es UploadFileToFolder ligada a la petición del cliente: en
// el perfil desktop, si el cliente se va (p. ej. rclone corta la espera), la subida
// a O2 se aborta y el elemento nuevo a medias se retira, en lugar de seguir subiendo
// para nadie y cruzarse con el reintento o el DELETE del cliente.
func (c *Client) UploadFileToFolderCtx(ctx context.Context, fileName string, reader io.Reader, size int64, folderID int64, mimeType string, existingMediaID string, modTime time.Time) (*MediaItem, error) {
	if !c.officialUploadEnabled() {
		return c.UploadFileToFolder(fileName, reader, size, folderID, mimeType, existingMediaID, modTime)
	}
	return c.uploadOfficial(officialUpload{
		name: fileName, reader: reader, size: size, folderID: folderID,
		mime: mimeType, existing: existingMediaID, modTime: modTime, ctx: ctx,
	})
}

// UploadFileReplacingCtx es UploadFileReplacing ligada a la petición del cliente.
func (c *Client) UploadFileReplacingCtx(ctx context.Context, existing MediaItem, fileName string, reader io.Reader, size int64, folderID int64, mimeType string, modTime time.Time) (*MediaItem, error) {
	if !c.officialUploadEnabled() {
		return c.UploadFileToFolder(fileName, reader, size, folderID, mimeType, existing.ID, modTime)
	}
	var created time.Time
	if existing.CreationDate != "" {
		created = existing.CreationDate.Time()
	}
	return c.uploadOfficial(officialUpload{
		name: fileName, reader: reader, size: size, folderID: folderID,
		mime: mimeType, existing: existing.ID, created: created, modTime: modTime, ctx: ctx,
	})
}

// UploadFileContinueCtx es UploadFileContinue ligada a la petición del cliente.
func (c *Client) UploadFileContinueCtx(ctx context.Context, pendingID string, pendingIsNew bool, fileName string, reader io.Reader, size int64, folderID int64, mimeType string, modTime time.Time) (*MediaItem, error) {
	return c.uploadOfficial(officialUpload{
		name: fileName, reader: reader, size: size, folderID: folderID,
		mime: mimeType, pendingID: pendingID, pendingNew: pendingIsNew, modTime: modTime, ctx: ctx,
	})
}

// UploadFileReplacing sobrescribe el archivo existing (mismo ID), reenviando su
// fecha de creación original como hace la app. Fuera del perfil desktop equivale a
// UploadFileToFolder con existing.ID.
func (c *Client) UploadFileReplacing(existing MediaItem, fileName string, reader io.Reader, size int64, folderID int64, mimeType string, modTime time.Time) (*MediaItem, error) {
	if !c.officialUploadEnabled() {
		return c.UploadFileToFolder(fileName, reader, size, folderID, mimeType, existing.ID, modTime)
	}
	var created time.Time
	if existing.CreationDate != "" {
		created = existing.CreationDate.Time()
	}
	return c.uploadOfficial(officialUpload{
		name: fileName, reader: reader, size: size, folderID: folderID,
		mime: mimeType, existing: existing.ID, created: created, modTime: modTime,
	})
}

// UploadFileContinue continúa una subida cuyo contenido quedó a medias sobre el ID
// que ya creó save-metadata (ver UploadInterruptedError). pendingIsNew indica si ese
// ID lo creó la subida interrumpida (y por tanto se puede retirar si al final falla).
func (c *Client) UploadFileContinue(pendingID string, pendingIsNew bool, fileName string, reader io.Reader, size int64, folderID int64, mimeType string, modTime time.Time) (*MediaItem, error) {
	return c.uploadOfficial(officialUpload{
		name: fileName, reader: reader, size: size, folderID: folderID,
		mime: mimeType, pendingID: pendingID, pendingNew: pendingIsNew, modTime: modTime,
	})
}

// DiscardInterruptedUpload retira a la papelera el elemento que creó una subida que
// no llegó a completarse: si se quedara, la nube mostraría un archivo con el tamaño
// completo pero el contenido a medias (y un reintento crearía otro con el mismo nombre).
func (c *Client) DiscardInterruptedUpload(err error, name string) {
	var ie *UploadInterruptedError
	if !errors.As(err, &ie) || !ie.New || ie.MediaID == "" {
		return
	}
	if derr := c.DeleteFile(ie.MediaID, name); derr != nil {
		logger.Log("[SAPI] ⚠️ No se pudo retirar el elemento a medias de '%s' (ID %s): %v", name, ie.MediaID, derr)
		return
	}
	logger.Log("[SAPI] 🧹 Retirado el elemento a medias de '%s' (ID %s) tras fallar su subida", name, ie.MediaID)
}

func (c *Client) uploadOfficial(p officialUpload) (*MediaItem, error) {
	kind := mediaKindForName(p.name)
	// El tipo como lo manda la app (sin "; charset=…" y con sus valores para m4a, heic…).
	contentType := contentTypeForName(p.name)
	now := time.Now()
	modTime := now
	if !p.modTime.IsZero() {
		modTime = p.modTime
	}

	id := p.pendingID
	isNew := p.existing == "" && (p.pendingID == "" || p.pendingNew)
	if id == "" {
		var err error
		id, err = c.officialSaveMetadata(kind, p, contentType, now, modTime)
		if err != nil {
			return nil, err
		}
	} else {
		logger.Log("[SAPI] 🔁 Continuando la subida de '%s' sobre el ID %s", p.name, id)
	}

	res, err := c.officialSendContent(kind, id, p, contentType)
	if err != nil {
		var ie *UploadInterruptedError
		if errors.As(err, &ie) {
			ie.New = isNew
			// Con un lector que se puede rebobinar ya no hay otro intento después: si el
			// elemento es nuevo, se retira para no dejar un archivo a medias en la nube.
			if _, seekable := p.reader.(io.ReadSeeker); seekable {
				c.DiscardInterruptedUpload(ie, p.name)
			}
			return nil, ie
		}
		return nil, err
	}

	if res.ID != "" {
		id = res.ID
	}

	item := &MediaItem{
		ID:               id,
		Name:             p.name,
		Size:             p.size,
		FolderID:         p.folderID,
		CreationDate:     FlexDate(now.UTC().Format("20060102T150405Z")),
		ModificationDate: FlexDate(modTime.UTC().Format("20060102T150405Z")),
		ETag:             res.Etag,
	}
	c.afterOfficialSave(id, p.folderID, res.Status, res.Etag)
	return item, nil
}

// officialSaveMetadata crea el elemento (o actualiza el que se sobrescribe) y devuelve su ID.
func (c *Client) officialSaveMetadata(kind string, p officialUpload, contentType string, now, modTime time.Time) (string, error) {
	data := map[string]interface{}{
		"contenttype":      contentType,
		"folderid":         p.folderID,
		"modificationdate": officialDate(modTime),
		"name":             p.name,
		"size":             p.size,
	}
	if p.existing != "" {
		// Como la app (captura del 18/11/2026): id, tamaño y fecha de modificación
		// nuevos, y la fecha de creación original del archivo.
		data["id"] = p.existing
		if !p.created.IsZero() {
			data["creationdate"] = officialDate(p.created)
		}
		logger.Log("[SAPI] 📝 Sobrescritura in-place para ID existente: %s (%s)", p.existing, p.name)
	} else {
		data["creationdate"] = officialDate(now)
	}
	body, _ := json.Marshal(map[string]interface{}{"data": data})
	u := fmt.Sprintf("%s/upload/%s?action=save-metadata&responsetime=true&lastupdate=true", c.baseURL, kind)

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(p.context(), "POST", u, bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := c.DoRequest(req)
		if err != nil {
			return "", err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()

		var res struct {
			ID    interface{} `json:"id"`
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &res)
		code := ""
		if res.Error != nil {
			code = res.Error.Code
		}
		switch {
		case resp.StatusCode == http.StatusRequestEntityTooLarge || code == "MED-1020":
			return "", ErrFileTooLarge
		case code == "MED-1017" && p.existing != "" && attempt < mediaMoveRetries:
			// El archivo que se sobrescribe aún se está validando (subido hace segundos):
			// se espera a que termine, como la app, en lugar de reintentar a ciegas.
			if c.waitMediaValidatedIn(p.existing, p.folderID) {
				logger.Log("[SAPI] ⏳ '%s' estaba en validación (MED-1017); validado, reintento la sobrescritura (%d/%d).", p.name, attempt+1, mediaMoveRetries)
			} else {
				delay := mediaMoveDelay(attempt)
				logger.Log("[SAPI] ⏳ '%s' aún en validación (MED-1017) al sobrescribirlo. Reintentando en %s (%d/%d)...", p.name, delay, attempt+1, mediaMoveRetries)
				time.Sleep(delay)
			}
			continue
		case code == "MED-1017":
			return "", fmt.Errorf("%w: %s", ErrMediaValidating, summarizeHTTPErrorBody(b))
		case res.Error != nil:
			return "", fmt.Errorf("save-metadata (%s) API error: %s (%s)", kind, res.Error.Message, code)
		case resp.StatusCode < 200 || resp.StatusCode >= 300:
			return "", fmt.Errorf("save-metadata (%s) failed (%d): %s", kind, resp.StatusCode, summarizeHTTPErrorBody(b))
		}
		id := parseFlexibleID(res.ID)
		if id == "" {
			if p.existing != "" {
				return p.existing, nil
			}
			return "", fmt.Errorf("save-metadata (%s) sin ID: %s", kind, summarizeHTTPErrorBody(b))
		}
		return id, nil
	}
}

// ErrMediaValidating (MED-1017): O2 aún valida el archivo y no deja operar sobre él.
var ErrMediaValidating = errors.New("SAPI MED-1017: media not yet validated")

type officialSaveResult struct {
	ID     string
	Status string
	Etag   string
}

// officialSendContent envía el contenido y, si se corta, continúa donde lo dejó el
// servidor. Si el lector no se puede rebobinar (subida en flujo), un corte se
// devuelve como UploadInterruptedError para continuar desde el disco.
func (c *Client) officialSendContent(kind, id string, p officialUpload, contentType string) (*officialSaveResult, error) {
	seeker, canSeek := p.reader.(io.ReadSeeker)
	var lastErr error
	for attempt := 0; attempt < officialUploadAttempts; attempt++ {
		offset := int64(0)
		if attempt > 0 || p.pendingID != "" {
			if attempt > 0 && !sleepCtx(p.context(), officialUploadBackoff(attempt-1)) {
				return nil, &UploadInterruptedError{MediaID: id, Err: fmt.Errorf("el cliente abandonó la subida: %w", p.context().Err())}
			}
			got, done, err := c.officialQueryOffset(p.context(), kind, id, p.size)
			switch {
			case err != nil:
				logger.Log("[SAPI] ⚠️ No se pudo consultar cuánto tiene O2 de '%s' (ID %s): %v", p.name, id, err)
			case done != nil:
				logger.Log("[SAPI] ✔ O2 ya tenía completo '%s' (ID %s)", p.name, id)
				return done, nil
			case got > 0:
				offset = got
				logger.Log("[SAPI] 🔁 Reanudando '%s' desde el byte %d de %d (ID %s)", p.name, offset, p.size, id)
			}
		}
		if offset > 0 || attempt > 0 {
			if !canSeek {
				return nil, &UploadInterruptedError{MediaID: id, Err: lastErr}
			}
		}
		if canSeek {
			if _, err := seeker.Seek(offset, io.SeekStart); err != nil {
				return nil, &UploadInterruptedError{MediaID: id, Err: err}
			}
		}

		res, retry, err := c.officialPostContent(kind, id, p, contentType, offset)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if p.context().Err() != nil {
			// El cliente se fue: no se sigue subiendo para nadie.
			logger.Log("[SAPI] ✋ El cliente abandonó la subida de '%s'; se aborta.", p.name)
			return nil, &UploadInterruptedError{MediaID: id, Err: fmt.Errorf("el cliente abandonó la subida: %w", p.context().Err())}
		}
		if !retry {
			return nil, &UploadInterruptedError{MediaID: id, Err: err}
		}
		if !canSeek {
			return nil, &UploadInterruptedError{MediaID: id, Err: err}
		}
		logger.Log("[SAPI] ⚠️ Envío de '%s' cortado (%v); intento %d/%d", p.name, err, attempt+1, officialUploadAttempts)
	}
	return nil, &UploadInterruptedError{MediaID: id, Err: lastErr}
}

// officialPostContent hace un envío (completo o el resto desde offset). retry indica
// si el fallo es de los que se arreglan reintentando.
func (c *Client) officialPostContent(kind, id string, p officialUpload, contentType string, offset int64) (*officialSaveResult, bool, error) {
	c.mu.RLock()
	u := fmt.Sprintf("%s/sapi/upload/%s?action=save&lastupdate=true&acceptasynchronous=true", c.uploadURL, kind)
	c.mu.RUnlock()

	remaining := p.size - offset
	req, err := http.NewRequestWithContext(p.context(), "POST", u, io.LimitReader(p.reader, remaining))
	if err != nil {
		return nil, false, err
	}
	req.ContentLength = remaining
	if remaining == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set("Content-Type", contentType)
	if offset > 0 {
		req.Header["content-range"] = []string{fmt.Sprintf("bytes %d-%d/%d", offset, p.size-1, p.size)}
	}
	req.Header["x-funambol-file-size"] = []string{strconv.FormatInt(p.size, 10)}
	req.Header["x-funambol-id"] = []string{id}

	resp, err := c.DoRequest(req)
	if err != nil {
		if errors.Is(err, ErrWAFBlocked) {
			return nil, false, err
		}
		return nil, true, err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	var res struct {
		ID     interface{} `json:"id"`
		Status string      `json:"status"`
		Etag   string      `json:"etag"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &res)
	code := ""
	if res.Error != nil {
		code = res.Error.Code
	}

	switch {
	case resp.StatusCode == http.StatusRequestEntityTooLarge || code == "MED-1020":
		return nil, false, ErrFileTooLarge
	case code == "MED-1017":
		// Sobrescritura de un archivo que aún se valida: se espera y se reintenta.
		c.waitMediaValidatedIn(id, p.folderID)
		return nil, true, fmt.Errorf("%w: %s", ErrMediaValidating, summarizeHTTPErrorBody(b))
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusPermanentRedirect,
		resp.StatusCode >= 500,
		code == "MED-1048", code == "PAPI-0000":
		return nil, true, fmt.Errorf("upload content failed (%d): %s", resp.StatusCode, summarizeHTTPErrorBody(b))
	case res.Error != nil:
		return nil, false, fmt.Errorf("upload content API error: %s (%s)", res.Error.Message, code)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, false, fmt.Errorf("upload content failed (%d): %s", resp.StatusCode, summarizeHTTPErrorBody(b))
	}
	got := parseFlexibleID(res.ID)
	if got != "" && got != id {
		logger.Log("[SAPI] ⚠️ O2 respondió a la subida de '%s' con el ID %s en lugar de %s", p.name, got, id)
	}
	return &officialSaveResult{ID: got, Status: res.Status, Etag: res.Etag}, false, nil
}

// officialQueryOffset pregunta cuántos bytes tiene ya O2 de una subida (308 + Range).
// Si el servidor responde que ya está completa, done no es nil.
func (c *Client) officialQueryOffset(ctx context.Context, kind, id string, size int64) (int64, *officialSaveResult, error) {
	u := fmt.Sprintf("%s/upload/%s?action=save&lastupdate=true&acceptasynchronous=true", c.baseURL, kind)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header["content-range"] = []string{fmt.Sprintf("bytes */%d", size)}
	req.Header["x-funambol-file-size"] = []string{strconv.FormatInt(size, 10)}
	req.Header["x-funambol-id"] = []string{id}

	resp, err := c.DoRequest(req)
	if err != nil {
		return 0, nil, err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPermanentRedirect:
		return parseReceivedRange(resp.Header.Get("Range"), size), nil, nil
	case http.StatusOK, http.StatusCreated:
		var res struct {
			Status string `json:"status"`
			Etag   string `json:"etag"`
			Error  *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &res) == nil && res.Error == nil && (res.Etag != "" || res.Status != "") {
			return size, &officialSaveResult{Status: res.Status, Etag: res.Etag}, nil
		}
		return 0, nil, nil
	}
	return 0, nil, fmt.Errorf("consulta de offset (%d): %s", resp.StatusCode, summarizeHTTPErrorBody(b))
}

// parseReceivedRange interpreta "Range: 0-N" (último byte recibido) y devuelve
// cuántos bytes tiene el servidor. Sin cabecera o con un valor raro, 0.
func parseReceivedRange(v string, size int64) int64 {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "bytes="))
	if v == "" {
		return 0
	}
	i := strings.LastIndex(v, "-")
	if i < 0 {
		return 0
	}
	last, err := strconv.ParseInt(strings.TrimSpace(v[i+1:]), 10, 64)
	if err != nil || last < 0 {
		return 0
	}
	got := last + 1
	if got > size {
		return 0
	}
	return got
}

// MediaRefresh es lo que devuelve la relectura de un archivo tras subirlo.
type MediaRefresh struct {
	ID         string
	URL        string // URL de descarga firmada
	ETag       string
	UploadETag string // etag de la subida que provocó la relectura
}

// SetMediaRefreshHook registra quién guarda el resultado de la relectura tras subir
// (la capa WebDAV: caché y base de metadatos).
func (c *Client) SetMediaRefreshHook(fn func(MediaRefresh)) {
	c.hookMu.Lock()
	c.onMediaRefreshed = fn
	c.hookMu.Unlock()
}

func (c *Client) mediaRefreshHook() func(MediaRefresh) {
	c.hookMu.Lock()
	defer c.hookMu.Unlock()
	return c.onMediaRefreshed
}

// La app consulta la validación unos 10–15 s después de subir. Si el archivo sigue
// en validación, las consultas se espacian hasta una por minuto (un archivo grande
// puede tardar varios minutos). Se cambian con WithValidationPolling.
const (
	defaultValidationFirst = 10 * time.Second
	defaultValidationMax   = time.Minute
)

// afterOfficialSave hace lo mismo que la app tras subir (captura del 18/11/2026): si
// el archivo queda en validación (estado V o A), consulta su estado unos segundos
// después (get-validation-status) hasta que termina, y entonces relee el elemento
// (media?action=get) para quedarse con su URL de descarga firmada. Si no queda en
// validación, lo relee enseguida. Va en segundo plano: el cliente ya tiene su
// respuesta. Con SAPI_REFRESH_AFTER_UPLOAD=false solo se consulta la validación.
//
// No hay límite de tiempo: se sigue consultando (cada 10 s al principio, luego cada
// vez más espaciado, hasta una vez por minuto) hasta que O2 termina de validar o deja
// de conocer el archivo (borrado o sustituido). Un fallo pasajero de la consulta no
// corta el seguimiento. Mientras dura, WaitUploadSettled permite a una descarga
// esperar a que el archivo se pueda leer.
func (c *Client) afterOfficialSave(id string, folderID int64, status, uploadETag string) {
	c.followNewMedia(id, folderID, status == "V" || status == "A", c.validationFirst, uploadETag)
}

// FollowCopiedMedia sigue un archivo que O2 acaba de crear al copiar en la nube, igual
// que una subida: consulta su validación (enseguida: una copia no pasa por el envío
// del contenido) y lo relee para guardar su URL de descarga firmada. Sin esto, su
// primera descarga iba a la URL por defecto (COM-1005) y necesitaba otra consulta.
// Mientras dura, WaitUploadSettled permite a una descarga esperar. Solo en el perfil
// desktop, donde el proxy relee como el cliente oficial.
func (c *Client) FollowCopiedMedia(id string, folderID int64) {
	if id == "" || !c.officialUploadEnabled() {
		return
	}
	c.followNewMedia(id, folderID, true, 0, "")
}

// followNewMedia: seguimiento de un archivo nuevo (ver afterOfficialSave). firstWait
// es la espera antes de la primera consulta de validación.
func (c *Client) followNewMedia(id string, folderID int64, validating bool, firstWait time.Duration, uploadETag string) {
	refresh := officialRefreshAfterSave()
	if !refresh && !validating {
		return
	}
	wait, maxWait := firstWait, c.validationMax
	if maxWait < c.validationFirst {
		maxWait = c.validationFirst
	}
	done := c.markSettling(id)
	go func() {
		defer c.unmarkSettling(id, done)
		if validating {
			logged := false
			for {
				time.Sleep(wait)
				st, err := c.mediaValidationStatus(id, folderID)
				if errors.Is(err, errValidationNoData) {
					return
				}
				if err == nil && st != "V" && st != "A" {
					if st != "" {
						logger.Log("[SAPI] ✔ Validación de %s terminada (estado %s)", id, st)
					}
					break
				}
				if err == nil && !logged && wait >= maxWait {
					logged = true
					logger.Log("[SAPI] ⏳ %s sigue en validación en O2; se consulta cada %s", id, maxWait)
				}
				switch {
				case wait < c.validationFirst:
					wait = c.validationFirst // tras una primera consulta inmediata, la cadencia normal
				case wait*3/2 > maxWait:
					wait = maxWait
				default:
					wait = wait * 3 / 2
				}
			}
		}
		if !refresh {
			return
		}
		fresh, err := c.officialGetMedia(id)
		if err != nil || fresh == nil {
			return
		}
		if h := c.mediaRefreshHook(); h != nil {
			h(MediaRefresh{ID: id, URL: fresh.URL, ETag: fresh.ETag, UploadETag: uploadETag})
		}
	}()
}

func (c *Client) markSettling(id string) chan struct{} {
	ch := make(chan struct{})
	c.hookMu.Lock()
	if c.settling == nil {
		c.settling = map[string]chan struct{}{}
	}
	c.settling[id] = ch
	c.hookMu.Unlock()
	return ch
}

func (c *Client) unmarkSettling(id string, ch chan struct{}) {
	c.hookMu.Lock()
	if c.settling[id] == ch {
		delete(c.settling, id)
	}
	c.hookMu.Unlock()
	close(ch)
}

// UploadSettling indica si el archivo se acaba de subir y O2 aún lo está validando
// (o el proxy aún no lo ha releído).
func (c *Client) UploadSettling(id string) bool {
	c.hookMu.Lock()
	defer c.hookMu.Unlock()
	_, ok := c.settling[id]
	return ok
}

// WaitUploadSettled espera, mientras ctx siga vivo (el cliente conectado), a que
// termine la validación y la relectura de un archivo recién subido. Devuelve true si
// había algo que esperar y terminó; false si no había nada pendiente o ctx terminó.
func (c *Client) WaitUploadSettled(ctx context.Context, id string) bool {
	c.hookMu.Lock()
	ch, ok := c.settling[id]
	c.hookMu.Unlock()
	if !ok {
		return false
	}
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

// officialGetMedia relee un archivo como la app: media?action=get sin «origin», con
// sus mismos campos y el ID como número.
func (c *Client) officialGetMedia(id string) (*MediaItem, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return c.GetMediaByID(id)
	}
	c.mu.RLock()
	u := fmt.Sprintf("%s/media?action=get", c.baseURL)
	c.mu.RUnlock()
	body, _ := json.Marshal(map[string]interface{}{"data": map[string]interface{}{
		"fields": []string{"name", "url", "creationdate", "modificationdate", "size", "etag", "folderid"},
		"ids":    []int64{n},
	}})
	req, err := http.NewRequest("POST", u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.DoRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relectura de %s (HTTP %d): %s", id, resp.StatusCode, summarizeHTTPErrorBody(b))
	}
	var res struct {
		Data struct {
			Media []MediaItem `json:"media"`
		} `json:"data"`
		Error interface{} `json:"error"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, fmt.Errorf("relectura de %s: %w", id, err)
	}
	if res.Error != nil {
		return nil, fmt.Errorf("relectura de %s: %v", id, res.Error)
	}
	for i := range res.Data.Media {
		if res.Data.Media[i].ID == id {
			return &res.Data.Media[i], nil
		}
	}
	return nil, nil
}
