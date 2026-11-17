package auth

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
	"testing"

	utls "github.com/refraction-networking/utls"
)

func TestClientProfile_DefaultsAndOverrides(t *testing.T) {
	cases := []struct {
		profile, webSession, want string
	}{
		{"", "", ProfileDesktop},
		{"", "true", ProfileBrowser},
		{"browser", "", ProfileBrowser},
		{"desktop", "true", ProfileDesktop},
	}
	for _, c := range cases {
		t.Setenv("SAPI_CLIENT_PROFILE", c.profile)
		t.Setenv("WEB_SESSION", c.webSession)
		if got := ClientProfile(); got != c.want {
			t.Errorf("SAPI_CLIENT_PROFILE=%q WEB_SESSION=%q: %q, want %q", c.profile, c.webSession, got, c.want)
		}
	}
}

func TestDesktopProfile_UserAgentAndDeviceID(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	t.Setenv("SAPI_USER_AGENT", "")
	t.Setenv("DESKTOP_USER_AGENT", "")

	if got := GetSAPIUserAgent(); got != "omh windows client 4.0.1" {
		t.Errorf("GetSAPIUserAgent() = %q, want the official desktop client UA", got)
	}

	id := GenerateDeviceID()
	if !strings.HasPrefix(id, "fol-") {
		t.Fatalf("GenerateDeviceID() = %q, want fol- prefix", id)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(id, "fol-"))
	if err != nil || len(raw) != 16 {
		t.Errorf("GenerateDeviceID() = %q, want fol- + base64 of 16 bytes (err=%v, len=%d)", id, err, len(raw))
	}

	if got := SanitizeDeviceID("fol-xShd2lUKwcsWLi7YPhSfnA=="); got != "fol-xShd2lUKwcsWLi7YPhSfnA==" {
		t.Errorf("SanitizeDeviceID(fol-…) = %q, want unchanged", got)
	}
	a, b := SanitizeDeviceID("web-0123abcd"), SanitizeDeviceID("web-0123abcd")
	if !strings.HasPrefix(a, "fol-") || a != b {
		t.Errorf("SanitizeDeviceID(web-…) = %q / %q, want a stable fol- identifier", a, b)
	}

	header := BuildOAuthHeader("at", "rt", 1794879489187, "3600")
	decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "oauth "))
	if !strings.Contains(string(decoded), `"platform":"windows"`) {
		t.Errorf("Authorization payload = %s, want platform windows", decoded)
	}
}

func TestNewRequestID_IsUUIDv4(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if id := NewRequestID(); !re.MatchString(id) {
		t.Errorf("NewRequestID() = %q, want a UUID v4", id)
	}
}

func TestFingerprintedTransport_SAPIProtocolByProfile(t *testing.T) {
	type h1Reporter interface{ UsesHTTP1ForSAPI() bool }
	for profile, want := range map[string]bool{"desktop": true, "browser": false} {
		t.Setenv("SAPI_CLIENT_PROFILE", profile)
		rt, ok := NewFingerprintedTransport().(h1Reporter)
		if !ok {
			t.Fatalf("transport does not report its SAPI protocol")
		}
		if got := rt.UsesHTTP1ForSAPI(); got != want {
			t.Errorf("profile %s: UsesHTTP1ForSAPI() = %v, want %v", profile, got, want)
		}
	}
	var _ http.RoundTripper = NewFingerprintedTransport()
}

func TestHTTP1OnlySpec_AdvertisesOnlyHTTP11(t *testing.T) {
	for _, id := range []utls.ClientHelloID{utls.HelloChrome_Auto, utls.HelloFirefox_Auto, utls.HelloEdge_Auto} {
		spec, err := http1OnlySpec(id)
		if err != nil {
			t.Fatalf("%s: %v", id.Str(), err)
		}
		found := false
		for _, ext := range spec.Extensions {
			if alpn, ok := ext.(*utls.ALPNExtension); ok {
				found = true
				if len(alpn.AlpnProtocols) != 1 || alpn.AlpnProtocols[0] != "http/1.1" {
					t.Errorf("%s: ALPN = %v, want [http/1.1]", id.Str(), alpn.AlpnProtocols)
				}
			}
		}
		if !found {
			t.Errorf("%s: no ALPN extension in spec", id.Str())
		}
	}
}
