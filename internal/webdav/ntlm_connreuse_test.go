package webdav

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"unicode/utf16"

	"movistar-webdav-proxy/internal/config"
)

// --- helpers de cliente NTLMv2 (copiados de la lógica del propio proxy) ---

func encUTF16LE(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	buf := make([]byte, len(u16)*2)
	for i, v := range u16 {
		binary.LittleEndian.PutUint16(buf[i*2:], v)
	}
	return buf
}

func md4(data []byte) [16]byte {
	var a, b, c, d uint32 = 0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476
	bitLen := uint64(len(data)) * 8
	padded := append([]byte{}, data...)
	padded = append(padded, 0x80)
	for (len(padded)+8)%64 != 0 {
		padded = append(padded, 0x00)
	}
	var lb [8]byte
	binary.LittleEndian.PutUint64(lb[:], bitLen)
	padded = append(padded, lb[:]...)
	rotl := func(x, n uint32) uint32 { return (x << n) | (x >> (32 - n)) }
	f := func(x, y, z uint32) uint32 { return (x & y) | (^x & z) }
	g := func(x, y, z uint32) uint32 { return (x & y) | (x & z) | (y & z) }
	h := func(x, y, z uint32) uint32 { return x ^ y ^ z }
	var x [16]uint32
	for i := 0; i < len(padded); i += 64 {
		for j := 0; j < 16; j++ {
			x[j] = binary.LittleEndian.Uint32(padded[i+j*4:])
		}
		aa, bb, cc, dd := a, b, c, d
		s1 := [4]uint32{3, 7, 11, 19}
		for j := 0; j < 16; j++ {
			t := a + f(b, c, d) + x[j]
			a, d, c, b = d, c, b, rotl(t, s1[j%4])
		}
		idx2 := [16]int{0, 4, 8, 12, 1, 5, 9, 13, 2, 6, 10, 14, 3, 7, 11, 15}
		s2 := [4]uint32{3, 5, 9, 13}
		for j := 0; j < 16; j++ {
			t := a + g(b, c, d) + x[idx2[j]] + 0x5a827999
			a, d, c, b = d, c, b, rotl(t, s2[j%4])
		}
		idx3 := [16]int{0, 8, 4, 12, 2, 10, 6, 14, 1, 9, 5, 13, 3, 11, 7, 15}
		s3 := [4]uint32{3, 9, 11, 15}
		for j := 0; j < 16; j++ {
			t := a + h(b, c, d) + x[idx3[j]] + 0x6ed9eba1
			a, d, c, b = d, c, b, rotl(t, s3[j%4])
		}
		a, b, c, d = a+aa, b+bb, c+cc, d+dd
	}
	var out [16]byte
	binary.LittleEndian.PutUint32(out[0:4], a)
	binary.LittleEndian.PutUint32(out[4:8], b)
	binary.LittleEndian.PutUint32(out[8:12], c)
	binary.LittleEndian.PutUint32(out[12:16], d)
	return out
}

func ntlmType1Header() string {
	t1 := make([]byte, 32)
	copy(t1[0:8], "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(t1[8:12], 1)
	binary.LittleEndian.PutUint32(t1[12:16], 0x00080201) // Unicode|NTLM|ExtendedSec
	return "NTLM " + base64.StdEncoding.EncodeToString(t1)
}

func ntlmType3Header(t *testing.T, serverChallenge []byte, domain, user, pass string) string {
	t.Helper()
	ntHash := md4(encUTF16LE(pass))
	h1 := hmac.New(md5.New, ntHash[:])
	h1.Write(encUTF16LE(strings.ToUpper(user) + domain))
	responseKeyNT := h1.Sum(nil)
	clientBlob := []byte{0x01, 0x01, 0, 0, 0, 0, 0, 0, 0xaa, 0xbb, 0xcc, 0xdd, 0, 0, 0, 0}
	h2 := hmac.New(md5.New, responseKeyNT)
	h2.Write(serverChallenge)
	h2.Write(clientBlob)
	ntResp := append(h2.Sum(nil), clientBlob...)
	domB, usrB := encUTF16LE(domain), encUTF16LE(user)
	const hdr = 64
	domOff, usrOff := hdr, hdr+len(domB)
	ntOff := usrOff + len(usrB)
	m := make([]byte, ntOff+len(ntResp))
	copy(m[0:8], "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(m[8:12], 3)
	binary.LittleEndian.PutUint16(m[20:22], uint16(len(ntResp)))
	binary.LittleEndian.PutUint16(m[22:24], uint16(len(ntResp)))
	binary.LittleEndian.PutUint32(m[24:28], uint32(ntOff))
	binary.LittleEndian.PutUint16(m[28:30], uint16(len(domB)))
	binary.LittleEndian.PutUint16(m[30:32], uint16(len(domB)))
	binary.LittleEndian.PutUint32(m[32:36], uint32(domOff))
	binary.LittleEndian.PutUint16(m[36:38], uint16(len(usrB)))
	binary.LittleEndian.PutUint16(m[38:40], uint16(len(usrB)))
	binary.LittleEndian.PutUint32(m[40:44], uint32(usrOff))
	copy(m[domOff:], domB)
	copy(m[usrOff:], usrB)
	copy(m[ntOff:], ntResp)
	return "NTLM " + base64.StdEncoding.EncodeToString(m)
}

// doNTLMLogin completa el handshake (Type1 -> Type2 -> Type3) a través de ServeHTTP
// desde la dirección remota dada, y falla si el proxy no la deja autenticada.
func doNTLMLogin(t *testing.T, srv *Server, remoteAddr, xff, connID, user, pass string) {
	t.Helper()
	req1 := httptest.NewRequest("PROPFIND", "/", nil)
	req1.RemoteAddr = remoteAddr
	if xff != "" {
		req1.Header.Set("X-Forwarded-For", xff)
	}
	req1.Header.Set("Authorization", ntlmType1Header())
	if connID != "" {
		req1 = req1.WithContext(WithConnID(req1.Context(), connID))
	}
	rec1 := httptest.NewRecorder()
	srv.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusUnauthorized {
		t.Fatalf("Type1: esperado 401 con reto, obtenido %d", rec1.Code)
	}
	var chal []byte
	for _, v := range rec1.Header().Values("WWW-Authenticate") {
		if strings.HasPrefix(v, "NTLM ") {
			b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, "NTLM "))
			if err == nil && len(b) >= 32 {
				chal = b[24:32]
			}
		}
	}
	if chal == nil {
		t.Fatalf("Type2: no se recibió reto NTLM")
	}
	req2 := httptest.NewRequest("PROPFIND", "/", nil)
	req2.RemoteAddr = remoteAddr
	req2.Header.Set("Depth", "0")
	if xff != "" {
		req2.Header.Set("X-Forwarded-For", xff)
	}
	req2.Header.Set("Authorization", ntlmType3Header(t, chal, "DESKTOP-WIN11", user, pass))
	if connID != "" {
		req2 = req2.WithContext(WithConnID(req2.Context(), connID))
	}
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusMultiStatus {
		t.Fatalf("Type3: esperado 207 (login correcto), obtenido %d: %s", rec2.Code, rec2.Body.String())
	}
}

// unauthRequest envía una petición SIN credenciales desde remoteAddr y devuelve el código.
func unauthRequest(srv *Server, remoteAddr, xff, connID string) int {
	req := httptest.NewRequest("PROPFIND", "/", nil)
	req.RemoteAddr = remoteAddr
	req.Header.Set("Depth", "0")
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	if connID != "" {
		req = req.WithContext(WithConnID(req.Context(), connID))
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Code
}

func md4SelfCheck(t *testing.T) {
	if got := hex.EncodeToString(func() []byte { s := md4([]byte("abc")); return s[:] }()); got != "a448017aaf21d8525fc10ae87aa6729d" {
		t.Fatalf("md4 de prueba incorrecto: %s", got)
	}
}

const (
	testUser = "backup"
	testPass = "S3cr3t-O2!"
)

func newAuthServer(t *testing.T) *Server {
	t.Helper()
	srv, cleanup := setupTestWebDAVServer(t, config.AuthConfig{Username: testUser, Password: testPass})
	t.Cleanup(cleanup)
	return srv
}

// Referencia: sin credenciales desde una conexión nueva, el proxy exige autenticación.
func TestNTLM_BaselineUnauthenticatedIsRejected(t *testing.T) {
	md4SelfCheck(t)
	srv := newAuthServer(t)
	if code := unauthRequest(srv, "203.0.113.9:40000", "", "conn-0"); code != http.StatusUnauthorized {
		t.Fatalf("esperado 401 sin credenciales, obtenido %d", code)
	}
}

// Caso legítimo: tras el login NTLM, la MISMA conexión sigue sirviendo sin repetir
// credenciales. Debe seguir funcionando con el arreglo.
func TestNTLM_LoginThenSameConnServed(t *testing.T) {
	md4SelfCheck(t)
	srv := newAuthServer(t)
	addr := "198.51.100.5:51000"
	doNTLMLogin(t, srv, addr, "", "conn-A", testUser, testPass)
	if code := unauthRequest(srv, addr, "", "conn-A"); code != http.StatusMultiStatus {
		t.Fatalf("misma conexión tras login: esperado 207, obtenido %d", code)
	}
}

// BYPASS en v2.2.1 (el estado se guarda por IP:puerto): otra conexión TCP que reutiliza
// el mismo RemoteAddr (puerto efímero reciclado, o socket upstream reutilizado por un
// proxy inverso) hereda la autenticación. Con el arreglo, al ligarse a la conexión real,
// una conexión distinta debe exigir credenciales.
func TestNTLM_ReusedRemoteAddrIsRejected(t *testing.T) {
	md4SelfCheck(t)
	srv := newAuthServer(t)
	addr := "198.51.100.5:51000"
	doNTLMLogin(t, srv, addr, "", "conn-A", testUser, testPass)
	// Nueva conexión (otro id) con el MISMO RemoteAddr, sin credenciales.
	if code := unauthRequest(srv, addr, "", "conn-B"); code != http.StatusUnauthorized {
		t.Fatalf("BYPASS: una conexión nueva que reutiliza el RemoteAddr autenticado fue aceptada (código %d); debería ser 401", code)
	}
}

// Tras un proxy inverso de confianza, una sola conexión upstream sirve a muchos
// usuarios: la autenticación NTLM por conexión no puede confiarse. El atacante llega por
// el mismo socket del proxy (incluso el mismo id) con otra IP real y sin credenciales.
func TestNTLM_BehindTrustedProxyNotTrusted(t *testing.T) {
	md4SelfCheck(t)
	os.Setenv("TRUSTED_PROXIES", "203.0.113.7")
	t.Cleanup(func() { os.Unsetenv("TRUSTED_PROXIES") })
	srv := newAuthServer(t)
	proxyConn := "203.0.113.7:44444"
	doNTLMLogin(t, srv, proxyConn, "10.0.0.50", "pool-1", testUser, testPass) // víctima
	if code := unauthRequest(srv, proxyConn, "10.0.0.99", "pool-1"); code != http.StatusUnauthorized {
		t.Fatalf("BYPASS tras proxy inverso: petición sin credenciales por la conexión agrupada aceptada (código %d); debería ser 401", code)
	}
}

// Al cerrarse la conexión TCP, su autenticación NTLM se invalida (ConnState -> InvalidateConn):
// una petición posterior con el mismo id de conexión ya no se sirve.
func TestNTLM_AuthInvalidatedOnConnClose(t *testing.T) {
	md4SelfCheck(t)
	srv := newAuthServer(t)
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	ctx := srv.ConnContext(context.Background(), c1)
	id := connIDFromContext(ctx)
	if id == "" {
		t.Fatal("ConnContext no asignó id de conexión")
	}
	addr := "198.51.100.8:52000"
	doNTLMLogin(t, srv, addr, "", id, testUser, testPass)
	if code := unauthRequest(srv, addr, "", id); code != http.StatusMultiStatus {
		t.Fatalf("antes de cerrar: esperado 207, obtenido %d", code)
	}
	srv.ConnState(c1, http.StateClosed)
	if code := unauthRequest(srv, addr, "", id); code != http.StatusUnauthorized {
		t.Fatalf("tras cerrar la conexión: esperado 401, obtenido %d", code)
	}
}

// La función que añade v2.2.1 es la reconexión silenciosa de unidades de red Windows.
// Esto simula ese caso: el cliente autentica, su conexión se cierra (reinicio/caducidad)
// y en una conexión NUEVA vuelve a autenticar solo (con credenciales en caché) y se le sirve.
func TestNTLM_SilentReconnectStillWorks(t *testing.T) {
	md4SelfCheck(t)
	srv := newAuthServer(t)
	addr := "192.168.1.50:49500"

	c1, _ := net.Pipe()
	id1 := connIDFromContext(srv.ConnContext(context.Background(), c1))
	doNTLMLogin(t, srv, addr, "", id1, testUser, testPass)
	if code := unauthRequest(srv, addr, "", id1); code != http.StatusMultiStatus {
		t.Fatalf("conexión 1: esperado 207, obtenido %d", code)
	}
	srv.ConnState(c1, http.StateClosed) // cierre de la conexión (p. ej. reinicio del PC)

	// Reconexión: conexión TCP nueva, nuevo handshake automático -> debe servirse.
	c2, _ := net.Pipe()
	id2 := connIDFromContext(srv.ConnContext(context.Background(), c2))
	doNTLMLogin(t, srv, addr, "", id2, testUser, testPass)
	if code := unauthRequest(srv, addr, "", id2); code != http.StatusMultiStatus {
		t.Fatalf("reconexión: esperado 207 (reconexión silenciosa), obtenido %d", code)
	}
}

// Sin proxy inverso configurado (TRUSTED_PROXIES vacío), el comportamiento NTLM normal
// no cambia: un cliente directo autentica y su conexión sigue sirviendo.
func TestNTLM_NoProxyBehaviourUnchanged(t *testing.T) {
	md4SelfCheck(t)
	os.Unsetenv("TRUSTED_PROXIES")
	srv := newAuthServer(t)
	addr := "192.168.1.77:50100"
	doNTLMLogin(t, srv, addr, "", "direct-1", testUser, testPass)
	if code := unauthRequest(srv, addr, "", "direct-1"); code != http.StatusMultiStatus {
		t.Fatalf("cliente directo: esperado 207, obtenido %d", code)
	}
}
