//go:build live

package sapi_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"movistar-webdav-proxy/internal/cache"
	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

func TestLive_ZeroByteVsOneByteBehavior(t *testing.T) {
	cfg := config.LoadAccountConfig("")
	if cfg.Phone == "" && cfg.RefreshToken == "" && cfg.JSessionID == "" {
		t.Skip("Skipping live test: no credentials available in tokens directory")
	}

	client, err := sapi.NewClient(cfg)
	if err != nil {
		t.Fatalf("Failed to initialize SAPI client: %v", err)
	}

	// Find a subfolder inside root to test uploading (e.g. Bici or first subfolder)
	testFolderID := int64(147103624) // Bici
	phoneClean := config.SanitizePhone(cfg.Phone)
	dbPath := filepath.Join(config.TokensDir, fmt.Sprintf("metadata_%s.db", phoneClean))
	if _, err := os.Stat(dbPath); err == nil {
		ms, err := cache.NewMetadataStore(dbPath)
		if err == nil {
			defer ms.Close()
			if root, err := ms.GetRootFolder(); err == nil && root != nil {
				if subs, err := ms.ListSubfolders(root.ID); err == nil && len(subs) > 0 {
					testFolderID = subs[0].ID
				}
			}
		}
	}

	zeroFileName := "test_zero_byte_" + time.Now().Format("20060102150405") + ".txt"
	oneFileName := "test_one_byte_" + time.Now().Format("20060102150405") + ".txt"

	t.Logf("=== TAREA 1 LIVE TEST: Testing 0-byte file upload to folder %d ===", testFolderID)
	zeroItem, zeroErr := client.UploadFileToFolder(zeroFileName, bytes.NewReader([]byte{}), 0, testFolderID, "application/octet-stream", "", time.Time{})
	t.Logf("0-byte upload result: item=%+v, err=%v", zeroItem, zeroErr)

	if zeroErr == nil && zeroItem != nil && zeroItem.ID != "" {
		defer func() {
			delErr := client.DeleteFile(zeroItem.ID)
			t.Logf("0-byte cleanup delete result: %v", delErr)
		}()

		zeroID, _ := strconv.ParseInt(zeroItem.ID, 10, 64)

		// Check GetMediaByIDs
		if zeroID > 0 {
			mediaList, err := client.GetMediaByIDs([]int64{zeroID})
			t.Logf("0-byte GetMediaByIDs result: count=%d, err=%v", len(mediaList), err)
			if len(mediaList) > 0 {
				t.Logf("0-byte item details: %+v", mediaList[0])
			}
		}

		// Check ListAllMediaInFolder
		folderMedia, err := client.ListAllMediaInFolder(testFolderID)
		foundInFolder := false
		if err == nil {
			for _, m := range folderMedia {
				if m.ID == zeroItem.ID || m.Name == zeroFileName {
					foundInFolder = true
					t.Logf("0-byte found in folderMedia: %+v", m)
					break
				}
			}
		}
		t.Logf("0-byte ListAllMediaInFolder result: found=%v, count=%d, err=%v", foundInFolder, len(folderMedia), err)

		// Check GetProfileChanges
		changes, err := client.GetProfileChanges("19700101T000000Z")
		foundInChanges := false
		if err == nil && changes != nil {
			for _, id := range append(changes.Data.File.N, changes.Data.File.U...) {
				if id == zeroID {
					foundInChanges = true
					break
				}
			}
		}
		t.Logf("0-byte GetProfileChanges result: found=%v, err=%v", foundInChanges, err)
	}

	time.Sleep(2 * time.Second)

	t.Logf("=== TAREA 1 LIVE TEST: Testing 1-byte control file upload to folder %d ===", testFolderID)
	oneItem, oneErr := client.UploadFileToFolder(oneFileName, bytes.NewReader([]byte("X")), 1, testFolderID, "application/octet-stream", "", time.Time{})
	t.Logf("1-byte control upload result: item=%+v, err=%v", oneItem, oneErr)

	if oneErr == nil && oneItem != nil && oneItem.ID != "" {
		defer func() {
			delErr := client.DeleteFile(oneItem.ID)
			t.Logf("1-byte cleanup delete result: %v", delErr)
		}()

		oneID, _ := strconv.ParseInt(oneItem.ID, 10, 64)

		// Check GetMediaByIDs
		if oneID > 0 {
			mediaList, err := client.GetMediaByIDs([]int64{oneID})
			t.Logf("1-byte GetMediaByIDs result: count=%d, err=%v", len(mediaList), err)
			if len(mediaList) > 0 {
				t.Logf("1-byte item details: %+v", mediaList[0])
			}
		}

		// Check ListAllMediaInFolder
		folderMedia, err := client.ListAllMediaInFolder(testFolderID)
		foundInFolder := false
		if err == nil {
			for _, m := range folderMedia {
				if m.ID == oneItem.ID || m.Name == oneFileName {
					foundInFolder = true
					t.Logf("1-byte found in folderMedia: %+v", m)
					break
				}
			}
		}
		t.Logf("1-byte ListAllMediaInFolder result: found=%v, count=%d, err=%v", foundInFolder, len(folderMedia), err)

		// Check GetProfileChanges
		changes, err := client.GetProfileChanges("19700101T000000Z")
		foundInChanges := false
		if err == nil && changes != nil {
			for _, id := range append(changes.Data.File.N, changes.Data.File.U...) {
				if id == oneID {
					foundInChanges = true
					break
				}
			}
		}
		t.Logf("1-byte GetProfileChanges result: found=%v, err=%v", foundInChanges, err)
	}
}
