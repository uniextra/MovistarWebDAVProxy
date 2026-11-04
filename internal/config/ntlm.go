package config

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

const (
	ntlmFlagUnicode                 = 0x00000001
	ntlmFlagRequestTarget           = 0x00000002
	ntlmFlagNegotiateNTLM           = 0x00000200
	ntlmFlagNegotiateAlwaysSign     = 0x00008000
	ntlmFlagNegotiateExtendedSec    = 0x00080000
	ntlmFlagNegotiateTargetInfo     = 0x00800000
	ntlmFlagNegotiateVersion        = 0x02000000
	ntlmFlagTargetTypeServer        = 0x00020000
	ntlmChallengeTTL                = 2 * time.Minute
	ntlmAuthenticatedConnIdleExpiry = 15 * time.Minute
)

type ntlmChallengeState struct {
	challenge [8]byte
	createdAt time.Time
}

type ntlmConnAuthState struct {
	user     string
	lastSeen time.Time
}

// NTLMAuthenticator gestiona el handshake NTLMv2 sobre HTTP (Type 1 -> Type 2 -> Type 3)
// y mantiene el estado de autenticación por conexión TCP (RemoteAddr) según MS-NLMP / RFC 4559.
type NTLMAuthenticator struct {
	mu           sync.Mutex
	challenges   map[string]ntlmChallengeState
	ipChallenges map[string]ntlmChallengeState
	authedConns  map[string]ntlmConnAuthState
}

// NewNTLMAuthenticator crea una nueva instancia de NTLMAuthenticator.
func NewNTLMAuthenticator() *NTLMAuthenticator {
	return &NTLMAuthenticator{
		challenges:   make(map[string]ntlmChallengeState),
		ipChallenges: make(map[string]ntlmChallengeState),
		authedConns:  make(map[string]ntlmConnAuthState),
	}
}

// IsConnAuthenticated comprueba si la conexión TCP actual (identificada por r.RemoteAddr)
// ya completó satisfactoriamente el handshake NTLMv2.
func (n *NTLMAuthenticator) IsConnAuthenticated(remoteAddr string) (string, bool) {
	if n == nil || remoteAddr == "" {
		return "", false
	}
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()

	st, ok := n.authedConns[remoteAddr]
	if !ok {
		return "", false
	}
	if now.Sub(st.lastSeen) > ntlmAuthenticatedConnIdleExpiry {
		delete(n.authedConns, remoteAddr)
		return "", false
	}
	st.lastSeen = now
	n.authedConns[remoteAddr] = st
	return st.user, true
}

// InvalidateConn elimina el estado autenticado de una conexión TCP.
func (n *NTLMAuthenticator) InvalidateConn(remoteAddr string) {
	if n == nil || remoteAddr == "" {
		return
	}
	n.mu.Lock()
	delete(n.authedConns, remoteAddr)
	delete(n.challenges, remoteAddr)
	n.mu.Unlock()
}

// IsNTLMHeader indica si el valor de Authorization corresponde al esquema NTLM o Negotiate/NTLMSSP.
func IsNTLMHeader(authHeader string) bool {
	if authHeader == "" {
		return false
	}
	parts := strings.SplitN(strings.TrimSpace(authHeader), " ", 2)
	if len(parts) != 2 {
		return false
	}
	scheme := strings.ToUpper(parts[0])
	return scheme == "NTLM" || scheme == "NEGOTIATE"
}

// HandleHandshake procesa un mensaje Authorization: NTLM <base64>.
// Devuelve:
//   - challengeHeader: valor para WWW-Authenticate si es un mensaje Type 1 (Negotiate).
//   - isType1: true si era el paso 1 del handshake (no es un fallo de credenciales).
//   - rawUser: usuario extraído en el paso Type 3.
//   - authenticated: true si el mensaje Type 3 fue validado criptográficamente con NTLMv2.
func (n *NTLMAuthenticator) HandleHandshake(remoteAddr, clientIP, authHeader string, cfg AuthConfig) (challengeHeader string, isType1 bool, rawUser string, authenticated bool) {
	parts := strings.SplitN(strings.TrimSpace(authHeader), " ", 2)
	if len(parts) != 2 {
		return "", false, "", false
	}
	scheme := parts[0]
	token, err := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[1]))
	if err != nil || len(token) < 12 {
		return "", false, "", false
	}
	if string(token[:8]) != "NTLMSSP\x00" {
		return "", false, "", false
	}

	msgType := binary.LittleEndian.Uint32(token[8:12])
	now := time.Now()

	switch msgType {
	case 1:
		// Type 1: Negotiate -> Generar desafío de 8 bytes y devolver Type 2 (Challenge)
		var challenge [8]byte
		if _, err := rand.Read(challenge[:]); err != nil {
			return "", false, "", false
		}
		st := ntlmChallengeState{
			challenge: challenge,
			createdAt: now,
		}
		n.mu.Lock()
		n.cleanupLocked(now)
		n.challenges[remoteAddr] = st
		if clientIP != "" {
			n.ipChallenges[clientIP] = st
		}
		delete(n.authedConns, remoteAddr)
		n.mu.Unlock()

		type2 := buildNTLMType2Message(challenge)
		return scheme + " " + base64.StdEncoding.EncodeToString(type2), true, "", false

	case 3:
		// Type 3: Authenticate -> Verificar respuesta NTLMv2 contra el desafío emitido para este RemoteAddr (o IP)
		n.mu.Lock()
		chState, hasChallenge := n.challenges[remoteAddr]
		delete(n.challenges, remoteAddr)
		if !hasChallenge && clientIP != "" {
			chState, hasChallenge = n.ipChallenges[clientIP]
		}
		n.mu.Unlock()

		if !hasChallenge || now.Sub(chState.createdAt) > ntlmChallengeTTL {
			return "", false, "", false
		}

		domain, user, ntResponse, ok := parseNTLMType3Message(token)
		if !ok {
			return "", false, "", false
		}
		if domain != "" {
			rawUser = domain + `\` + user
		} else {
			rawUser = user
		}

		if verifyNTLMv2(cfg, domain, user, chState.challenge, ntResponse) {
			n.mu.Lock()
			n.authedConns[remoteAddr] = ntlmConnAuthState{
				user:     NormalizeUsername(user),
				lastSeen: now,
			}
			n.mu.Unlock()
			return "", false, rawUser, true
		}
		return "", false, rawUser, false

	default:
		return "", false, "", false
	}
}

func (n *NTLMAuthenticator) cleanupLocked(now time.Time) {
	for k, v := range n.challenges {
		if now.Sub(v.createdAt) > ntlmChallengeTTL {
			delete(n.challenges, k)
		}
	}
	for k, v := range n.ipChallenges {
		if now.Sub(v.createdAt) > ntlmChallengeTTL {
			delete(n.ipChallenges, k)
		}
	}
	for k, v := range n.authedConns {
		if now.Sub(v.lastSeen) > ntlmAuthenticatedConnIdleExpiry {
			delete(n.authedConns, k)
		}
	}
}

func buildNTLMType2Message(serverChallenge [8]byte) []byte {
	targetName := encodeUTF16LE("WEBDAV")
	targetInfo := buildNTLMTargetInfo()

	const headerLen = 56
	targetNameOffset := headerLen
	targetInfoOffset := headerLen + len(targetName)
	totalLen := targetInfoOffset + len(targetInfo)

	buf := make([]byte, totalLen)
	copy(buf[0:8], "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(buf[8:12], 2) // MessageType = 2 (Challenge)

	// TargetNameFields (offset 12..20)
	binary.LittleEndian.PutUint16(buf[12:14], uint16(len(targetName)))
	binary.LittleEndian.PutUint16(buf[14:16], uint16(len(targetName)))
	binary.LittleEndian.PutUint32(buf[16:20], uint32(targetNameOffset))

	// NegotiateFlags (offset 20..24)
	flags := uint32(ntlmFlagUnicode |
		ntlmFlagRequestTarget |
		ntlmFlagNegotiateNTLM |
		ntlmFlagNegotiateAlwaysSign |
		ntlmFlagNegotiateExtendedSec |
		ntlmFlagNegotiateTargetInfo |
		ntlmFlagTargetTypeServer |
		ntlmFlagNegotiateVersion)
	binary.LittleEndian.PutUint32(buf[20:24], flags)

	// ServerChallenge (offset 24..32)
	copy(buf[24:32], serverChallenge[:])

	// Reserved (offset 32..40) -> 0

	// TargetInfoFields (offset 40..48)
	binary.LittleEndian.PutUint16(buf[40:42], uint16(len(targetInfo)))
	binary.LittleEndian.PutUint16(buf[42:44], uint16(len(targetInfo)))
	binary.LittleEndian.PutUint32(buf[44:48], uint32(targetInfoOffset))

	// Version (offset 48..56): Windows 10/11 NTLM revision 15
	buf[48] = 10
	buf[49] = 0
	binary.LittleEndian.PutUint16(buf[50:52], 19041)
	buf[55] = 15

	copy(buf[targetNameOffset:], targetName)
	copy(buf[targetInfoOffset:], targetInfo)
	return buf
}

func buildNTLMTargetInfo() []byte {
	var out []byte
	appendAvPair := func(avID uint16, val []byte) {
		var hdr [4]byte
		binary.LittleEndian.PutUint16(hdr[0:2], avID)
		binary.LittleEndian.PutUint16(hdr[2:4], uint16(len(val)))
		out = append(out, hdr[:]...)
		out = append(out, val...)
	}

	nbName := encodeUTF16LE("WEBDAV")
	dnsName := encodeUTF16LE("webdav.local")
	appendAvPair(0x0002, nbName)  // MsvAvNbDomainName
	appendAvPair(0x0001, nbName)  // MsvAvNbComputerName
	appendAvPair(0x0004, dnsName) // MsvAvDnsDomainName
	appendAvPair(0x0003, dnsName) // MsvAvDnsComputerName

	// MsvAvTimestamp (0x0007): Windows FILETIME (100-ns intervals since Jan 1, 1601 UTC)
	ft := uint64(time.Now().UnixNano()/100) + 116444736000000000
	var ftBytes [8]byte
	binary.LittleEndian.PutUint64(ftBytes[:], ft)
	appendAvPair(0x0007, ftBytes[:])

	// MsvAvEOL (0x0000)
	appendAvPair(0x0000, nil)
	return out
}

func parseNTLMType3Message(msg []byte) (domain, user string, ntResponse []byte, ok bool) {
	if len(msg) < 52 {
		return "", "", nil, false
	}
	readSlice := func(fieldOffset int) ([]byte, bool) {
		length := int(binary.LittleEndian.Uint16(msg[fieldOffset : fieldOffset+2]))
		offset := int(binary.LittleEndian.Uint32(msg[fieldOffset+4 : fieldOffset+8]))
		if length == 0 {
			return nil, true
		}
		if offset < 0 || length < 0 || offset+length > len(msg) {
			return nil, false
		}
		return msg[offset : offset+length], true
	}

	ntResp, ok1 := readSlice(20)
	domBytes, ok2 := readSlice(28)
	usrBytes, ok3 := readSlice(36)
	if !ok1 || !ok2 || !ok3 {
		return "", "", nil, false
	}
	return decodeUTF16LE(domBytes), decodeUTF16LE(usrBytes), ntResp, true
}

func verifyNTLMv2(cfg AuthConfig, clientDomain, clientUser string, serverChallenge [8]byte, ntResponse []byte) bool {
	if !cfg.IsEnabled() || cfg.Password == "" {
		return false
	}
	if len(ntResponse) < 24 {
		return false
	}

	cleanUser := NormalizeUsername(clientUser)
	expectedUser := NormalizeUsername(cfg.Username)
	if expectedUser == "" {
		return false
	}
	if !strings.Contains(expectedUser, "@") {
		if idx := strings.Index(cleanUser, "@"); idx != -1 {
			cleanUser = cleanUser[:idx]
		}
	}
	if !strings.EqualFold(cleanUser, expectedUser) {
		return false
	}

	ntHash := md4Sum(encodeUTF16LE(cfg.Password))
	ntProofStr := ntResponse[:16]
	clientBlob := ntResponse[16:]

	// MS-NLMP §3.3.2: ResponseKeyNT = HMAC_MD5(NTOWFv2, UpperCase(User) + UserDom)
	candidates := []string{
		strings.ToUpper(clientUser) + clientDomain,
		strings.ToUpper(clientUser) + strings.ToUpper(clientDomain),
		strings.ToUpper(clientUser) + strings.ToLower(clientDomain),
		strings.ToUpper(clientUser),
		strings.ToUpper(cleanUser) + clientDomain,
		strings.ToUpper(cleanUser),
	}

	payload := make([]byte, 8+len(clientBlob))
	copy(payload[0:8], serverChallenge[:])
	copy(payload[8:], clientBlob)

	for _, ident := range candidates {
		h1 := hmac.New(md5.New, ntHash[:])
		h1.Write(encodeUTF16LE(ident))
		responseKeyNT := h1.Sum(nil)

		h2 := hmac.New(md5.New, responseKeyNT)
		h2.Write(payload)
		expectedProof := h2.Sum(nil)

		if subtle.ConstantTimeCompare(ntProofStr, expectedProof) == 1 {
			return true
		}
	}
	return false
}

func encodeUTF16LE(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	buf := make([]byte, len(u16)*2)
	for i, v := range u16 {
		binary.LittleEndian.PutUint16(buf[i*2:], v)
	}
	return buf
}

func decodeUTF16LE(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	u16 := make([]uint16, len(b)/2)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u16))
}

// md4Sum calcula el digest MD4 (RFC 1320) requerido por NTOWFv2 (NT Hash = MD4(UTF-16LE(password))).
func md4Sum(data []byte) [16]byte {
	var a uint32 = 0x67452301
	var b uint32 = 0xefcdab89
	var c uint32 = 0x98badcfe
	var d uint32 = 0x10325476

	origLen := len(data)
	bitLen := uint64(origLen) * 8

	padded := make([]byte, origLen, ((origLen+9+63)/64)*64)
	copy(padded, data)
	padded = append(padded, 0x80)
	for (len(padded)+8)%64 != 0 {
		padded = append(padded, 0x00)
	}
	var lenBytes [8]byte
	binary.LittleEndian.PutUint64(lenBytes[:], bitLen)
	padded = append(padded, lenBytes[:]...)

	rotl := func(x, n uint32) uint32 {
		return (x << n) | (x >> (32 - n))
	}
	f := func(x, y, z uint32) uint32 { return (x & y) | (^x & z) }
	g := func(x, y, z uint32) uint32 { return (x & y) | (x & z) | (y & z) }
	h := func(x, y, z uint32) uint32 { return x ^ y ^ z }

	var x [16]uint32
	for i := 0; i < len(padded); i += 64 {
		for j := 0; j < 16; j++ {
			x[j] = binary.LittleEndian.Uint32(padded[i+j*4:])
		}
		aa, bb, cc, dd := a, b, c, d

		// Ronda 1
		s1 := [4]uint32{3, 7, 11, 19}
		for j := 0; j < 16; j++ {
			t := a + f(b, c, d) + x[j]
			a, d, c, b = d, c, b, rotl(t, s1[j%4])
		}

		// Ronda 2
		idx2 := [16]int{0, 4, 8, 12, 1, 5, 9, 13, 2, 6, 10, 14, 3, 7, 11, 15}
		s2 := [4]uint32{3, 5, 9, 13}
		for j := 0; j < 16; j++ {
			t := a + g(b, c, d) + x[idx2[j]] + 0x5a827999
			a, d, c, b = d, c, b, rotl(t, s2[j%4])
		}

		// Ronda 3
		idx3 := [16]int{0, 8, 4, 12, 2, 10, 6, 14, 1, 9, 5, 13, 3, 11, 7, 15}
		s3 := [4]uint32{3, 9, 11, 15}
		for j := 0; j < 16; j++ {
			t := a + h(b, c, d) + x[idx3[j]] + 0x6ed9eba1
			a, d, c, b = d, c, b, rotl(t, s3[j%4])
		}

		a += aa
		b += bb
		c += cc
		d += dd
	}

	var out [16]byte
	binary.LittleEndian.PutUint32(out[0:4], a)
	binary.LittleEndian.PutUint32(out[4:8], b)
	binary.LittleEndian.PutUint32(out[8:12], c)
	binary.LittleEndian.PutUint32(out[12:16], d)
	return out
}
