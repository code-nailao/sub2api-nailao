package admin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Balance identities contain no model key and cannot authorize an upstream call.
// The marker changes only when the account's calling identity changes.
type balanceAccountIdentity struct {
	AccountID int64  `json:"account_id"`
	Supported bool   `json:"supported"`
	Marker    string `json:"marker,omitempty"`
}

func balanceIdentity(a *service.Account, secret string) balanceAccountIdentity {
	result := balanceAccountIdentity{AccountID: a.ID}
	if a.Type != service.AccountTypeAPIKey || a.Platform == service.PlatformAntigravity || a.ParentAccountID != nil || a.GetCredential("api_key") == "" {
		return result
	}
	result.Supported = true
	// Adaptive accounts can change one protocol endpoint without touching base_url.
	// encoding/json sorts map keys, making the marker independent of map order.
	endpoints := map[string]string{}
	if values, ok := a.Credentials["api_base_urls"].(map[string]any); ok {
		for protocol, value := range values {
			if endpoint, ok := value.(string); ok {
				endpoints[protocol] = strings.TrimSpace(endpoint)
			}
		}
	}
	material, _ := json.Marshal([]any{"sources-balance-identity/v1", a.ID, a.CreatedAt.UTC().UnixNano(), a.Platform, a.Type, strings.TrimSpace(a.GetCredential("base_url")), a.GetCredential("api_key"), strings.TrimSpace(a.GetCredential("account_mode")), a.GetAPIProtocol(), endpoints})
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(material)
	result.Marker = "v1:" + hex.EncodeToString(mac.Sum(nil))
	return result
}

// GET /api/v1/admin/accounts/balance-identities?ids=1,2
// Native administrator middleware protects this narrowly scoped read-only API.
func (h *AccountHandler) GetBalanceIdentities(c *gin.Context) {
	if h.cfg == nil || len(h.cfg.JWT.Secret) < 32 {
		response.Error(c, 503, "Account identity unavailable")
		return
	}
	parts := strings.Split(c.Query("ids"), ",")
	if len(parts) == 0 || len(parts) > 100 {
		response.BadRequest(c, "Expected 1 to 100 account IDs")
		return
	}
	ids := make([]int64, 0, len(parts))
	seen := map[int64]bool{}
	for _, part := range parts {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != part || seen[id] {
			response.BadRequest(c, "Invalid or duplicate account ID")
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	accounts, err := h.adminService.GetAccountsByIDs(c.Request.Context(), ids)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	identities := make([]balanceAccountIdentity, 0, len(accounts))
	for _, a := range accounts {
		if a != nil && seen[a.ID] {
			identities = append(identities, balanceIdentity(a, h.cfg.JWT.Secret))
		}
	}
	response.Success(c, gin.H{"accounts": identities})
}
