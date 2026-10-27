package server

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/stefan-zemljic/data-organizer/internal/backupjob"
	"github.com/stefan-zemljic/data-organizer/internal/store"
)

func registerBackupRoutes(api *echo.Group, backups *backupjob.Manager) {
	h := backupHandlers{backups}
	api.GET("/backups", h.list)
	api.POST("/backups", h.start)
	api.GET("/backups/current", h.current)
	api.GET("/backups/:id", h.get)
	api.DELETE("/backups/:id", h.delete)
	api.POST("/backups/:id/pause", h.action(backups.Pause))
	api.POST("/backups/:id/resume", h.action(backups.Resume))
	api.POST("/backups/:id/stop", h.action(backups.Stop))
	api.GET("/backups/:id/entries/roots", h.entryRoots)
	api.GET("/backups/:id/entries/children", h.entryChildren)
	api.POST("/backups/:id/entries/exclude", h.exclude)
	api.POST("/backups/:id/entries/reset", h.reset)
	api.POST("/backups/:id/entries/state", h.entryState)
}

type backupHandlers struct {
	backups *backupjob.Manager
}

func (h backupHandlers) list(c *echo.Context) error {
	backups, err := h.backups.List(c.Request().Context())
	if err != nil {
		return backupError(err)
	}
	return c.JSON(http.StatusOK, map[string]any{"backups": backups})
}

func (h backupHandlers) start(c *echo.Context) error {
	var req struct {
		Roots []string `json:"roots"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, `body must be JSON: {"roots": [paths]}`)
	}
	sc, err := h.backups.Start(c.Request().Context(), req.Roots)
	if err != nil {
		return backupError(err)
	}
	return c.JSON(http.StatusCreated, sc)
}

// current returns {"backup": <active backup>} or {"backup": null}.
func (h backupHandlers) current(c *echo.Context) error {
	var body struct {
		Backup *store.Backup `json:"backup"`
	}
	if sc, ok := h.backups.Current(); ok {
		body.Backup = &sc
	}
	return c.JSON(http.StatusOK, body)
}

func (h backupHandlers) get(c *echo.Context) error {
	id, err := pathID(c, "backup")
	if err != nil {
		return err
	}
	sc, err := h.backups.Get(c.Request().Context(), id)
	if err != nil {
		return backupError(err)
	}
	return c.JSON(http.StatusOK, sc)
}

// delete removes a finished backup with everything derived from it and
// returns what was removed: {"deleted": {items, restores, names}}.
func (h backupHandlers) delete(c *echo.Context) error {
	id, err := pathID(c, "backup")
	if err != nil {
		return err
	}
	d, err := h.backups.Delete(c.Request().Context(), id)
	if err != nil {
		return backupError(err)
	}
	return c.JSON(http.StatusOK, map[string]any{"deleted": d})
}

// action serves pause, resume and stop.
func (h backupHandlers) action(do func(id int64) (store.Backup, error)) echo.HandlerFunc {
	return func(c *echo.Context) error {
		id, err := pathID(c, "backup")
		if err != nil {
			return err
		}
		sc, err := do(id)
		if err != nil {
			return backupError(err)
		}
		return c.JSON(http.StatusOK, sc)
	}
}

func (h backupHandlers) entryRoots(c *echo.Context) error {
	id, err := pathID(c, "backup")
	if err != nil {
		return err
	}
	entries, err := h.backups.Store().EntryRoots(c.Request().Context(), id)
	if err != nil {
		return backupError(err)
	}
	return c.JSON(http.StatusOK, map[string]any{"entries": entries})
}

func (h backupHandlers) entryChildren(c *echo.Context) error {
	id, err := pathID(c, "backup")
	if err != nil {
		return err
	}
	p, err := bindChildrenParams(c)
	if err != nil {
		return err
	}
	page, err := h.backups.Store().EntryChildren(c.Request().Context(), id, p.Parent, p.Offset, p.Limit)
	if err != nil {
		return backupError(err)
	}
	return c.JSON(http.StatusOK, page)
}

// exclude sets ({"on": true}) or clears the exclusion mark of items. Only
// those rows change; everything below them follows when read.
func (h backupHandlers) exclude(c *echo.Context) error {
	id, err := pathID(c, "backup")
	if err != nil {
		return err
	}
	var req struct {
		Items []int64 `json:"items"`
		On    *bool   `json:"on"`
	}
	if err := c.Bind(&req); err != nil || req.On == nil || len(req.Items) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, `body must be JSON: {"items": [ids], "on": true|false}`)
	}
	if err := h.backups.Store().SetExcluded(c.Request().Context(), id, req.Items, *req.On); err != nil {
		return backupError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// reset removes every exclusion of a backup.
func (h backupHandlers) reset(c *echo.Context) error {
	id, err := pathID(c, "backup")
	if err != nil {
		return err
	}
	removed, err := h.backups.Store().ResetExclusions(c.Request().Context(), id)
	if err != nil {
		return backupError(err)
	}
	return c.JSON(http.StatusOK, map[string]int{"removed": removed})
}

// entryState returns the current entries for item ids the client has
// loaded, so it can redraw them after exclusions changed.
func (h backupHandlers) entryState(c *echo.Context) error {
	id, err := pathID(c, "backup")
	if err != nil {
		return err
	}
	var req struct {
		Items []int64 `json:"items"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, `body must be JSON: {"items": [ids]}`)
	}
	entries, err := h.backups.Store().EntryState(c.Request().Context(), id, req.Items)
	if err != nil {
		return backupError(err)
	}
	return c.JSON(http.StatusOK, map[string]any{"entries": entries})
}

func backupError(err error) error {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, backupjob.ErrInvalidRoots), errors.Is(err, store.ErrNotExcludable),
		errors.Is(err, store.ErrTooManyItems):
		code = http.StatusBadRequest
	case errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, backupjob.ErrBusy), errors.Is(err, backupjob.ErrDeleting), errors.Is(err, backupjob.ErrNotActive),
		errors.Is(err, backupjob.ErrInvalidState),
		errors.Is(err, store.ErrNotReady), errors.Is(err, store.ErrRestoreUnfinished):
		code = http.StatusConflict
	}
	return echo.NewHTTPError(code, err.Error())
}
