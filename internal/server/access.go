package server

import (
	"crypto/rand"
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
)

const codeLifetime = time.Minute

// Access locks the UI and API with a secret key. Any program or user on this
// machine can reach the loopback port; only requests that carry the key's
// cookie are served. A link with ?key=<key> (printed at startup) or with a
// one-time ?code= (used to open the browser) sets the cookie.
type Access struct {
	key    string
	cookie string // named per port: cookies ignore ports, and several instances may run

	mu    sync.Mutex
	codes map[string]time.Time // unused login codes and when they expire
}

// NewAccess returns an Access for the server listening on port.
func NewAccess(key, port string) *Access {
	return &Access{key: key, cookie: "data_organizer_" + port, codes: map[string]time.Time{}}
}

// LoginCode returns a code that unlocks the UI once, within a minute. Opening
// the browser with it keeps the key itself out of process command lines,
// which other users can read on some systems.
func (a *Access) LoginCode() string {
	code := rand.Text()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.codes[code] = time.Now().Add(codeLifetime)
	return code
}

func (a *Access) useCode(code string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for c, expires := range a.codes {
		if now.After(expires) {
			delete(a.codes, c)
		}
	}
	if _, ok := a.codes[code]; !ok || code == "" {
		return false
	}
	delete(a.codes, code)
	return true
}

func (a *Access) validKey(key string) bool {
	return subtle.ConstantTimeCompare([]byte(key), []byte(a.key)) == 1
}

func (a *Access) middleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		req := c.Request()
		q := req.URL.Query()
		if q.Has("key") || q.Has("code") {
			if !a.validKey(q.Get("key")) && !a.useCode(q.Get("code")) {
				return a.deny(c)
			}
			http.SetCookie(c.Response(), &http.Cookie{
				Name:     a.cookie,
				Value:    a.key,
				Path:     "/",
				MaxAge:   400 * 24 * 60 * 60, // the longest browsers keep
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			// Drop the secret from the address bar and history.
			q.Del("key")
			q.Del("code")
			u := *req.URL
			u.RawQuery = q.Encode()
			return c.Redirect(http.StatusSeeOther, u.RequestURI())
		}
		if ck, err := req.Cookie(a.cookie); err == nil && a.validKey(ck.Value) {
			return next(c)
		}
		return a.deny(c)
	}
}

const deniedMessage = "Access key missing or wrong. Open the link data_organizer printed when it started."

func (a *Access) deny(c *echo.Context) error {
	if strings.HasPrefix(c.Request().URL.Path, "/api/") {
		return echo.NewHTTPError(http.StatusUnauthorized, deniedMessage)
	}
	return c.HTML(http.StatusUnauthorized, `<!doctype html><meta charset="utf-8"><title>Data Organizer</title>`+
		`<p style="font-family:system-ui,sans-serif;margin:3rem">`+deniedMessage+`</p>`)
}
