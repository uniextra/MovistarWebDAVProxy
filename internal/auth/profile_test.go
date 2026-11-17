package auth

import (
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
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

func TestClientHelloIDFromEnv_DefaultsByProfile(t *testing.T) {
	t.Setenv("UTLS_CLIENT_HELLO", "")

	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	if got := clientHelloIDFromEnv(); got != HelloO2Windows {
		t.Errorf("desktop default ClientHello = %v, want HelloO2Windows", got)
	}

	t.Setenv("SAPI_CLIENT_PROFILE", "browser")
	if got := clientHelloIDFromEnv(); got != utls.HelloChrome_Auto {
		t.Errorf("browser default ClientHello = %v, want HelloChrome_Auto", got)
	}

	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	t.Setenv("UTLS_CLIENT_HELLO", "chrome_auto")
	if got := clientHelloIDFromEnv(); got != utls.HelloChrome_Auto {
		t.Errorf("explicit UTLS_CLIENT_HELLO=chrome_auto = %v, want HelloChrome_Auto", got)
	}
}

// ja3 calcula la cadena JA3 de un ClientHello (mensaje handshake sin cabecera de registro).
func ja3(t *testing.T, h []byte) (string, int) {
	t.Helper()
	u16 := func(b []byte) int { return int(b[0])<<8 | int(b[1]) }
	b := h[4:]
	parts := []string{strconv.Itoa(u16(b))}
	i := 2 + 32
	i += 1 + int(b[i])
	cl := u16(b[i:])
	var cs []string
	for j := 0; j < cl; j += 2 {
		cs = append(cs, strconv.Itoa(u16(b[i+2+j:])))
	}
	i += 2 + cl
	i += 1 + int(b[i])
	end := i + 2 + u16(b[i:])
	i += 2
	var exts, groups, pf []string
	alpn := 0
	for i < end {
		typ, l := u16(b[i:]), u16(b[i+2:])
		d := b[i+4 : i+4+l]
		exts = append(exts, strconv.Itoa(typ))
		switch typ {
		case 10:
			for j := 2; j < len(d); j += 2 {
				groups = append(groups, strconv.Itoa(u16(d[j:])))
			}
		case 11:
			for _, p := range d[1:] {
				pf = append(pf, strconv.Itoa(int(p)))
			}
		case 16:
			alpn++
		}
		i += 4 + l
	}
	parts = append(parts, strings.Join(cs, "-"), strings.Join(exts, "-"), strings.Join(groups, "-"), strings.Join(pf, "-"))
	return strings.Join(parts, ","), alpn
}

// El ClientHello del perfil desktop debe ser el del cliente oficial capturado sin MITM.
func TestO2WindowsHello_MatchesOfficialClientCapture(t *testing.T) {
	t.Setenv("SAPI_CLIENT_PROFILE", "desktop")
	t.Setenv("UTLS_CLIENT_HELLO", "")
	if id := clientHelloIDFromEnv(); id != HelloO2Windows {
		t.Fatalf("huella por defecto en desktop = %s, want O2Windows", id.Str())
	}
	spec, err := http1OnlySpec(HelloO2Windows)
	if err != nil {
		t.Fatal(err)
	}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	u := utls.UClient(c1, &utls.Config{ServerName: "cloud.o2online.es"}, utls.HelloCustom)
	if err := u.ApplyPreset(&spec); err != nil {
		t.Fatal(err)
	}
	if err := u.BuildHandshakeState(); err != nil {
		t.Fatal(err)
	}
	raw := u.HandshakeState.Hello.Raw
	s, alpn := ja3(t, raw)
	if got := fmt.Sprintf("%x", md5.Sum([]byte(s))); got != "57fbe0aefee44901190849b0e877a5e1" {
		t.Fatalf("JA3 = %s (%s), want 57fbe0aefee44901190849b0e877a5e1", got, s)
	}
	if alpn != 0 {
		t.Fatal("el cliente oficial no envía ALPN")
	}
	if len(raw) != 512 {
		t.Fatalf("longitud del ClientHello = %d, want 512 (relleno de OpenSSL)", len(raw))
	}
	t.Setenv("SAPI_CLIENT_PROFILE", "browser")
	if id := clientHelloIDFromEnv(); id != utls.HelloChrome_Auto {
		t.Fatalf("perfil browser: huella = %s, want Chrome", id.Str())
	}
}
