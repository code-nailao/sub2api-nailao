package admin

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// This native administrator endpoint reserves deletion only. Portal still owns
// the source file and must delete the exact acknowledged revision locally.
func (h *AccountHandler) ReserveManagedWalletSourceDeletion(c *gin.Context) {
	m := h.walletManager(c)
	if m == nil {
		return
	}
	var in struct {
		SourceID       string `json:"source_id"`
		SourceRevision int64  `json:"source_revision"`
	}
	if !decodeWalletManagement(c, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	result, err := m.ReserveSourceDeletion(ctx, in.SourceID, in.SourceRevision)
	if !response.ErrorFrom(c, err) {
		response.Success(c, result)
	}
}
