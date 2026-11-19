package admin

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"strconv"
)

func (h *AccountHandler) SetAccountWalletReader(reader *service.AccountWalletReader) {
	h.automaticWallet = reader
}

// GetAutomaticWallet uses the existing account configuration without disclosing it.
// The native administrator middleware is required; this endpoint never changes a binding.
func (h *AccountHandler) GetAutomaticWallet(c *gin.Context) {
	raw := c.Query("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	refresh := c.Query("refresh")
	if refresh != "" && refresh != "1" {
		response.BadRequest(c, "Invalid refresh flag")
		return
	}
	if h.automaticWallet == nil {
		response.Error(c, 503, "Balance reader unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), service.AccountWalletOperationTimeout)
	defer cancel()
	accounts, err := h.adminService.GetAccountsByIDs(ctx, []int64{id})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if len(accounts) != 1 || accounts[0] == nil || accounts[0].ID != id {
		response.NotFound(c, "Account not found")
		return
	}
	identity := service.AccountWalletIdentity(accounts[0])
	scope, scopeErr := h.automaticWallet.Scope(ctx, accounts[0])
	if scopeErr != nil {
		response.ErrorFrom(c, scopeErr)
		return
	}
	var value service.AccountWallet
	if requestID := c.Query("request_id"); requestID != "" {
		if refresh != "1" || !service.ValidWalletRequestID(requestID) {
			response.BadRequest(c, "Invalid wallet request ID")
			return
		}
		value = h.automaticWallet.ReadAsync(ctx, accounts[0], requestID)
	} else {
		value = h.automaticWallet.Read(ctx, accounts[0], refresh == "1")
	}
	// Account edits during an upstream query must not expose a previous wallet result.
	current, err := h.adminService.GetAccountsByIDs(ctx, []int64{id})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if len(current) != 1 || current[0] == nil || service.AccountWalletIdentity(current[0]) != identity {
		response.Error(c, 409, "Account changed; reload balances")
		return
	}
	currentScope, currentScopeErr := h.automaticWallet.Scope(ctx, current[0])
	if currentScopeErr != nil || currentScope != scope {
		response.Error(c, 409, "Wallet membership changed; reload balances")
		return
	}
	response.Success(c, value)
}
