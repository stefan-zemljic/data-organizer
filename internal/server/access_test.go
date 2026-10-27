package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var keyCookie = &http.Cookie{Name: "data_organizer_8080", Value: accessKey}

// request sends a GET without the key cookie unless one is given.
func request(t *testing.T, h http.Handler, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAccessDenied(t *testing.T) {
	h := newHandler(t)
	wrong := &http.Cookie{Name: keyCookie.Name, Value: "wrong"}
	for _, tc := range []struct {
		target  string
		cookies []*http.Cookie
	}{
		{"/", nil},
		{"/api/roots", nil},
		{"/api/roots", []*http.Cookie{wrong}},
		{"/?key=wrong", nil},
		{"/?code=wrong", nil},
		{"/api/roots", []*http.Cookie{{Name: "data_organizer_9090", Value: accessKey}}}, // another instance's cookie
	} {
		rec := request(t, h, tc.target, tc.cookies...)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<!doctype html>ui") || rec.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s: leaked UI or cookie: %q %q", tc.target, rec.Body, rec.Header().Get("Set-Cookie"))
		}
	}
}

func TestAccessKeyLink(t *testing.T) {
	h := newHandler(t)
	rec := request(t, h, "/?key="+accessKey+"&tab=backups")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?tab=backups" {
		t.Fatalf("status = %d, Location = %q", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != accessKey || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookies = %+v", cookies)
	}
	if rec := request(t, h, "/", cookies[0]); rec.Code != http.StatusOK {
		t.Errorf("with cookie: status = %d", rec.Code)
	}
}

func TestAccessLoginCode(t *testing.T) {
	a := NewAccess(accessKey, "8080")
	code := a.LoginCode()
	if !a.useCode(code) {
		t.Fatal("fresh code refused")
	}
	if a.useCode(code) {
		t.Error("code accepted twice")
	}
	if a.useCode("") {
		t.Error("empty code accepted")
	}
	old := a.LoginCode()
	a.codes[old] = time.Now().Add(-time.Second)
	if a.useCode(old) {
		t.Error("expired code accepted")
	}
	if len(a.codes) != 0 {
		t.Errorf("expired codes kept: %d", len(a.codes))
	}
}
