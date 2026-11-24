package cache

import (
	"fmt"
	"strings"
	"testing"

	"movistar-webdav-proxy/internal/sapi"
)

func explainQueryPlan(t *testing.T, store *MetadataStore, query string, args ...any) string {
	t.Helper()
	rows, err := store.DB().Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN failed: %v", err)
	}
	defer rows.Close()

	var plans []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		plans = append(plans, detail)
	}
	return strings.Join(plans, "\n")
}

func TestMetadataStore_BinaryCollationUsesCompositeIndex(t *testing.T) {
	store, _ := createTempStore(t)

	// Poblar 2.000 ficheros y 100 subcarpetas para que SQLite use los índices
	folders := make([]sapi.FolderItem, 0, 100)
	for i := 0; i < 100; i++ {
		folders = append(folders, sapi.FolderItem{
			ID:       int64(1000 + i),
			Name:     fmt.Sprintf("Sub_%03d", i),
			ParentID: 100,
		})
	}
	if err := store.UpsertFoldersBatch(folders); err != nil {
		t.Fatalf("UpsertFoldersBatch: %v", err)
	}

	media := make([]sapi.MediaItem, 0, 2000)
	for i := 0; i < 2000; i++ {
		media = append(media, sapi.MediaItem{
			ID:       fmt.Sprintf("m-%04d", i),
			Name:     fmt.Sprintf("duplicati-b%04d.dblock.zip.aes", i),
			Size:     50 * 1024 * 1024,
			FolderID: 1000,
		})
	}
	if err := store.UpsertMediaBatch(media); err != nil {
		t.Fatalf("UpsertMediaBatch: %v", err)
	}

	// 1. Modo por defecto (COLLATE BINARY): debe usar ambos campos (folder_id y name) del índice _bin,
	// incluso con el ORDER BY real de GetMediaByFolderAndName / GetFolderByParentAndName.
	mediaPlanBin := explainQueryPlan(
		t, store,
		`SELECT id, name, size, url, etag, folder_id, creation_date, modification_date
		 FROM media
		 WHERE folder_id = ? AND name = ? COLLATE BINARY
		 ORDER BY CASE WHEN name = ? COLLATE BINARY THEN 0 ELSE 1 END, modification_date DESC, id DESC`,
		1000, "duplicati-b1234.dblock.zip.aes", "duplicati-b1234.dblock.zip.aes",
	)
	if !strings.Contains(mediaPlanBin, "idx_media_folder_name_bin") || !strings.Contains(mediaPlanBin, "folder_id=? AND name=?") {
		t.Fatalf("BINARY media lookup debe usar idx_media_folder_name_bin(folder_id=? AND name=?), plan:\n%s", mediaPlanBin)
	}

	folderPlanBin := explainQueryPlan(
		t, store,
		`SELECT id, name, parent_id, magic, offline, creation_date, modification_date
		 FROM folders
		 WHERE parent_id = ? AND name = ? COLLATE BINARY
		 ORDER BY CASE WHEN name = ? COLLATE BINARY THEN 0 ELSE 1 END
		 LIMIT 1`,
		100, "Sub_042", "Sub_042",
	)
	if !strings.Contains(folderPlanBin, "idx_folders_parent_name_bin") || !strings.Contains(folderPlanBin, "parent_id=? AND name=?") {
		t.Fatalf("BINARY folder lookup debe usar idx_folders_parent_name_bin(parent_id=? AND name=?), plan:\n%s", folderPlanBin)
	}

	// 2. Modo opcional (COLLATE NOCASE): debe seguir usando los índices NOCASE
	mediaPlanNocase := explainQueryPlan(
		t, store,
		`SELECT id FROM media WHERE folder_id = ? AND name = ? COLLATE NOCASE`,
		1000, "DUPLICATI-B1234.DBLOCK.ZIP.AES",
	)
	if !strings.Contains(mediaPlanNocase, "idx_media_folder_name") || !strings.Contains(mediaPlanNocase, "folder_id=? AND name=?") {
		t.Fatalf("NOCASE media lookup debe usar idx_media_folder_name(folder_id=? AND name=?), plan:\n%s", mediaPlanNocase)
	}

	// 3. Verificación funcional de archivos y carpetas en modo BINARY (distingue mayúsculas) vs NOCASE
	got, err := store.GetMediaByFolderAndName(1000, "duplicati-b1234.dblock.zip.aes")
	if err != nil || got == nil || got.ID != "m-1234" {
		t.Fatalf("GetMediaByFolderAndName exacto falló: got=%+v err=%v", got, err)
	}
	miss, err := store.GetMediaByFolderAndName(1000, "DUPLICATI-B1234.DBLOCK.ZIP.AES")
	if err != nil || miss != nil {
		t.Fatalf("GetMediaByFolderAndName con distintas mayúsculas en modo BINARY debe devolver nil, got=%+v err=%v", miss, err)
	}

	gotF, err := store.GetFolderByParentAndName(100, "Sub_042")
	if err != nil || gotF == nil || gotF.ID != 1042 {
		t.Fatalf("GetFolderByParentAndName exacto falló: got=%+v err=%v", gotF, err)
	}
	missF, err := store.GetFolderByParentAndName(100, "sub_042")
	if err != nil || missF != nil {
		t.Fatalf("GetFolderByParentAndName con distintas mayúsculas en modo BINARY debe devolver nil, got=%+v err=%v", missF, err)
	}

	store.SetNameFolding(func() bool { return true })
	gotFNocase, err := store.GetFolderByParentAndName(100, "sub_042")
	if err != nil || gotFNocase == nil || gotFNocase.ID != 1042 {
		t.Fatalf("GetFolderByParentAndName en modo NOCASE falló: got=%+v err=%v", gotFNocase, err)
	}
}
