package admin

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) walletManager(c *gin.Context) *service.WalletManager {
	if h.automaticWallet == nil || h.automaticWallet.Manager == nil {
		response.Error(c, 503, "Wallet management unavailable")
		return nil
	}
	return h.automaticWallet.Manager
}

func walletAdminID(c *gin.Context) int64 {
	if subject, ok := middleware.GetAuthSubjectFromContext(c); ok {
		return subject.UserID
	}
	return 0
}

func decodeWalletManagement(c *gin.Context, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 128<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		response.BadRequest(c, "Invalid wallet settings")
		return false
	}
	return true
}

func (h *AccountHandler) GetManagedWallet(c *gin.Context) {
	m := h.walletManager(c)
	if m == nil {
		return
	}
	a := h.walletConfigAccount(c, c.Request.Context())
	if a == nil {
		return
	}
	view, err := m.View(c.Request.Context(), a.ID)
	if !response.ErrorFrom(c, err) {
		response.Success(c, view)
	}
}

func (h *AccountHandler) SaveManagedWallet(c *gin.Context) {
	m := h.walletManager(c)
	if m == nil {
		return
	}
	a := h.walletConfigAccount(c, c.Request.Context())
	if a == nil {
		return
	}
	var input service.WalletSettingsInput
	if !decodeWalletManagement(c, &input) {
		return
	}
	result, err := m.Save(c.Request.Context(), a.ID, walletAdminID(c), input)
	if !response.ErrorFrom(c, err) {
		response.Success(c, result)
	}
}

func (h *AccountHandler) UnlinkManagedWallet(c *gin.Context) {
	m := h.walletManager(c)
	if m == nil {
		return
	}
	a := h.walletConfigAccount(c, c.Request.Context())
	if a == nil {
		return
	}
	var input struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	if !decodeWalletManagement(c, &input) {
		return
	}
	if err := m.Unlink(c.Request.Context(), a.ID, input.ID, input.Revision); !response.ErrorFrom(c, err) {
		response.Success(c, gin.H{"unlinked": true})
	}
}

func (h *AccountHandler) PreviewWalletMail(c *gin.Context) {
	if h.walletManager(c) == nil {
		return
	}
	a := h.walletConfigAccount(c, c.Request.Context())
	if a == nil {
		return
	}
	var in struct {
		Name   string                  `json:"name"`
		Alert  service.WalletAlertRule `json:"alert"`
		Amount string                  `json:"amount"`
		Names  []string                `json:"names"`
	}
	if !decodeWalletManagement(c, &in) {
		return
	}
	if len(in.Name) > 160 || len(in.Amount) > 64 || len(in.Names) > 1000 {
		response.BadRequest(c, "Invalid preview")
		return
	}
	result := service.BuildWalletMail(service.ManagedWallet{Name: in.Name, Alert: in.Alert}, in.Amount, in.Names, time.Now(), false)
	response.Success(c, result)
}

func (h *AccountHandler) VerifyWalletChannel(c *gin.Context) {
	if h.walletManager(c) == nil {
		return
	}
	a := h.walletConfigAccount(c, c.Request.Context())
	if a == nil {
		return
	}
	requestID := c.Query("request_id")
	if !service.ValidWalletRequestID(requestID) {
		response.BadRequest(c, "Invalid request ID")
		return
	}
	identity := service.AccountWalletIdentity(a)
	result := h.automaticWallet.VerifyQueryChannelAsync(c.Request.Context(), a, requestID)
	current := h.walletConfigAccount(c, c.Request.Context())
	if current == nil {
		return
	}
	if service.AccountWalletIdentity(current) != identity {
		response.ErrorFrom(c, service.ErrWalletConfigChanged)
		return
	}
	response.Success(c, result)
}

func (h *AccountHandler) ManagedWalletSummaries(c *gin.Context) {
	m := h.walletManager(c)
	if m == nil {
		return
	}
	ids, err := service.ParseWalletAccountIDs(c.Query("ids"))
	if response.ErrorFrom(c, err) {
		return
	}
	rows, err := m.Summaries(c.Request.Context(), ids)
	if !response.ErrorFrom(c, err) {
		response.Success(c, gin.H{"accounts": rows})
	}
}

func (h *AccountHandler) BindManagedWalletSource(c *gin.Context) {
	m := h.walletManager(c)
	if m == nil {
		return
	}
	id, err := strconv.ParseInt(c.Query("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var in struct {
		SourceID       string `json:"source_id"`
		SourceRevision int64  `json:"source_revision"`
		Revision       int64  `json:"binding_revision"`
	}
	if !decodeWalletManagement(c, &in) {
		return
	}
	if err = m.BindSource(c.Request.Context(), id, in.SourceID, in.SourceRevision, in.Revision, walletAdminID(c)); response.ErrorFrom(c, err) {
		return
	}
	rows, err := m.Summaries(c.Request.Context(), []int64{id})
	if !response.ErrorFrom(c, err) {
		response.Success(c, rows[0])
	}
}

// Dedicated Portal capability: its derived token authorizes only this bounded
// metadata synchronization, not native administration or operator lifecycle.
func (h *AccountHandler) SyncWalletSources(c *gin.Context) {
	if h.cfg == nil {
		c.Status(404)
		return
	}
	expected := service.WalletBridgeToken(h.cfg.JWT.Secret)
	provided := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	if expected == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
		c.Status(404)
		return
	}
	m := h.walletManager(c)
	if m == nil {
		return
	}
	var input service.PortalWalletSync
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		response.BadRequest(c, "Invalid wallet snapshot")
		return
	}
	if err := m.SyncPortal(c.Request.Context(), input); !response.ErrorFrom(c, err) {
		response.Success(c, gin.H{"synchronized": true})
	}
}
