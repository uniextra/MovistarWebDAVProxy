package sapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// EmptyTrash vacía la papelera como el botón de la web (POST /media/trash?action=empty).
func (c *Client) EmptyTrash() error {
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/media/trash?action=empty", c.baseURL), strings.NewReader(""))
	if err != nil {
		return err
	}
	resp, err := c.DoRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("empty trash failed (%d): %s", resp.StatusCode, string(b))
	}
	if code := apiErrorCode(b); code != "" {
		return fmt.Errorf("empty trash API error (%s): %s", code, string(b))
	}
	return nil
}

// accountSettingKeys son las opciones de /system/information que interesan al
// proxy, las mismas que lee la web al cargar.
var accountSettingKeys = []string{
	"sapi.upload.max-size-in-mb",
	"folders.softdelete-updates-limit",
	"folders.move-updates-limit",
	"trash.cleanup.items-expire-time-in-minutes",
	"sapi.login.persistent-login-cookie.enabled",
}

// AccountSettings extrae de la respuesta de /system/information las opciones de
// accountSettingKeys (en la raíz, en "data" o, la del login persistente, en "logininfo").
func AccountSettings(info map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	var scopes []map[string]interface{}
	add := func(v interface{}) {
		if m, ok := v.(map[string]interface{}); ok {
			scopes = append(scopes, m)
			if li, ok := m["logininfo"].(map[string]interface{}); ok {
				scopes = append(scopes, li)
			}
		}
	}
	add(info)
	add(info["data"])
	for _, k := range accountSettingKeys {
		for _, sc := range scopes {
			if v, ok := sc[k]; ok {
				out[k] = v
				break
			}
		}
	}
	return out
}

// apiErrorCode devuelve el código de error de SAPI del cuerpo ({"error":{"code":...}}), si lo hay.
func apiErrorCode(b []byte) string {
	code, _, _ := apiError(b)
	return code
}

// apiError devuelve el error de SAPI del cuerpo ({"error":{"code","message"}}); ok
// indica si lo hay.
func apiError(b []byte) (code, message string, ok bool) {
	var res struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &res) != nil || res.Error == nil {
		return "", "", false
	}
	return res.Error.Code, res.Error.Message, true
}

// ErrCopyOutcomeUnknown: la petición de copia no obtuvo respuesta, así que no se sabe
// si O2 la hizo (las de archivos grandes tardan más que el tiempo de espera).
var ErrCopyOutcomeUnknown = errors.New("resultado de la copia en la nube desconocido")

// CopyFile copia un archivo dentro de la nube, como la web (POST /media/file?action=copy
// con {"data":{"id","parentid","name"}}), sin descargarlo ni volver a subirlo.
// Devuelve el ID de la copia.
func (c *Client) CopyFile(mediaID string, parentID int64, name string) (string, error) {
	var id interface{} = mediaID
	if n, err := strconv.ParseInt(mediaID, 10, 64); err == nil {
		id = n
	}
	data := map[string]interface{}{"id": id, "parentid": parentID}
	if name != "" {
		data["name"] = toCloudName(name)
	}
	body, _ := json.Marshal(map[string]interface{}{"data": data})
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/media/file?action=copy", c.baseURL), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	resp, err := c.DoRequest(req)
	if err != nil {
		if errors.Is(err, ErrWAFBlocked) {
			return "", err
		}
		// Sin respuesta (p. ej. O2 tarda más de 60 s en copiar 3 GB): la copia puede
		// terminar igualmente en el servidor. Copiar a mano daría un duplicado.
		return "", fmt.Errorf("%w: %v", ErrCopyOutcomeUnknown, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusGatewayTimeout || resp.StatusCode == http.StatusBadGateway {
		return "", fmt.Errorf("%w: HTTP %d", ErrCopyOutcomeUnknown, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("copy file failed (%d): %s", resp.StatusCode, string(b))
	}
	if code := apiErrorCode(b); code != "" {
		return "", fmt.Errorf("copy file API error (%s): %s", code, string(b))
	}
	// La web solo comprueba que haya "data"; el ID de la copia puede venir en
	// data.id o dentro de data.media / data.file.
	var res struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &res); err != nil || res.Data == nil {
		return "", fmt.Errorf("copy file: respuesta inesperada: %s", string(b))
	}
	if newID := rawID(res.Data["id"]); newID != "" {
		return newID, nil
	}
	for _, k := range []string{"media", "file"} {
		var inner struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(res.Data[k], &inner) == nil {
			if newID := rawID(inner.ID); newID != "" {
				return newID, nil
			}
		}
	}
	return "", fmt.Errorf("copy file: la respuesta no trae el ID de la copia: %s", string(b))
}

func rawID(r json.RawMessage) string {
	id := strings.Trim(strings.TrimSpace(string(r)), `"`)
	if id == "null" {
		return ""
	}
	return id
}
