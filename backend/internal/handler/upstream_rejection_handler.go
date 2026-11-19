package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// UpstreamRejectionHandler exposes pending holds separately from settled usage.
// It deliberately depends only on the owner-scoped read interface, never refund
// or reconciliation mutations and never the recovery worker's global scan.
type UpstreamRejectionHandler struct {
	reader service.UserPendingUpstreamRejectionReader
}

func NewUpstreamRejectionHandler(billing service.UsageBillingRepository) *UpstreamRejectionHandler {
	reader, _ := billing.(service.UserPendingUpstreamRejectionReader)
	return &UpstreamRejectionHandler{reader: reader}
}

type userPendingCharge struct {
	RequestID string    `json:"request_id"`
	Model     string    `json:"model"`
	State     string    `json:"state"`
	HeldPrice float64   `json:"held_price"`
	CreatedAt time.Time `json:"created_at"`
}

type userPendingChargesResponse struct {
	Items      []userPendingCharge `json:"items"`
	NextCursor *string             `json:"next_cursor"`
	PageSize   int                 `json:"page_size"`
}

// ListPending returns only the authenticated user's active monetary holds.
// GET /api/v1/usage/pending-charges?page_size=20&cursor=<opaque>
func (h *UpstreamRejectionHandler) ListPending(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	query := c.Request.URL.Query()
	for name, values := range query {
		if (name != "cursor" && name != "page_size" && name != "timezone") || len(values) != 1 {
			response.BadRequest(c, "Invalid pending-charge pagination")
			return
		}
	}
	// The shared panel client supplies this display preference on GET requests.
	// Validate it for compatibility, but never use it for ownership, cursor
	// ordering, amounts, SQL filters or the timestamps returned by this endpoint.
	if values, present := query["timezone"]; present {
		zone := values[0]
		if zone == "" || len(zone) > 128 || strings.TrimSpace(zone) != zone || zone == "Local" {
			response.BadRequest(c, "Invalid timezone")
			return
		}
		if _, err := time.LoadLocation(zone); err != nil {
			response.BadRequest(c, "Invalid timezone")
			return
		}
	}
	pageSize := 20
	if raw, present := query["page_size"]; present {
		parsed, err := strconv.Atoi(raw[0])
		if err != nil || parsed < 1 || parsed > 50 || raw[0] != strconv.Itoa(parsed) {
			response.BadRequest(c, "page_size must be an integer between 1 and 50")
			return
		}
		pageSize = parsed
	}
	cursor := query.Get("cursor")
	if len(cursor) > 512 || strings.TrimSpace(cursor) != cursor {
		response.BadRequest(c, "Invalid pending-charge pagination")
		return
	}
	if h == nil || h.reader == nil {
		response.Error(c, http.StatusServiceUnavailable, "Pending charges are temporarily unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	page, err := h.reader.ListUserPendingUpstreamRejections(ctx, subject.UserID, cursor, pageSize)
	if errors.Is(err, service.ErrUpstreamRejectionInvalid) {
		response.BadRequest(c, "Invalid pending-charge pagination")
		return
	}
	if err != nil || page == nil || len(page.Items) > pageSize {
		response.Error(c, http.StatusServiceUnavailable, "Pending charges are temporarily unavailable")
		return
	}
	out := userPendingChargesResponse{Items: make([]userPendingCharge, 0, len(page.Items)), PageSize: pageSize}
	for _, item := range page.Items {
		// Fail closed on a reader contract violation; never label settled usage as pending.
		if (item.State != service.UpstreamRejectionReserved && item.State != service.UpstreamRejectionSent && item.State != service.UpstreamRejectionUnknown) ||
			service.ValidateUpstreamRejectionBillingConfig(true, item.HeldPrice) != nil {
			response.Error(c, http.StatusServiceUnavailable, "Pending charges are temporarily unavailable")
			return
		}
		out.Items = append(out.Items, userPendingCharge{
			RequestID: item.RequestID, Model: item.Model, State: item.State, HeldPrice: item.HeldPrice, CreatedAt: item.CreatedAt,
		})
	}
	if page.NextCursor != "" {
		out.NextCursor = &page.NextCursor
	}
	response.Success(c, out)
}
