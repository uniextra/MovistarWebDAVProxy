package config

import (
	"crypto/hmac"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

func TestMD4_RFC1320Vectors(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "31d6cfe0d16ae931b73c59d7e0c089c0"},
		{"a", "bde52cb31de33e46245e05fbdbd6fb24"},
		{"abc", "a448017aaf21d8525fc10ae87aa6729d"},
		{"message digest", "d9130a8164549fe818874806e1c7014b"},
		{"abcdefghijklmnopqrstuvwxyz", "d79e1c308aa5bbcdeea8ed63df412da9"},
	}
	for _, tc := range tests {
		sum := md4Sum([]byte(tc.input))
		got := hex.EncodeToString(sum[:])
		if got != tc.want {
			t.Errorf("md4(%q) = %s, want %s", tc.input, got, tc.want)
		}
	}
}

func TestNTLMHandshake_Type1Type2Type3(t *testing.T) {
	cfg := AuthConfig{
		Username: "gauntlet",
		Password: "1234758+-",
	}
	auth := NewNTLMAuthenticator()
	remoteAddr := "192.168.1.34:54321"
	clientIP := "192.168.1.34"

	// 1. Build Type 1 (Negotiate)
	type1 := make([]byte, 32)
	copy(type1[0:8], "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(type1[8:12], 1)
	binary.LittleEndian.PutUint32(type1[12:16], ntlmFlagUnicode|ntlmFlagNegotiateNTLM|ntlmFlagNegotiateExtendedSec)
	type1Header := "NTLM " + base64.StdEncoding.EncodeToString(type1)

	chHeader, isType1, _, ok := auth.HandleHandshake(remoteAddr, clientIP, type1Header, cfg)
	if !isType1 || ok || !strings.HasPrefix(chHeader, "NTLM ") {
		t.Fatalf("expected Type 2 challenge, got isType1=%v ok=%v chHeader=%q", isType1, ok, chHeader)
	}

	// 2. Decode Type 2 (Challenge) and extract 8-byte ServerChallenge
	type2Bytes, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(chHeader, "NTLM "))
	if err != nil || len(type2Bytes) < 32 {
		t.Fatalf("invalid Type 2 bytes: %v (len=%d)", err, len(type2Bytes))
	}
	serverChallenge := type2Bytes[24:32]

	// 3. Build Type 3 (Authenticate) with valid NTLMv2 response
	domain := "DESKTOP-WIN11"
	user := "gauntlet"
	ntHash := md4Sum(encodeUTF16LE(cfg.Password))

	h1 := hmac.New(md5.New, ntHash[:])
	h1.Write(encodeUTF16LE(strings.ToUpper(user) + domain))
	responseKeyNT := h1.Sum(nil)

	clientBlob := []byte{0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xaa, 0xbb, 0xcc, 0xdd, 0x00, 0x00, 0x00, 0x00}
	h2 := hmac.New(md5.New, responseKeyNT)
	h2.Write(serverChallenge)
	h2.Write(clientBlob)
	ntProof := h2.Sum(nil)

	ntChallengeResp := append(ntProof, clientBlob...)
	domBytes := encodeUTF16LE(domain)
	usrBytes := encodeUTF16LE(user)

	const hdrLen = 64
	domOff := hdrLen
	usrOff := domOff + len(domBytes)
	ntOff := usrOff + len(usrBytes)
	type3 := make([]byte, ntOff+len(ntChallengeResp))
	copy(type3[0:8], "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(type3[8:12], 3)

	// NtChallengeResponseFields (20..28)
	binary.LittleEndian.PutUint16(type3[20:22], uint16(len(ntChallengeResp)))
	binary.LittleEndian.PutUint16(type3[22:24], uint16(len(ntChallengeResp)))
	binary.LittleEndian.PutUint32(type3[24:28], uint32(ntOff))

	// DomainNameFields (28..36)
	binary.LittleEndian.PutUint16(type3[28:30], uint16(len(domBytes)))
	binary.LittleEndian.PutUint16(type3[30:32], uint16(len(domBytes)))
	binary.LittleEndian.PutUint32(type3[32:36], uint32(domOff))

	// UserNameFields (36..44)
	binary.LittleEndian.PutUint16(type3[36:38], uint16(len(usrBytes)))
	binary.LittleEndian.PutUint16(type3[38:40], uint16(len(usrBytes)))
	binary.LittleEndian.PutUint32(type3[40:44], uint32(usrOff))

	copy(type3[domOff:], domBytes)
	copy(type3[usrOff:], usrBytes)
	copy(type3[ntOff:], ntChallengeResp)

	type3Header := "NTLM " + base64.StdEncoding.EncodeToString(type3)
	_, isType1, rawUser, ok := auth.HandleHandshake(remoteAddr, clientIP, type3Header, cfg)
	if isType1 || !ok {
		t.Fatalf("expected Type 3 authentication success, got isType1=%v ok=%v rawUser=%q", isType1, ok, rawUser)
	}
	if _, authed := auth.IsConnAuthenticated(remoteAddr); !authed {
		t.Fatalf("expected connection %s to remain authenticated", remoteAddr)
	}
}
