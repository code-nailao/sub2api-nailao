package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// UpdateSortOrder PUT /api/v1/admin/channel-monitors/sort-order.
func (h *ChannelMonitorHandler) UpdateSortOrder(c *gin.Context) {
	var req struct {
		Updates []service.ChannelMonitorSortOrderUpdate `json:"updates" binding:"required,min=1,max=10000"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid monitor ordering")
		return
	}
	if err := h.monitorService.UpdateSortOrders(c.Request.Context(), req.Updates); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Monitor order updated"})
}
