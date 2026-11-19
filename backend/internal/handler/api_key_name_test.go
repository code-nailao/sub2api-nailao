package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type conditionalKeyNameHandlerRepo struct {
	service.APIKeyRepository
	key *service.APIKey
}

func (r *conditionalKeyNameHandlerRepo) GetByID(context.Context, int64) (*service.APIKey, error) {
	key := *r.key
	return &key, nil
}

func (r *conditionalKeyNameHandlerRepo) RenameIfNameMatches(_ context.Context, id, owner, group int64, expected, name string) (bool, error) {
	if r.key.ID != id || r.key.UserID != owner || r.key.GroupID == nil || *r.key.GroupID != group || r.key.Name != expected {
		return false, nil
	}
	r.key.Name = name
	return true, nil
}

func TestConditionalAPIKeyRenameUsesAuthenticatedOwnerAndMinimalResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := int64(4)
	repo := &conditionalKeyNameHandlerRepo{key: &service.APIKey{ID: 9, UserID: 7, GroupID: &group, Key: "secret-test-key", Name: "old"}}
	svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, nil)
	handler := NewAPIKeyHandler(svc)
	call := func(owner int64, expected string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rr)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/api/v1/keys/9/name", strings.NewReader(`{"name":"生图 · 分组","expected_name":"`+expected+`","expected_group_id":4}`))
		ctx.Request.Header.Set("Content-Type", "application/json")
		ctx.Params = gin.Params{{Key: "id", Value: "9"}}
		ctx.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: owner})
		handler.RenameName(ctx)
		return rr
	}
	require.Equal(t, http.StatusNotFound, call(8, "old").Code)
	require.Equal(t, "old", repo.key.Name)
	rr := call(7, "old")
	require.Equal(t, http.StatusOK, rr.Code)
	var out struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Equal(t, map[string]any{"id": float64(9), "name": "生图 · 分组"}, out.Data)
	require.NotContains(t, rr.Body.String(), repo.key.Key)
	require.Equal(t, http.StatusConflict, call(7, "old").Code)
}

func TestConditionalAPIKeyRenameRejectsUnauthenticatedAndUnexpectedFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	valid := `{"name":"new","expected_name":"old","expected_group_id":4}`
	for _, test := range []struct {
		name, id, contentType, body string
		auth                        bool
		status                      int
	}{
		{"unauthenticated", "9", "application/json", valid, false, http.StatusUnauthorized},
		{"invalid id", "-1", "application/json", valid, true, http.StatusBadRequest},
		{"wrong media type", "9", "text/plain", valid, true, http.StatusBadRequest},
		{"quota field forbidden", "9", "application/json", `{"name":"new","expected_name":"old","expected_group_id":4,"quota":0}`, true, http.StatusBadRequest},
		{"missing condition", "9", "application/json", `{"name":"new"}`, true, http.StatusBadRequest},
		{"null body", "9", "application/json", `null`, true, http.StatusBadRequest},
		{"trailing value", "9", "application/json", valid + `{}`, true, http.StatusBadRequest},
		{"oversized", "9", "application/json", `{"name":"` + strings.Repeat("x", 4096) + `","expected_name":"old","expected_group_id":4}`, true, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rr)
			ctx.Request = httptest.NewRequest(http.MethodPatch, "/api/v1/keys/"+test.id+"/name", strings.NewReader(test.body))
			ctx.Request.Header.Set("Content-Type", test.contentType)
			ctx.Params = gin.Params{{Key: "id", Value: test.id}}
			if test.auth {
				ctx.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
			}
			NewAPIKeyHandler(nil).RenameName(ctx)
			require.Equal(t, test.status, rr.Code)
		})
	}
}
