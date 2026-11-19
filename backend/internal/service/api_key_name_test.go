package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type conditionalNameRepoStub struct {
	APIKeyRepository
	key      *APIKey
	updated  bool
	err      error
	calls    int
	name     string
	expected string
}

func (r *conditionalNameRepoStub) GetByID(context.Context, int64) (*APIKey, error) {
	if r.key == nil {
		return nil, ErrAPIKeyNotFound
	}
	key := *r.key
	return &key, nil
}

func (r *conditionalNameRepoStub) RenameIfNameMatches(_ context.Context, _, _, _ int64, expected, name string) (bool, error) {
	r.calls++
	r.name, r.expected = name, expected
	return r.updated, r.err
}

func TestAPIKeyConditionalRenamePreservesStoredPreconditionAndEscapesNewName(t *testing.T) {
	groupID := int64(4)
	repo := &conditionalNameRepoStub{key: &APIKey{ID: 9, UserID: 7, GroupID: &groupID, Name: "old &amp; name"}, updated: true}
	svc := &APIKeyService{apiKeyRepo: repo}
	name, err := svc.RenameNameIfMatches(context.Background(), 9, 7, 4, "old &amp; name", "生图 · A & B")
	require.NoError(t, err)
	require.Equal(t, "生图 · A &amp; B", name)
	require.Equal(t, "old &amp; name", repo.expected, "stored precondition must not be escaped again")
	require.Equal(t, name, repo.name)
}

func TestAPIKeyConditionalRenameRejectsOtherOwnerOrChangedBinding(t *testing.T) {
	groupID := int64(4)
	for _, test := range []struct {
		name         string
		owner, group int64
		expected     string
		want         error
	}{
		{"another owner", 8, 4, "original", ErrAPIKeyNotFound},
		{"changed group", 7, 5, "original", ErrAPIKeyNameChanged},
		{"user renamed", 7, 4, "different", ErrAPIKeyNameChanged},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &conditionalNameRepoStub{key: &APIKey{ID: 9, UserID: 7, GroupID: &groupID, Name: "original"}, updated: true}
			svc := &APIKeyService{apiKeyRepo: repo}
			_, err := svc.RenameNameIfMatches(context.Background(), 9, test.owner, test.group, test.expected, "new")
			require.ErrorIs(t, err, test.want)
			require.Zero(t, repo.calls)
		})
	}
}

func TestAPIKeyConditionalRenameConcurrentChangeDoesNotRetryUnconditionally(t *testing.T) {
	groupID := int64(4)
	repo := &conditionalNameRepoStub{key: &APIKey{ID: 9, UserID: 7, GroupID: &groupID, Name: "original"}}
	svc := &APIKeyService{apiKeyRepo: repo}
	_, err := svc.RenameNameIfMatches(context.Background(), 9, 7, 4, "original", "new")
	require.ErrorIs(t, err, ErrAPIKeyNameChanged)
	require.Equal(t, 1, repo.calls)
	repo.err = errors.New("database unavailable")
	_, err = svc.RenameNameIfMatches(context.Background(), 9, 7, 4, "original", "new")
	require.ErrorIs(t, err, repo.err)
	require.Equal(t, 2, repo.calls)
}

func TestAPIKeyConditionalRenameValidatesEscapedByteLengthAndUTF8(t *testing.T) {
	for _, name := range []string{"", " \t ", strings.Repeat("a", 101), strings.Repeat("&", 21), strings.Repeat("图", 34), string([]byte{0xff})} {
		t.Run(name, func(t *testing.T) {
			svc := &APIKeyService{}
			_, err := svc.RenameNameIfMatches(context.Background(), 9, 7, 4, "original", name)
			require.ErrorIs(t, err, ErrAPIKeyNameInvalid)
		})
	}
}
