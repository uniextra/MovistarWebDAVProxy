package auth

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrimeSAPILoginSessionSharesJSessionIDWithSAPILogin(t *testing.T) {
	const jSessionID = "primed-session"

	var loginCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ui/html/clientoauth.html":
			http.SetCookie(w, &http.Cookie{
				Name:  "JSESSIONID",
				Value: jSessionID,
				Path:  "/",
			})
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html></html>"))
		case "/sapi/login/oauth":
			if cookie, err := r.Cookie("JSESSIONID"); err == nil {
				loginCookie = cookie.Value
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"validationkey":"validation-value","responsetime":123}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	primed, err := primeSAPILoginSession(client, server.URL+"/ui/html/clientoauth.html")
	if err != nil {
		t.Fatalf("primeSAPILoginSession: %v", err)
	}
	if primed.JSessionID != jSessionID {
		t.Fatalf("primed JSESSIONID = %q, want %q", primed.JSessionID, jSessionID)
	}

	result := SAPILoginOAuth(
		client,
		server.URL+"/sapi",
		"access-token",
		"refresh-token",
		"device-id",
		"PC Windows",
		123,
		"3600",
	)
	if result.StatusCode != http.StatusOK {
		t.Fatalf("SAPILoginOAuth status = %d, want %d", result.StatusCode, http.StatusOK)
	}
	if loginCookie != jSessionID {
		t.Fatalf("login received JSESSIONID %q, want %q", loginCookie, jSessionID)
	}
}

func TestPrimeSAPILoginSessionCapturesValidationKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "session", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "validationKey", Value: "validation-key", Path: "/"})
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer server.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	primed, err := primeSAPILoginSession(&http.Client{Jar: jar}, server.URL+"/ui/html/clientoauth.html?code=test&state=test")
	if err != nil {
		t.Fatalf("primeSAPILoginSession: %v", err)
	}
	if primed.JSessionID != "session" {
		t.Errorf("JSessionID = %q", primed.JSessionID)
	}
	if primed.ValidationKey != "validation-key" {
		t.Errorf("ValidationKey = %q", primed.ValidationKey)
	}
}

func TestSAPILoginOAuthCapturesSafeDiagnostics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.Header().Set("Server", "CloudFront")
		w.Header().Set("X-Cache", "Error from cloudfront")
		w.Header().Set("X-Amz-Cf-Id", "cloudfront-request-id")
		w.Header().Set("WWW-Authenticate", `OAuth realm="Movistar"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`<html><title>Request blocked</title><body>access-token refresh-token</body></html>`))
	}))
	defer server.Close()

	result := SAPILoginOAuth(
		server.Client(),
		server.URL+"/sapi",
		"access-token",
		"refresh-token",
		"device-id",
		"PC Windows",
		123,
		"3600",
	)

	if result.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", result.StatusCode, http.StatusUnauthorized)
	}
	if result.ContentType != "text/html; charset=UTF-8" {
		t.Errorf("ContentType = %q", result.ContentType)
	}
	if result.Server != "CloudFront" {
		t.Errorf("Server = %q", result.Server)
	}
	if result.XCache != "Error from cloudfront" {
		t.Errorf("XCache = %q", result.XCache)
	}
	if result.CloudFrontRequestID != "cloudfront-request-id" {
		t.Errorf("CloudFrontRequestID = %q", result.CloudFrontRequestID)
	}
	if result.WWWAuthenticate != `OAuth realm="Movistar"` {
		t.Errorf("WWWAuthenticate = %q", result.WWWAuthenticate)
	}
	if !strings.Contains(result.BodyPreview, "Request blocked") {
		t.Errorf("BodyPreview does not contain response title: %q", result.BodyPreview)
	}
	if strings.Contains(result.BodyPreview, "access-token") || strings.Contains(result.BodyPreview, "refresh-token") {
		t.Errorf("BodyPreview leaked credentials: %q", result.BodyPreview)
	}
}

func TestSAPILoginOAuthWithAuthorizationUsesProvidedHeader(t *testing.T) {
	const authorization = `oauth custom-value`

	var received string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"validationkey":"validation-value"}}`))
	}))
	defer server.Close()

	result := SAPILoginOAuthWithAuthorization(
		server.Client(),
		server.URL+"/sapi",
		authorization,
		"custom-user-agent",
	)
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", result.StatusCode, http.StatusOK)
	}
	if received != authorization {
		t.Fatalf("Authorization = %q, want %q", received, authorization)
	}
}

func TestNewHTTP1FingerprintedTransportUsesHTTP1(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	client := &http.Client{Transport: NewHTTP1FingerprintedTransport()}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 1 {
		t.Fatalf("protocol = %s, want HTTP/1.x", resp.Proto)
	}
}
