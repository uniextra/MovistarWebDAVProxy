package sapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const (
	// MaxFolderBatchSize defines the maximum number of folder IDs per request to SAPI.
	MaxFolderBatchSize = 300

	// MaxMediaBatchSize defines the maximum number of media IDs per request to SAPI.
	MaxMediaBatchSize = 1000
)

// ItemChanges represents the list of changed entity IDs for a specific media or folder type.
// N: New entity IDs (commonly returned for folders).
// U: Updated entity IDs (commonly returned for media/files).
// D: Deleted entity IDs.
// L: Locked entity IDs.
type ItemChanges struct {
	N []int64 `json:"N,omitempty"`
	U []int64 `json:"U,omitempty"`
	D []int64 `json:"D,omitempty"`
	L []int64 `json:"L,omitempty"`
}

// HasChanges returns true if any changes (N, U, D, L) are present.
func (ic ItemChanges) HasChanges() bool {
	return len(ic.N) > 0 || len(ic.U) > 0 || len(ic.D) > 0 || len(ic.L) > 0
}

// ProfileChangesData contains the categorized changes returned by /sapi/profile/changes.
type ProfileChangesData struct {
	Picture ItemChanges            `json:"picture,omitempty"`
	Video   ItemChanges            `json:"video,omitempty"`
	Audio   ItemChanges            `json:"audio,omitempty"`
	File    ItemChanges            `json:"file,omitempty"`
	Folder  ItemChanges            `json:"folder,omitempty"`
	Raw     map[string]ItemChanges `json:"-"`
}

func (pcd *ProfileChangesData) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[]")) || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawMap); err != nil {
		return nil // Graceful si data no es un objeto
	}
	pcd.Raw = make(map[string]ItemChanges)
	for k, v := range rawMap {
		var ic ItemChanges
		if err := json.Unmarshal(v, &ic); err == nil {
			pcd.Raw[k] = ic
			switch k {
			case "picture":
				pcd.Picture = ic
			case "video":
				pcd.Video = ic
			case "audio":
				pcd.Audio = ic
			case "file":
				pcd.File = ic
			case "folder":
				pcd.Folder = ic
			}
		}
	}
	return nil
}

func (pcd ProfileChangesData) MarshalJSON() ([]byte, error) {
	if pcd.Raw != nil {
		return json.Marshal(pcd.Raw)
	}
	m := make(map[string]ItemChanges)
	if pcd.Picture.HasChanges() {
		m["picture"] = pcd.Picture
	}
	if pcd.Video.HasChanges() {
		m["video"] = pcd.Video
	}
	if pcd.Audio.HasChanges() {
		m["audio"] = pcd.Audio
	}
	if pcd.File.HasChanges() {
		m["file"] = pcd.File
	}
	if pcd.Folder.HasChanges() {
		m["folder"] = pcd.Folder
	}
	return json.Marshal(m)
}

// ProfileChangesResponse represents the parsed response from GET /sapi/profile/changes.
type ProfileChangesResponse struct {
	RequestTime  int64              `json:"requesttime"`
	ResponseTime int64              `json:"responsetime"`
	Data         ProfileChangesData `json:"data"`
	Error        interface{}        `json:"error,omitempty"`
}

// HasChanges returns true if any item changes exist in the response.
func (p *ProfileChangesResponse) HasChanges() bool {
	if p == nil {
		return false
	}
	return p.Data.Picture.HasChanges() ||
		p.Data.Video.HasChanges() ||
		p.Data.Audio.HasChanges() ||
		p.Data.File.HasChanges() ||
		p.Data.Folder.HasChanges()
}

// FolderChanges returns the item changes for folders.
func (p *ProfileChangesResponse) FolderChanges() ItemChanges {
	if p == nil {
		return ItemChanges{}
	}
	return p.Data.Folder
}

// MediaChanges aggregates changes across all media types (picture, video, audio, file).
func (p *ProfileChangesResponse) MediaChanges() ItemChanges {
	if p == nil {
		return ItemChanges{}
	}
	var res ItemChanges
	for _, ic := range []ItemChanges{p.Data.Picture, p.Data.Video, p.Data.Audio, p.Data.File} {
		res.N = append(res.N, ic.N...)
		res.U = append(res.U, ic.U...)
		res.D = append(res.D, ic.D...)
		res.L = append(res.L, ic.L...)
	}
	return res
}

// isNumericCursor checks if the cursor string consists only of digits.
func isNumericCursor(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// GetProfileChanges retrieves incremental profile changes since the given epoch milliseconds cursor 'from'.
// If 'from' is empty or non-numeric, "0" is used.
// The endpoint queried matches the official Windows desktop client (Movistar Cloud-sync.exe):
// GET /sapi/profile/changes?action=get&from={from}&type=picture,video,audio,file,folder&responsetime=true&sortby=creationdate&sortorder=descending&locked=true
func (c *Client) GetProfileChanges(from string) (*ProfileChangesResponse, error) {
	if !isNumericCursor(from) {
		from = "0"
	}

	u := fmt.Sprintf("%s/profile/changes?action=get&from=%s&type=picture,video,audio,file,folder&responsetime=true&sortby=creationdate&sortorder=descending&locked=true",
		c.baseURL, url.QueryEscape(from))

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("create profile changes request: %w", err)
	}

	resp, err := c.DoRequest(req)
	if err != nil {
		return nil, fmt.Errorf("execute profile changes request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("sesión SAPI no autorizada (HTTP 401): vincula la cuenta en el panel web")
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("get profile changes failed (HTTP %d): %s", resp.StatusCode, string(b))
	}

	var res ProfileChangesResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("decode profile changes response: %w", err)
	}

	if res.Error != nil {
		return nil, fmt.Errorf("API error in profile changes: %v", res.Error)
	}

	return &res, nil
}

// GetFoldersByIDs fetches full metadata for the specified folder IDs using batching of 300 IDs
// via POST /sapi/media/folder?action=get.
func (c *Client) GetFoldersByIDs(ids []int64) ([]FolderItem, error) {
	if len(ids) == 0 {
		return []FolderItem{}, nil
	}

	var allFolders []FolderItem
	u := fmt.Sprintf("%s/media/folder?action=get", c.baseURL)

	for i := 0; i < len(ids); i += MaxFolderBatchSize {
		end := i + MaxFolderBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]

		payload := map[string]interface{}{
			"data": map[string]interface{}{
				"ids": batch,
			},
		}

		bodyBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal get folders payload: %w", err)
		}

		req, err := http.NewRequest("POST", u, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("create get folders request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")

		resp, err := c.DoRequest(req)
		if err != nil {
			return nil, fmt.Errorf("execute get folders request (batch %d-%d): %w", i, end, err)
		}

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return nil, fmt.Errorf("get folders by IDs failed (batch %d-%d, HTTP %d): %s", i, end, resp.StatusCode, string(b))
		}

		var res struct {
			Data struct {
				Folders []FolderItem `json:"folders"`
			} `json:"data"`
			Error interface{} `json:"error"`
		}

		err = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode get folders response (batch %d-%d): %w", i, end, err)
		}

		if res.Error != nil {
			return nil, fmt.Errorf("API error in get folders (batch %d-%d): %v", i, end, res.Error)
		}

		allFolders = append(allFolders, res.Data.Folders...)
	}

	return allFolders, nil
}

// GetMediaByIDs fetches full metadata for the specified media IDs using batching of 1,000 IDs
// via POST /sapi/media?action=get&origin=omh,dropbox with fields ["name", "url", "creationdate", "modificationdate", "size", "etag", "folderid"].
func (c *Client) GetMediaByIDs(ids []int64) ([]MediaItem, error) {
	if len(ids) == 0 {
		return []MediaItem{}, nil
	}

	var allMedia []MediaItem
	u := fmt.Sprintf("%s/media?action=get&origin=omh,dropbox", c.baseURL)

	fields := []string{"name", "url", "creationdate", "modificationdate", "size", "etag", "folderid"}

	for i := 0; i < len(ids); i += MaxMediaBatchSize {
		end := i + MaxMediaBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]

		payload := map[string]interface{}{
			"data": map[string]interface{}{
				"ids":    batch,
				"fields": fields,
			},
		}

		bodyBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal get media payload: %w", err)
		}

		req, err := http.NewRequest("POST", u, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("create get media request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")

		resp, err := c.DoRequest(req)
		if err != nil {
			return nil, fmt.Errorf("execute get media request (batch %d-%d): %w", i, end, err)
		}

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return nil, fmt.Errorf("get media by IDs failed (batch %d-%d, HTTP %d): %s", i, end, resp.StatusCode, string(b))
		}

		var res struct {
			Data struct {
				Media []MediaItem `json:"media"`
			} `json:"data"`
			Error interface{} `json:"error"`
		}

		err = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode get media response (batch %d-%d): %w", i, end, err)
		}

		if res.Error != nil {
			return nil, fmt.Errorf("API error in get media (batch %d-%d): %v", i, end, res.Error)
		}

		allMedia = append(allMedia, res.Data.Media...)
	}

	return allMedia, nil
}
