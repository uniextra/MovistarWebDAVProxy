package sapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestOfficialHeartbeat_CallsLikeTheApp(t *testing.T) {
	client, calls := captureCalls(t, func(capturedCall) string { return `{"data":{}}` })
	if err := client.OfficialMinuteBeat(); err != nil {
		t.Fatal(err)
	}
	if err := client.OfficialFiveMinuteBeat(); err != nil {
		t.Fatal(err)
	}
	got := calls()
	want := []string{
		"/sapi/system/information?action=get",
		"/sapi/subscription/plan?action=get",
		"/sapi/subscription?action=get",
		"/sapi/profile/role?action=get",
		"/sapi/profile/client?action=get-update-info&component=windows",
		"/sapi/media?action=get-storage-space&softdeleted=true",
	}
	if len(got) != len(want) {
		t.Fatalf("llamadas = %+v", got)
	}
	for i, w := range want {
		c := got[i]
		p, q, _ := strings.Cut(w, "?")
		if c.Method != http.MethodGet || c.Path != p {
			t.Errorf("llamada %d = %s %s, esperado GET %s", i, c.Method, c.Path, p)
		}
		for _, kv := range strings.Split(q, "&") {
			if !strings.Contains(c.Query, kv) {
				t.Errorf("llamada %d: falta %q en %q", i, kv, c.Query)
			}
		}
		hasVK := strings.Contains(c.Query, "validationkey=")
		if (p == "/sapi/system/information") == hasVK {
			t.Errorf("llamada %d (%s): validationkey presente=%v (la app solo la omite en system/information)", i, p, hasVK)
		}
	}
}
