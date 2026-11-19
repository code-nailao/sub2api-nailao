package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type walletConfigInput struct {
	URL               string `json:"url"`
	Revision          string `json:"revision"`
	VerificationToken string `json:"verification_token"`
}

func (h *AccountHandler) walletConfigAccount(c *gin.Context, ctx context.Context) *service.Account {
	raw := c.Query("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
		response.BadRequest(c, "Invalid account ID")
		return nil
	}
	if h.automaticWallet == nil {
		response.Error(c, http.StatusServiceUnavailable, "Balance reader unavailable")
		return nil
	}
	accounts, err := h.adminService.GetAccountsByIDs(ctx, []int64{id})
	if response.ErrorFrom(c, err) {
		return nil
	}
	if len(accounts) != 1 || accounts[0] == nil || accounts[0].ID != id {
		response.NotFound(c, "Account not found")
		return nil
	}
	return accounts[0]
}

func (h *AccountHandler) GetWalletConfiguration(c *gin.Context) {
	if a := h.walletConfigAccount(c, c.Request.Context()); a != nil {
		response.Success(c, h.automaticWallet.Configuration(a))
	}
}

func (h *AccountHandler) TestWalletConfiguration(c *gin.Context) {
	h.changeWalletConfiguration(c, true)
}

func (h *AccountHandler) SaveWalletConfiguration(c *gin.Context) {
	h.changeWalletConfiguration(c, false)
}

func (h *AccountHandler) changeWalletConfiguration(c *gin.Context, probe bool) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), service.AccountWalletOperationTimeout)
	defer cancel()
	var input walletConfigInput
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || len(input.Revision) != 64 || len(input.VerificationToken) > 100 {
		response.BadRequest(c, "Invalid balance configuration")
		return
	}
	a := h.walletConfigAccount(c, ctx)
	if a == nil {
		return
	}
	if !probe {
		value, err := h.automaticWallet.SaveConfiguration(ctx, a, input.URL, input.Revision, input.VerificationToken)
		if !response.ErrorFrom(c, err) {
			response.Success(c, value)
		}
		return
	}
	var value service.AccountWalletProbe
	var err error
	if requestID := c.Query("request_id"); requestID != "" {
		if !service.ValidWalletRequestID(requestID) {
			response.BadRequest(c, "Invalid wallet request ID")
			return
		}
		value, err = h.automaticWallet.ProbeConfigurationAsync(ctx, a, input.URL, input.Revision, requestID)
	} else {
		value, err = h.automaticWallet.ProbeConfiguration(ctx, a, input.URL, input.Revision)
	}
	if response.ErrorFrom(c, err) {
		return
	}
	current := h.walletConfigAccount(c, ctx)
	if current == nil {
		return
	}
	if service.AccountWalletIdentity(current) != service.AccountWalletIdentity(a) {
		response.ErrorFrom(c, service.ErrWalletConfigChanged)
		return
	}
	response.Success(c, value)
}
