package service

import (
	"context"
	"fmt"
	"html"
	"strings"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrAPIKeyNameInvalid     = infraerrors.BadRequest("API_KEY_NAME_INVALID", "invalid api key name or rename condition")
	ErrAPIKeyNameChanged     = infraerrors.Conflict("API_KEY_NAME_CHANGED", "api key name or group has changed")
	ErrAPIKeyNameUnsupported = infraerrors.ServiceUnavailable("API_KEY_NAME_UNSUPPORTED", "conditional api key rename is unavailable")
)

// 条件改名单独扩展仓储，旧调用者仍沿用原来的 Key 更新接口。
type apiKeyConditionalNameRepository interface {
	RenameIfNameMatches(ctx context.Context, id, userID, groupID int64, expectedName, name string) (bool, error)
}

// RenameNameIfMatches 只更新仍属于原账号、分组和名称的 Key，不覆盖用户并发改名。
func (s *APIKeyService) RenameNameIfMatches(ctx context.Context, id, userID, groupID int64, expectedName, name string) (string, error) {
	escaped := html.EscapeString(name)
	if id <= 0 || userID <= 0 || groupID <= 0 || !utf8.ValidString(name) || !utf8.ValidString(expectedName) ||
		strings.TrimSpace(name) == "" || expectedName == "" || len(escaped) > 100 || len(expectedName) > 100 {
		return "", ErrAPIKeyNameInvalid
	}
	key, err := s.apiKeyRepo.GetByID(ctx, id)
	if err != nil {
		return "", fmt.Errorf("get api key for conditional rename: %w", err)
	}
	if key.UserID != userID {
		return "", ErrAPIKeyNotFound
	}
	if key.GroupID == nil || *key.GroupID != groupID || key.Name != expectedName {
		return "", ErrAPIKeyNameChanged
	}
	repo, ok := s.apiKeyRepo.(apiKeyConditionalNameRepository)
	if !ok {
		return "", ErrAPIKeyNameUnsupported
	}
	updated, err := repo.RenameIfNameMatches(ctx, id, userID, groupID, expectedName, escaped)
	if err != nil {
		return "", fmt.Errorf("conditionally rename api key: %w", err)
	}
	if !updated {
		return "", ErrAPIKeyNameChanged
	}
	s.InvalidateAuthCacheByKey(ctx, key.Key)
	return escaped, nil
}
