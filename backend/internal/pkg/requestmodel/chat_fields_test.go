package requestmodel

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/stretchr/testify/require"
)

func TestIndexedChatBodyDescribesOnlyThePrereadBytes(t *testing.T) {
	body := []byte(" \n" + `{"\u0074ools" : [ {} ]  , "other":{"tools":[]}, "tool_choice":"required", "messages":[  ], "input":[{}]  }` + "\t")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	field, conflict := ResetAndCheckChatBody(req, body)
	require.False(t, conflict, field)

	// Even a consumer that has already streamed the preread body does not
	// invalidate the borrowed bytes or trigger another allocation.
	_, err := io.Copy(io.Discard, req.Body)
	require.NoError(t, err)
	read, err := httputil.ReadRequestBodyWithPrealloc(req)
	require.NoError(t, err)
	require.Equal(t, body, read)
	require.True(t, &body[0] == &read[0])
	fields := ChatFieldsForBody(req, read)
	require.NotNil(t, fields)
	require.Equal(t, `[ {} ]`, string(read[fields.Tools.ValueStart:fields.Tools.ValueEnd]))
	require.Equal(t, `"required"`, string(read[fields.ToolChoice.ValueStart:fields.ToolChoice.ValueEnd]))
	require.Equal(t, `[  ]`, string(read[fields.Messages.ValueStart:fields.Messages.ValueEnd]))
	require.Equal(t, "[{}]", string(read[fields.Input.ValueStart:fields.Input.ValueEnd]))
	require.Equal(t, -1, fields.Tools.PrevComma)
	require.Equal(t, byte(','), read[fields.Tools.NextComma])
	require.Equal(t, byte(','), read[fields.ToolChoice.PrevComma])
	require.Equal(t, 2, fields.ToolChoice.Index)
	require.False(t, fields.Messages.NonemptyArray)
	require.True(t, fields.Input.NonemptyArray)
	require.Equal(t, byte('}'), read[fields.Close])
	require.Equal(t, 5, fields.Count)

	replacement := bytes.Clone(read)
	require.Nil(t, ChatFieldsForBody(req, replacement))
	ResetRequestBody(req, read)
	require.Equal(t, fields, ChatFieldsForBody(req, read), "resetting the cursor does not change the bytes")
	ResetRequestBody(req, replacement)
	require.Nil(t, ChatFieldsForBody(req, replacement))
}

func TestIndexedChatBodyStillDetectsDuplicateMembers(t *testing.T) {
	for _, tc := range []struct{ body, field string }{
		{`{"tools":[],"\u0054OOLS":[]}`, `TOOLS`},
		{`{"tools":[{"function":{"name":"a","Name":"b"}}]}`, `tools[0].function.Name`},
		{`{"tool_choice":{"type":"none","type":"auto"}}`, `tool_choice.type`},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		body := []byte(tc.body)
		field, conflict := ResetAndCheckChatBody(req, body)
		require.True(t, conflict)
		require.Equal(t, tc.field, field)
		require.Nil(t, ChatFieldsForBody(req, body))
	}
}
