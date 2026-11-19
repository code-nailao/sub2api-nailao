package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// RenameName 是独立的条件改名接口。旧服务会返回 404，不会忽略条件后无条件改名。
func (h *APIKeyHandler) RenameName(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 || c.ContentType() != "application/json" {
		response.BadRequest(c, "Invalid conditional rename request")
		return
	}
	var in struct {
		Name            string `json:"name"`
		ExpectedName    string `json:"expected_name"`
		ExpectedGroupID int64  `json:"expected_group_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || decoder.Decode(new(any)) != io.EOF || in.Name == "" || in.ExpectedName == "" || in.ExpectedGroupID <= 0 {
		response.BadRequest(c, "Invalid conditional rename request")
		return
	}
	name, err := h.apiKeyService.RenameNameIfMatches(c.Request.Context(), id, subject.UserID, in.ExpectedGroupID, in.ExpectedName, in.Name)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// 改名回执不需要返回凭据或完整 Key 配置。
	response.Success(c, gin.H{"id": id, "name": name})
}
