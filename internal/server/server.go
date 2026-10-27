// Package server wires the filesystem API and the web UI into Echo.
package server

import (
	"errors"
	"io/fs"
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"github.com/stefan-zemljic/data-organizer/internal/backupjob"
	"github.com/stefan-zemljic/data-organizer/internal/fsbrowse"
	"github.com/stefan-zemljic/data-organizer/internal/restorejob"
)

// New returns the HTTP handler. Requests are only served when their Host
// header is one of allowedHosts (e.g. "127.0.0.1:8080"); this blocks DNS
// rebinding, where a foreign website resolves its own name to 127.0.0.1.
// Cross-origin writes (CSRF: POST, DELETE) are rejected, so other sites
// cannot start or delete anything, and access requires the key (see Access).
func New(static fs.FS, allowedHosts []string, access *Access, backups *backupjob.Manager, restores *restorejob.Manager) http.Handler {
	app := echo.New()
	app.Use(middleware.Recover())
	app.Use(echo.WrapMiddleware(http.NewCrossOriginProtection().Handler))
	app.Use(middleware.BodyLimit(8 << 20))
	app.Use(middleware.SecureWithConfig(middleware.SecureConfig{
		ContentTypeNosniff:    "nosniff",
		XFrameOptions:         "DENY",
		ReferrerPolicy:        "no-referrer",
		ContentSecurityPolicy: "default-src 'self'; frame-ancestors 'none'; form-action 'none'",
	}))
	app.Use(access.middleware)

	api := app.Group("/api", noStore)
	api.GET("/roots", roots)
	api.GET("/list", list)
	registerBackupRoutes(api, backups)
	registerRestoreRoutes(api, restores, backups.Store())
	app.StaticFS("/", static, revalidate)

	vhosts := make(map[string]*echo.Echo, len(allowedHosts))
	for _, h := range allowedHosts {
		vhosts[h] = app
	}
	return echo.NewVirtualHostHandler(vhosts)
}

// revalidate makes browsers check UI files on every load, so an updated
// module is never combined with stale cached ones.
func revalidate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-cache")
		return next(c)
	}
}

func noStore(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-store")
		return next(c)
	}
}

func roots(c *echo.Context) error {
	entries, err := fsbrowse.Roots()
	if err != nil {
		return toHTTPError(err)
	}
	return c.JSON(http.StatusOK, map[string]any{"entries": entries})
}

type listParams struct {
	Path   string `query:"path"`
	Offset int    `query:"offset"`
	Limit  int    `query:"limit"`
}

func list(c *echo.Context) error {
	var p listParams
	if err := echo.BindQueryParams(c, &p); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "path, offset and limit must be valid query parameters")
	}
	page, err := fsbrowse.List(p.Path, p.Offset, p.Limit)
	if err != nil {
		return toHTTPError(err)
	}
	return c.JSON(http.StatusOK, page)
}

func toHTTPError(err error) error {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, fsbrowse.ErrInvalidRequest), errors.Is(err, fsbrowse.ErrNotDirectory):
		code = http.StatusBadRequest
	case errors.Is(err, fs.ErrNotExist):
		code = http.StatusNotFound
	case errors.Is(err, fs.ErrPermission):
		code = http.StatusForbidden
	}
	return echo.NewHTTPError(code, err.Error())
}
