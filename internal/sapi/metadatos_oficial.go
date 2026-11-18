package sapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"movistar-webdav-proxy/internal/logger"
)

// Operaciones de metadatos como el cliente oficial de Windows (perfil desktop),
// capturadas con Fiddler el 18/11/2026 (08:12–08:54 y 13:15):
//
//   - Renombrar o mover un archivo (o las dos cosas a la vez): una sola llamada
//     upload/<tipo>?action=save-metadata con
//     {"data":{"contenttype","folderid","id","modificationdate","name"}}.
//   - Crear carpeta: media/folder?action=save, content-type application/json, con
//     {"data":{"creationdate","magic":false,"modificationdate","name","parentid"}}.
//     Renombrar o mover carpeta: la misma con "id".
//   - Borrar archivo: media?action=softdelete&id=<id>; carpeta:
//     media/folder?action=softdelete&id=<id>. Cuerpo vacío,
//     application/x-www-form-urlencoded; charset=UTF-8.

// officialClient indica si se habla como el cliente oficial de escritorio.
func (c *Client) officialClient() bool {
	return c.officialUploadEnabled()
}

func contentTypeForName(name string) string {
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// UpdateMediaItem renombra y/o mueve un archivo. En el perfil desktop manda lo
// mismo que la app (con contenttype y modificationdate del archivo); en el resto,
// lo de siempre (UpdateMedia).
func (c *Client) UpdateMediaItem(item MediaItem, newName string, folderID int64) error {
	if !c.officialClient() {
		return c.UpdateMedia(item.ID, item.Name, newName, folderID)
	}
	mod := time.Now()
	if item.ModificationDate != "" {
		mod = item.ModificationDate.Time()
	}
	data := map[string]interface{}{
		"contenttype":      contentTypeForName(newName),
		"folderid":         folderID,
		"id":               item.ID,
		"modificationdate": officialDate(mod),
		"name":             newName,
	}
	return c.updateMediaKinds(item.ID, item.Name, newName, func(kind string) error {
		return c.officialSaveMediaMetadata(kind, data)
	})
}

func (c *Client) officialSaveMediaMetadata(kind string, data map[string]interface{}) error {
	body, _ := json.Marshal(map[string]interface{}{"data": data})
	u := fmt.Sprintf("%s/upload/%s?action=save-metadata&responsetime=true&lastupdate=true", c.baseURL, kind)
	req, err := http.NewRequest("POST", u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.DoRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("update media (%s) failed (%d): %s", kind, resp.StatusCode, summarizeHTTPErrorBody(b))
	}
	var res struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &res); err == nil && res.Error != nil {
		return fmt.Errorf("update media (%s) API error: %s (%s)", kind, res.Error.Message, res.Error.Code)
	}
	return nil
}

// officialFolderSave crea (sin id) o renombra/mueve (con id) una carpeta como la app.
func (c *Client) officialFolderSave(data map[string]interface{}) ([]byte, int, error) {
	body, _ := json.Marshal(map[string]interface{}{"data": data})
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/media/folder?action=save", c.baseURL), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.DoRequest(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, nil
}

func officialFolderCreateData(name string, parentID int64, now time.Time) map[string]interface{} {
	return map[string]interface{}{
		"creationdate":     officialDate(now),
		"magic":            false,
		"modificationdate": officialDate(now),
		"name":             name,
		"parentid":         parentID,
	}
}

// UpdateFolderItem renombra y/o mueve una carpeta. En el perfil desktop manda lo
// mismo que la app (con creationdate, magic y modificationdate).
func (c *Client) UpdateFolderItem(folder FolderItem, newName string, parentID int64) error {
	if !c.officialClient() {
		return c.UpdateFolder(folder.ID, newName, parentID)
	}
	created := time.Now()
	if folder.CreationDate != "" {
		created = folder.CreationDate.Time()
	}
	if parentID == 0 {
		parentID = folder.ParentID
	}
	data := map[string]interface{}{
		"creationdate":     officialDate(created),
		"id":               folder.ID,
		"magic":            folder.Magic,
		"modificationdate": officialDate(time.Now()),
		"name":             newName,
		"parentid":         parentID,
	}
	b, status, err := c.officialFolderSave(data)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("update folder failed (%d): %s", status, summarizeHTTPErrorBody(b))
	}
	var res struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &res); err == nil && res.Error != nil {
		if res.Error.Code == "FOL-1036" {
			return ErrFolderUpdatesLimit
		}
		return fmt.Errorf("update folder API error: %s (%s)", res.Error.Message, res.Error.Code)
	}
	return nil
}

// officialSoftDelete manda a la papelera como la app: id en la URL, cuerpo vacío.
// Devuelve el cuerpo de la respuesta para que el llamante interprete los errores.
func (c *Client) officialSoftDeleteRequest(folder bool, id string) (*http.Request, error) {
	u := fmt.Sprintf("%s/media?action=softdelete&id=%s", c.baseURL, id)
	if folder {
		u = fmt.Sprintf("%s/media/folder?action=softdelete&id=%s", c.baseURL, id)
	}
	req, err := http.NewRequest("POST", u, strings.NewReader(""))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	return req, nil
}

func logOfficialFallback(op string, id interface{}, err error) {
	logger.Log("[SAPI] ↪ %s de %v como la app rechazado (%v); se prueba la llamada alternativa.", op, id, err)
}
