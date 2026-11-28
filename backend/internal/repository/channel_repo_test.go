//go:build unit

package repository

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// --- marshalModelMapping ---

func TestMarshalModelMapping(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]map[string]string
		wantJSON string // expected JSON output (exact match)
	}{
		{
			name:     "empty map",
			input:    map[string]map[string]string{},
			wantJSON: "{}",
		},
		{
			name:     "nil map",
			input:    nil,
			wantJSON: "{}",
		},
		{
			name: "populated map",
			input: map[string]map[string]string{
				"openai": {"gpt-4": "gpt-4-turbo"},
			},
		},
		{
			name: "nested values",
			input: map[string]map[string]string{
				"openai":    {"*": "gpt-5.4"},
				"anthropic": {"claude-old": "claude-new"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := marshalModelMapping(tt.input)
			require.NoError(t, err)

			if tt.wantJSON != "" {
				require.Equal(t, []byte(tt.wantJSON), result)
			} else {
				// round-trip: unmarshal and compare with input
				var parsed map[string]map[string]string
				require.NoError(t, json.Unmarshal(result, &parsed))
				require.Equal(t, tt.input, parsed)
			}
		})
	}
}

// --- unmarshalModelMapping ---

func TestUnmarshalModelMapping(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		wantNil bool
		want    map[string]map[string]string
	}{
		{
			name:    "nil data",
			input:   nil,
			wantNil: true,
		},
		{
			name:    "empty data",
			input:   []byte{},
			wantNil: true,
		},
		{
			name:    "invalid JSON",
			input:   []byte("not-json"),
			wantNil: true,
		},
		{
			name:    "type error - number",
			input:   []byte("42"),
			wantNil: true,
		},
		{
			name:    "type error - array",
			input:   []byte("[1,2,3]"),
			wantNil: true,
		},
		{
			name:  "valid JSON",
			input: []byte(`{"openai":{"gpt-4":"gpt-4-turbo"},"anthropic":{"old":"new"}}`),
			want: map[string]map[string]string{
				"openai":    {"gpt-4": "gpt-4-turbo"},
				"anthropic": {"old": "new"},
			},
		},
		{
			name:  "empty object",
			input: []byte("{}"),
			want:  map[string]map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := unmarshalModelMapping(tt.input)
			if tt.wantNil {
				require.Nil(t, result)
			} else {
				require.NotNil(t, result)
				require.Equal(t, tt.want, result)
			}
		})
	}
}

// --- escapeLike ---

func TestEscapeLike(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no special chars",
			input: "hello",
			want:  "hello",
		},
		{
			name:  "backslash",
			input: `a\b`,
			want:  `a\\b`,
		},
		{
			name:  "percent",
			input: "50%",
			want:  `50\%`,
		},
		{
			name:  "underscore",
			input: "a_b",
			want:  `a\_b`,
		},
		{
			name:  "all special chars",
			input: `a\b%c_d`,
			want:  `a\\b\%c\_d`,
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "consecutive special chars",
			input: "%_%",
			want:  `\%\_\%`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, escapeLike(tt.input))
		})
	}
}

// --- isUniqueViolation ---

func TestIsUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "unique violation code 23505",
			err:  &pq.Error{Code: "23505"},
			want: true,
		},
		{
			name: "different pq error code",
			err:  &pq.Error{Code: "23503"},
			want: false,
		},
		{
			name: "non-pq error",
			err:  errors.New("some generic error"),
			want: false,
		},
		{
			name: "typed nil pq.Error",
			err: func() error {
				var pqErr *pq.Error
				return pqErr
			}(),
			want: false,
		},
		{
			name: "bare nil",
			err:  nil,
			want: false,
		},
		{
			name: "wrapped pq error with 23505",
			err:  fmt.Errorf("wrapped: %w", &pq.Error{Code: "23505"}),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isUniqueViolation(tt.err))
		})
	}
}

func TestChannelListOrderBy_AllowsDescendingIDSort(t *testing.T) {
	params := pagination.PaginationParams{
		SortBy:    "id",
		SortOrder: "desc",
	}

	require.Equal(t, "c.id DESC, c.id DESC", channelListOrderBy(params))
}

// TestChannelListOrderBy_GroupOrderIsAscendingRegardlessOfSortOrder 验证 group_order
// 的方向固定为升序：无论 sort_order 传 asc、desc 还是省略，ORDER BY 片段都相同。
func TestChannelListOrderBy_GroupOrderIsAscendingRegardlessOfSortOrder(t *testing.T) {
	want := "(c.status = 'active') DESC, (fg.id IS NULL) ASC, fg.sort_order ASC, fg.id ASC, c.id ASC"
	for _, order := range []string{"asc", "desc", "DESC", ""} {
		params := pagination.PaginationParams{SortBy: "group_order", SortOrder: order}
		require.Equalf(t, want, channelListOrderBy(params), "sort_order=%q", order)
	}
}

// TestChannelListJoin_OnlyGroupOrderJoins 验证只有 group_order 追加 LATERAL JOIN，
// 且该片段也不随 sort_order 变化；其它排序键、空值、未知值都不带 JOIN。
func TestChannelListJoin_OnlyGroupOrderJoins(t *testing.T) {
	wantJoin := ` LEFT JOIN LATERAL (
    SELECT g.sort_order, g.id
    FROM channel_groups cg
    JOIN groups g ON g.id = cg.group_id
    WHERE cg.channel_id = c.id
      AND g.status = 'active'
      AND g.deleted_at IS NULL
    ORDER BY g.sort_order ASC, g.id ASC
    LIMIT 1
) fg ON true`
	for _, order := range []string{"asc", "desc", ""} {
		params := pagination.PaginationParams{SortBy: "group_order", SortOrder: order}
		require.Equalf(t, wantJoin, channelListJoin(params), "sort_order=%q", order)
	}

	for _, sortBy := range []string{"", "id", "name", "status", "created_at", "unknown"} {
		params := pagination.PaginationParams{SortBy: sortBy, SortOrder: "desc"}
		require.Emptyf(t, channelListJoin(params), "sort_by=%q must not add a join", sortBy)
	}
}

// TestChannelListOrderBy_NonGroupKeysUnchanged 固定其它排序键、空值、未知值的输出，
// 防止回归改动它们的 SQL 文本。
func TestChannelListOrderBy_NonGroupKeysUnchanged(t *testing.T) {
	cases := []struct {
		sortBy    string
		sortOrder string
		want      string
	}{
		{"", "", "c.id ASC, c.id ASC"},
		{"", "desc", "c.id ASC, c.id ASC"},
		{"id", "asc", "c.id ASC, c.id ASC"},
		{"id", "desc", "c.id DESC, c.id DESC"},
		{"name", "asc", "c.name ASC, c.id ASC"},
		{"name", "desc", "c.name DESC, c.id DESC"},
		{"status", "asc", "c.status ASC, c.id ASC"},
		{"status", "desc", "c.status DESC, c.id DESC"},
		{"created_at", "asc", "c.created_at ASC, c.id ASC"},
		{"created_at", "desc", "c.created_at DESC, c.id DESC"},
		{"unknown", "desc", "c.id ASC, c.id ASC"},
	}
	for _, tc := range cases {
		params := pagination.PaginationParams{SortBy: tc.sortBy, SortOrder: tc.sortOrder}
		require.Equalf(t, tc.want, channelListOrderBy(params), "sort_by=%q sort_order=%q", tc.sortBy, tc.sortOrder)
	}
}
