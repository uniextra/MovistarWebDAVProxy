package sapi

import (
	"bytes"
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
	officialUploadAttempts  = 5
	officialUploadBackoff   = func(attempt int) time.Duration { return time.Duration(1+attempt*2) * time.Second }
	officialValidationProbe = 10 * time.Second // la app consulta la validación ~10 s después de subir
	// La app relee el elemento (media?action=get) tras cada subida. Por defecto el
	// proxy no, para no sumar una llamada a la API por cada archivo (Hyper Backup
	// sube miles de archivos pequeños); con SAPI_REFRESH_AFTER_UPLOAD=true lo hace
	// como la app (y de paso obtiene la URL de descarga firmada).
	officialRefreshAfterSave = func() bool {
		switch strings.ToLower(strings.TrimSpace(os.Getenv("SAPI_REFRESH_AFTER_UPLOAD"))) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
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
	existing   string // archivo que se sobrescribe (su ID se conserva)
	pendingID  string // ID de un intento anterior cuyo contenido quedó a medias
	pendingNew bool   // pendingID lo creó ese intento (no es un archivo que ya existía)
	modTime    time.Time
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
	if res.Status == "V" || res.Status == "A" {
		c.probeValidationLater(id, p.folderID)
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
	if officialRefreshAfterSave() {
		if fresh, ferr := c.GetMediaByID(id); ferr == nil && fresh != nil {
			if fresh.URL != "" {
				item.URL = fresh.URL
			}
			if fresh.ETag != "" {
				item.ETag = fresh.ETag
			}
		}
	}
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
		data["id"] = p.existing
		logger.Log("[SAPI] 📝 Sobrescritura in-place para ID existente: %s (%s)", p.existing, p.name)
	} else {
		data["creationdate"] = officialDate(now)
	}
	body, _ := json.Marshal(map[string]interface{}{"data": data})
	u := fmt.Sprintf("%s/upload/%s?action=save-metadata&responsetime=true&lastupdate=true", c.baseURL, kind)

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("POST", u, bytes.NewReader(body))
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
			if attempt > 0 {
				time.Sleep(officialUploadBackoff(attempt - 1))
			}
			got, done, err := c.officialQueryOffset(kind, id, p.size)
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
	req, err := http.NewRequest("POST", u, io.LimitReader(p.reader, remaining))
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
func (c *Client) officialQueryOffset(kind, id string, size int64) (int64, *officialSaveResult, error) {
	u := fmt.Sprintf("%s/upload/%s?action=save&lastupdate=true&acceptasynchronous=true", c.baseURL, kind)
	req, err := http.NewRequest("GET", u, nil)
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

// probeValidationLater imita a la app: unos segundos después de subir un genérico
// (estado "V") consulta una vez su estado de validación.
func (c *Client) probeValidationLater(id string, folderID int64) {
	if officialValidationProbe <= 0 {
		return
	}
	go func() {
		time.Sleep(officialValidationProbe)
		if st, err := c.mediaValidationStatus(id, folderID); err == nil && st != "" && st != "V" && st != "A" {
			logger.Log("[SAPI] ✔ Validación de %s terminada (estado %s)", id, st)
		}
	}()
}
