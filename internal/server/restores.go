package server

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/stefan-zemljic/data-organizer/internal/restorejob"
	"github.com/stefan-zemljic/data-organizer/internal/store"
)

func registerRestoreRoutes(api *echo.Group, restores *restorejob.Manager, st *store.Store) {
	h := restoreHandlers{restores, st}
	api.POST("/backups/:id/restore", h.start)
	api.GET("/restores", h.list)
	api.GET("/restores/current", h.current)
	api.GET("/restores/:id", h.get)
	api.POST("/restores/:id/pause", h.pause)
	api.POST("/restores/:id/resume", h.resume)
	api.POST("/restores/:id/stop", h.stop)
	api.GET("/restores/:id/entries/roots", h.entryRoots)
	api.GET("/restores/:id/entries/children", h.entryChildren)
}

type restoreHandlers struct {
	restores *restorejob.Manager
	store    *store.Store
}

// start restores everything of a backup that is not excluded and is the first
// copy by path into a new or empty target folder.
func (h restoreHandlers) start(c *echo.Context) error {
	backupID, err := pathID(c, "backup")
	if err != nil {
		return err
	}
	var req struct {
		Target string `json:"target"`
	}
	if err := c.Bind(&req); err != nil || req.Target == "" {
		return echo.NewHTTPError(http.StatusBadRequest, `body must be JSON: {"target": "absolute folder path"}`)
	}
	cp, err := h.restores.Start(c.Request().Context(), backupID, req.Target)
	if err != nil {
		return restoreError(err)
	}
	return c.JSON(http.StatusCreated, cp)
}

func (h restoreHandlers) list(c *echo.Context) error {
	restores, err := h.restores.List(c.Request().Context())
	if err != nil {
		return restoreError(err)
	}
	return c.JSON(http.StatusOK, map[string]any{"restores": restores})
}

// current returns {"restore": <running restore>} or {"restore": null}.
func (h restoreHandlers) current(c *echo.Context) error {
	var body struct {
		Restore *store.Restore `json:"restore"`
	}
	if cp, ok := h.restores.Current(); ok {
		body.Restore = &cp
	}
	return c.JSON(http.StatusOK, body)
}

func (h restoreHandlers) get(c *echo.Context) error {
	id, err := pathID(c, "restore")
	if err != nil {
		return err
	}
	cp, err := h.restores.Get(c.Request().Context(), id)
	if err != nil {
		return restoreError(err)
	}
	return c.JSON(http.StatusOK, cp)
}

func (h restoreHandlers) pause(c *echo.Context) error {
	id, err := pathID(c, "restore")
	if err != nil {
		return err
	}
	cp, err := h.restores.Pause(id)
	if err != nil {
		return restoreError(err)
	}
	return c.JSON(http.StatusOK, cp)
}

func (h restoreHandlers) resume(c *echo.Context) error {
	id, err := pathID(c, "restore")
	if err != nil {
		return err
	}
	cp, err := h.restores.Resume(c.Request().Context(), id)
	if err != nil {
		return restoreError(err)
	}
	return c.JSON(http.StatusOK, cp)
}

func (h restoreHandlers) stop(c *echo.Context) error {
	id, err := pathID(c, "restore")
	if err != nil {
		return err
	}
	cp, err := h.restores.Stop(c.Request().Context(), id)
	if err != nil {
		return restoreError(err)
	}
	return c.JSON(http.StatusOK, cp)
}

func (h restoreHandlers) entryRoots(c *echo.Context) error {
	id, err := pathID(c, "restore")
	if err != nil {
		return err
	}
	entries, err := h.store.RestoreEntryRoots(c.Request().Context(), id)
	if err != nil {
		return restoreError(err)
	}
	return c.JSON(http.StatusOK, map[string]any{"entries": entries})
}

func (h restoreHandlers) entryChildren(c *echo.Context) error {
	id, err := pathID(c, "restore")
	if err != nil {
		return err
	}
	p, err := bindChildrenParams(c)
	if err != nil {
		return err
	}
	page, entries, err := h.store.RestoreEntryChildren(c.Request().Context(), id, p.Parent, p.Offset, p.Limit)
	if err != nil {
		return restoreError(err)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"offset": page.Offset, "limit": page.Limit, "total": page.Total, "nextOffset": page.NextOffset, "entries": entries,
	})
}

func restoreError(err error) error {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, restorejob.ErrInvalidTarget), errors.Is(err, restorejob.ErrTargetNotEmpty):
		code = http.StatusBadRequest
	case errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, restorejob.ErrBusy), errors.Is(err, restorejob.ErrBackupBusy), errors.Is(err, restorejob.ErrNotActive),
		errors.Is(err, restorejob.ErrInvalidState),
		errors.Is(err, store.ErrNotReady):
		code = http.StatusConflict
	}
	return echo.NewHTTPError(code, err.Error())
}
