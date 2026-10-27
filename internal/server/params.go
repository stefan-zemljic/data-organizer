package server

import (
	"fmt"
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/stefan-zemljic/data-organizer/internal/fsbrowse"
)

// pathID reads the numeric :id path parameter; what names it in the error
// ("backup", "restore").
func pathID(c *echo.Context, what string) (int64, error) {
	id, err := echo.PathParam[int64](c, "id")
	if err != nil {
		return 0, echo.NewHTTPError(http.StatusBadRequest, what+" id must be a number")
	}
	return id, nil
}

// childrenParams selects a page of a stored folder's children, with the
// same limits as live folder listings.
type childrenParams struct {
	Parent int64 `query:"parent"`
	Offset int   `query:"offset"`
	Limit  int   `query:"limit"`
}

func bindChildrenParams(c *echo.Context) (childrenParams, error) {
	var p childrenParams
	if err := echo.BindQueryParams(c, &p); err != nil {
		return p, echo.NewHTTPError(http.StatusBadRequest, "parent, offset and limit must be numbers")
	}
	if p.Limit < 1 || p.Limit > fsbrowse.MaxLimit || p.Offset < 0 {
		return p, echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("limit must be between 1 and %d and offset not negative", fsbrowse.MaxLimit))
	}
	return p, nil
}
