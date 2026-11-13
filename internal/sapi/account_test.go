package sapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccountSettings_FindsKeysInAnyScope(t *testing.T) {
	info := map[string]interface{}{
		"data": map[string]interface{}{
			"sapi.upload.max-size-in-mb":                 4096.0,
			"folders.softdelete-updates-limit":           "1000",
			"trash.cleanup.items-expire-time-in-minutes": "43200",
			"logininfo": map[string]interface{}{"sapi.login.persistent-login-cookie.enabled": "true"},
			"otra.cosa": 1,
		},
		"folders.move-updates-limit": "500",
	}
	got := AccountSettings(info)
	want := map[string]interface{}{
		"sapi.upload.max-size-in-mb":                 4096.0,
		"folders.softdelete-updates-limit":           "1000",
		"folders.move-updates-limit":                 "500",
		"trash.cleanup.items-expire-time-in-minutes": "43200",
		"sapi.login.persistent-login-cookie.enabled": "true",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, esperado %v", k, got[k], v)
		}
	}
}

func TestEmptyTrash_WebCall(t *testing.T) {
	for _, c := range []struct {
		body    string
		wantErr bool
	}{{`{"success":"ok"}`, false}, {`{"error":{"code":"MED-1000","message":"x"}}`, true}} {
		var path string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path = r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery
			_, _ = io.WriteString(w, c.body)
		}))
		cl := newWebTestClient(t, srv.URL)
		err := cl.EmptyTrash()
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v", c.body, err)
		}
		if path != "POST /sapi/media/trash?action=empty" {
			t.Errorf("llamada %q", path)
		}
		srv.Close()
	}
}
