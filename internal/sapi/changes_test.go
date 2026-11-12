package sapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"movistar-webdav-proxy/internal/config"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := NewClient(config.AccountConfig{
		Provider:      "movistar",
		Phone:         "612345678",
		AccessToken:   "test_token",
		RefreshToken:  "test_refresh",
		ValidationKey: "test_vk",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	c.SetBaseURLs(srv.URL+"/sapi", srv.URL)
	return c, srv
}

func TestGetProfileChanges_Success(t *testing.T) {
	var capturedPath string
	var capturedQuery map[string][]string

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.Query()

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"requesttime":  1794389693022,
			"responsetime": 1794389695887,
			"data": map[string]interface{}{
				"picture": map[string]interface{}{
					"U": []int64{7178339481, 7177699130},
					"D": []int64{7167035131},
				},
				"video": map[string]interface{}{
					"U": []int64{7179372756},
				},
				"file": map[string]interface{}{
					"U": []int64{7191646907},
					"D": []int64{7190751844},
				},
				"folder": map[string]interface{}{
					"N": []int64{147103624, 146625936},
					"D": []int64{147011190},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	resp, err := client.GetProfileChanges("")
	if err != nil {
		t.Fatalf("GetProfileChanges failed: %v", err)
	}

	if capturedPath != "/sapi/profile/changes" {
		t.Errorf("expected path /sapi/profile/changes, got %s", capturedPath)
	}
	if capturedQuery["action"][0] != "get" {
		t.Errorf("expected action=get, got %v", capturedQuery["action"])
	}
	if capturedQuery["from"][0] != "0" {
		t.Errorf("expected from=0 for empty from, got %v", capturedQuery["from"])
	}
	if capturedQuery["origin"][0] != "omh,dropbox" {
		t.Errorf("expected origin=omh,dropbox, got %v", capturedQuery["origin"])
	}
	if capturedQuery["locked"][0] != "true" {
		t.Errorf("expected locked=true, got %v", capturedQuery["locked"])
	}

	if resp.RequestTime != 1794389693022 {
		t.Errorf("unexpected RequestTime: %d", resp.RequestTime)
	}
	if resp.ResponseTime != 1794389695887 {
		t.Errorf("unexpected ResponseTime: %d", resp.ResponseTime)
	}
	if !resp.HasChanges() {
		t.Error("expected HasChanges to be true")
	}

	folderChanges := resp.FolderChanges()
	if len(folderChanges.N) != 2 || folderChanges.N[0] != 147103624 || folderChanges.N[1] != 146625936 {
		t.Errorf("unexpected folder N changes: %v", folderChanges.N)
	}
	if len(folderChanges.D) != 1 || folderChanges.D[0] != 147011190 {
		t.Errorf("unexpected folder D changes: %v", folderChanges.D)
	}

	mediaChanges := resp.MediaChanges()
	if len(mediaChanges.U) != 4 { // 2 picture + 1 video + 1 file
		t.Errorf("expected 4 updated media, got %d (%v)", len(mediaChanges.U), mediaChanges.U)
	}
	if len(mediaChanges.D) != 2 { // 1 picture + 1 file
		t.Errorf("expected 2 deleted media, got %d (%v)", len(mediaChanges.D), mediaChanges.D)
	}
}

func TestGetProfileChanges_Incremental(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		from := r.URL.Query().Get("from")
		if from != "1794389695887" {
			t.Errorf("expected from=1794389695887, got %s", from)
		}

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"requesttime":  1794389713320,
			"responsetime": 1794389713679,
			"data":         map[string]interface{}{},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	resp, err := client.GetProfileChanges("1794389695887")
	if err != nil {
		t.Fatalf("GetProfileChanges incremental failed: %v", err)
	}

	if resp.HasChanges() {
		t.Error("expected HasChanges to be false for empty data")
	}
	if resp.ResponseTime != 1794389713679 {
		t.Errorf("unexpected ResponseTime: %d", resp.ResponseTime)
	}
}

func TestGetProfileChanges_Errors(t *testing.T) {
	t.Run("401 Unauthorized", func(t *testing.T) {
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
		_, err := client.GetProfileChanges("0")
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Errorf("expected 401 error, got %v", err)
		}
	})

	t.Run("500 Server Error", func(t *testing.T) {
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("Internal error"))
		})
		_, err := client.GetProfileChanges("0")
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Errorf("expected 500 error, got %v", err)
		}
	})

	t.Run("API Error in JSON", func(t *testing.T) {
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "PRO-1001",
					"message": "Invalid request",
				},
			})
		})
		_, err := client.GetProfileChanges("0")
		if err == nil || !strings.Contains(err.Error(), "PRO-1001") {
			t.Errorf("expected API error, got %v", err)
		}
	})
}

func TestGetFoldersByIDs_Empty(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("handler should not have been called for empty IDs")
	})

	folders, err := client.GetFoldersByIDs(nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(folders) != 0 {
		t.Errorf("expected 0 folders, got %d", len(folders))
	}
}

func TestGetFoldersByIDs_SingleBatch(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/sapi/media/folder" {
			t.Errorf("expected /sapi/media/folder, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("action") != "get" {
			t.Errorf("expected action=get, got %s", r.URL.Query().Get("action"))
		}

		var body struct {
			Data struct {
				IDs []int64 `json:"ids"`
			} `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body failed: %v", err)
		}

		if len(body.Data.IDs) != 2 || body.Data.IDs[0] != 147103624 || body.Data.IDs[1] != 146625936 {
			t.Errorf("unexpected IDs in request: %v", body.Data.IDs)
		}

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"folders": []map[string]interface{}{
					{
						"id":       147103624,
						"name":     "Bici",
						"parentid": 145379669,
						"date":     1794385000042,
						"magic":    false,
						"offline":  false,
					},
					{
						"id":       146625936,
						"name":     "@reader",
						"parentid": 145739639,
						"date":     1793569278505,
						"magic":    false,
						"offline":  false,
					},
				},
			},
			"responsetime": 1794389763955,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	folders, err := client.GetFoldersByIDs([]int64{147103624, 146625936})
	if err != nil {
		t.Fatalf("GetFoldersByIDs failed: %v", err)
	}

	if len(folders) != 2 {
		t.Fatalf("expected 2 folders, got %d", len(folders))
	}
	if folders[0].ID != 147103624 || folders[0].Name != "Bici" || folders[0].ParentID != 145379669 {
		t.Errorf("unexpected folder[0]: %+v", folders[0])
	}
	if folders[0].ModificationDate.String() != "1794385000042" {
		t.Errorf("unexpected folder[0] ModificationDate: %v", folders[0].ModificationDate)
	}
	if folders[1].ID != 146625936 || folders[1].Name != "@reader" || folders[1].ParentID != 145739639 {
		t.Errorf("unexpected folder[1]: %+v", folders[1])
	}
}

func TestGetFoldersByIDs_Batching300(t *testing.T) {
	var mu sync.Mutex
	var batchSizes []int

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Data struct {
				IDs []int64 `json:"ids"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		mu.Lock()
		batchSizes = append(batchSizes, len(body.Data.IDs))
		mu.Unlock()

		var resFolders []map[string]interface{}
		for _, id := range body.Data.IDs {
			resFolders = append(resFolders, map[string]interface{}{
				"id":       id,
				"name":     fmt.Sprintf("folder_%d", id),
				"parentid": 1,
				"date":     1700000000000,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"folders": resFolders,
			},
		})
	})

	// Generate 750 IDs -> Should be split into batches of 300, 300, 150
	const totalIDs = 750
	ids := make([]int64, totalIDs)
	for i := 0; i < totalIDs; i++ {
		ids[i] = int64(1000 + i)
	}

	folders, err := client.GetFoldersByIDs(ids)
	if err != nil {
		t.Fatalf("GetFoldersByIDs failed: %v", err)
	}

	if len(folders) != totalIDs {
		t.Fatalf("expected %d folders, got %d", totalIDs, len(folders))
	}

	mu.Lock()
	defer mu.Unlock()
	expectedBatches := []int{300, 300, 150}
	if len(batchSizes) != len(expectedBatches) {
		t.Fatalf("expected %d batches, got %d: %v", len(expectedBatches), len(batchSizes), batchSizes)
	}
	for i, exp := range expectedBatches {
		if batchSizes[i] != exp {
			t.Errorf("batch %d: expected size %d, got %d", i, exp, batchSizes[i])
		}
	}
}

func TestGetMediaByIDs_Empty(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("handler should not have been called for empty IDs")
	})

	media, err := client.GetMediaByIDs(nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(media) != 0 {
		t.Errorf("expected 0 media, got %d", len(media))
	}
}

func TestGetMediaByIDs_SingleBatch(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/sapi/media" {
			t.Errorf("expected /sapi/media, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("action") != "get" {
			t.Errorf("expected action=get, got %s", r.URL.Query().Get("action"))
		}

		var body struct {
			Data struct {
				IDs    []int64  `json:"ids"`
				Fields []string `json:"fields"`
			} `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body failed: %v", err)
		}

		if len(body.Data.IDs) != 2 || body.Data.IDs[0] != 7178339481 || body.Data.IDs[1] != 7191646907 {
			t.Errorf("unexpected IDs in request: %v", body.Data.IDs)
		}

		expectedFields := []string{"name", "url", "creationdate", "modificationdate", "size", "etag", "folderid"}
		if len(body.Data.Fields) != len(expectedFields) {
			t.Errorf("expected %d fields, got %v", len(expectedFields), body.Data.Fields)
		}

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"media": []map[string]interface{}{
					{
						"id":               "7178339481",
						"name":             "photo.jpg",
						"size":             58125,
						"url":              "https://micloud.movistar.es/dl/photo.jpg",
						"etag":             "tag1",
						"folder":           145739638,
						"creationdate":     1791130782104,
						"modificationdate": 1791130782104,
					},
					{
						"id":               7191646907,
						"name":             "synobkpinfo.db",
						"size":             12288,
						"url":              "https://micloud.movistar.es/dl/db",
						"etag":             "tag2",
						"folderid":         145739638,
						"creationdate":     1791617780000,
						"modificationdate": 1791617780000,
					},
				},
			},
			"responsetime": 1794389782782,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	media, err := client.GetMediaByIDs([]int64{7178339481, 7191646907})
	if err != nil {
		t.Fatalf("GetMediaByIDs failed: %v", err)
	}

	if len(media) != 2 {
		t.Fatalf("expected 2 media items, got %d", len(media))
	}
	if media[0].ID != "7178339481" || media[0].Name != "photo.jpg" || media[0].Size != 58125 || media[0].FolderID != 145739638 {
		t.Errorf("unexpected media[0]: %+v", media[0])
	}
	if media[1].ID != "7191646907" || media[1].Name != "synobkpinfo.db" || media[1].Size != 12288 || media[1].FolderID != 145739638 {
		t.Errorf("unexpected media[1]: %+v", media[1])
	}
}

func TestGetMediaByIDs_Batching1000(t *testing.T) {
	var mu sync.Mutex
	var batchSizes []int

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Data struct {
				IDs    []int64  `json:"ids"`
				Fields []string `json:"fields"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		mu.Lock()
		batchSizes = append(batchSizes, len(body.Data.IDs))
		mu.Unlock()

		var resMedia []map[string]interface{}
		for _, id := range body.Data.IDs {
			resMedia = append(resMedia, map[string]interface{}{
				"id":               fmt.Sprintf("%d", id),
				"name":             fmt.Sprintf("file_%d.dat", id),
				"size":             1024,
				"url":              fmt.Sprintf("https://dl/%d", id),
				"etag":             "etag",
				"folderid":         10,
				"creationdate":     1700000000000,
				"modificationdate": 1700000000000,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"media": resMedia,
			},
		})
	})

	// Generate 2500 IDs -> Should be split into batches of 1000, 1000, 500
	const totalIDs = 2500
	ids := make([]int64, totalIDs)
	for i := 0; i < totalIDs; i++ {
		ids[i] = int64(100000 + i)
	}

	media, err := client.GetMediaByIDs(ids)
	if err != nil {
		t.Fatalf("GetMediaByIDs failed: %v", err)
	}

	if len(media) != totalIDs {
		t.Fatalf("expected %d media, got %d", totalIDs, len(media))
	}

	mu.Lock()
	defer mu.Unlock()
	expectedBatches := []int{1000, 1000, 500}
	if len(batchSizes) != len(expectedBatches) {
		t.Fatalf("expected %d batches, got %d: %v", len(expectedBatches), len(batchSizes), batchSizes)
	}
	for i, exp := range expectedBatches {
		if batchSizes[i] != exp {
			t.Errorf("batch %d: expected size %d, got %d", i, exp, batchSizes[i])
		}
	}
}

func TestFolderItem_UnmarshalJSON_Flexibility(t *testing.T) {
	tests := []struct {
		name         string
		jsonInput    string
		wantID       int64
		wantParentID int64
		wantDate     string
	}{
		{
			name:         "string IDs and aux.Date",
			jsonInput:    `{"id":"101","name":"FolderA","parentid":"50","date":"20261111T120000Z"}`,
			wantID:       101,
			wantParentID: 50,
			wantDate:     "20261111T120000Z",
		},
		{
			name:         "numeric IDs and explicit creation/modification date",
			jsonInput:    `{"id":202,"name":"FolderB","parentid":100,"creationdate":"20260101T000000Z","modificationdate":"20260102T000000Z"}`,
			wantID:       202,
			wantParentID: 100,
			wantDate:     "20260101T000000Z",
		},
		{
			name:         "numeric timestamp in date",
			jsonInput:    `{"id":303,"name":"FolderC","parentid":0,"date":1700000000000}`,
			wantID:       303,
			wantParentID: 0,
			wantDate:     "1700000000000",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var f FolderItem
			if err := json.Unmarshal([]byte(tc.jsonInput), &f); err != nil {
				t.Fatalf("Unmarshal error: %v", err)
			}
			if f.ID != tc.wantID {
				t.Errorf("expected ID %d, got %d", tc.wantID, f.ID)
			}
			if f.ParentID != tc.wantParentID {
				t.Errorf("expected ParentID %d, got %d", tc.wantParentID, f.ParentID)
			}
			if string(f.CreationDate) != tc.wantDate {
				t.Errorf("expected CreationDate %q, got %q", tc.wantDate, f.CreationDate)
			}
		})
	}
}

func TestMediaItem_UnmarshalJSON_Flexibility(t *testing.T) {
	tests := []struct {
		name         string
		jsonInput    string
		wantID       string
		wantSize     int64
		wantFolderID int64
		wantDate     string
	}{
		{
			name:         "aux.Folder and aux.Date with string ID",
			jsonInput:    `{"id":"media_1","name":"photo.jpg","size":"4096","folder":"77","date":"20261111T120000Z"}`,
			wantID:       "media_1",
			wantSize:     4096,
			wantFolderID: 77,
			wantDate:     "20261111T120000Z",
		},
		{
			name:         "numeric ID and folderid with float",
			jsonInput:    `{"id":9999,"name":"doc.pdf","size":1048576,"folderid":88,"creationdate":"20260501T080000Z"}`,
			wantID:       "9999",
			wantSize:     1048576,
			wantFolderID: 88,
			wantDate:     "20260501T080000Z",
		},
		{
			name:         "aux.Folder as float with timestamp date",
			jsonInput:    `{"id":"media_2","name":"video.mp4","size":20971520,"folder":55,"date":1700000000000}`,
			wantID:       "media_2",
			wantSize:     20971520,
			wantFolderID: 55,
			wantDate:     "1700000000000",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var m MediaItem
			if err := json.Unmarshal([]byte(tc.jsonInput), &m); err != nil {
				t.Fatalf("Unmarshal error: %v", err)
			}
			if m.ID != tc.wantID {
				t.Errorf("expected ID %q, got %q", tc.wantID, m.ID)
			}
			if m.Size != tc.wantSize {
				t.Errorf("expected Size %d, got %d", tc.wantSize, m.Size)
			}
			if m.FolderID != tc.wantFolderID {
				t.Errorf("expected FolderID %d, got %d", tc.wantFolderID, m.FolderID)
			}
			if string(m.CreationDate) != tc.wantDate {
				t.Errorf("expected CreationDate %q, got %q", tc.wantDate, m.CreationDate)
			}
		})
	}
}
