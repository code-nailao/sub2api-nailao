//go:build unit

package service

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDeepSeekFailedToolContinuesWithoutServerTools(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, failure := range []string{"http502", "transport", "invalid_json", "empty"} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", protocol, stream, failure), func(t *testing.T) {
					models, tools := 0, 0
					result, journal, response, err := deepSeekHostedProtocolFixture(t, protocol, map[string]any{"stream": stream}, func(r *http.Request) (*http.Response, error) {
						if strings.Contains(r.URL.Path, "/tools/") {
							tools++
							switch failure {
							case "transport":
								return nil, errors.New("fixture transport error")
							case "http502":
								res := deepSeekFixtureResponse(`{"error":{"message":"invalid JSON"}}`)
								res.StatusCode = 502
								return res, nil
							case "invalid_json":
								return deepSeekFixtureResponse("invalid"), nil
							default:
								return deepSeekFixtureResponse(`{"results":[]}`), nil
							}
						}
						models++
						if models == 1 {
							return deepSeekFixtureModelResponse(t, stream, deepSeekModelFixtureBody(t, r, "call_1", "Python")), nil
						}
						raw, e := io.ReadAll(r.Body)
						require.NoError(t, e)
						require.Empty(t, gjson.GetBytes(raw, "tools").Array(), "failed hosted tools must not be offered again in this request")
						require.Contains(t, gjson.GetBytes(raw, "messages").String(), "unavailable")
						require.Contains(t, gjson.GetBytes(raw, "messages").String(), "Do not claim")
						r.Body = io.NopCloser(strings.NewReader(string(raw)))
						return deepSeekFixtureModelResponse(t, stream, deepSeekModelFixtureBody(t, r, "", "")), nil
					})
					require.NoError(t, err)
					require.Equal(t, 2, models)
					require.Equal(t, 1, tools)
					require.Zero(t, result.DeepSeekTools.Cost())
					require.Len(t, journal.checkpoints, 2)
					require.Contains(t, response, "Final answer")
					require.NotContains(t, response, "continuation failed")
				})
			}
		}
	}
}

func TestDeepSeekFirstHTTPFailureFallsBackOnceWithoutHostedTools(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", protocol, stream), func(t *testing.T) {
				models := 0
				result, _, response, err := deepSeekHostedProtocolFixture(t, protocol, map[string]any{"stream": stream}, func(r *http.Request) (*http.Response, error) {
					models++
					raw, e := io.ReadAll(r.Body)
					require.NoError(t, e)
					if models == 1 {
						require.NotEmpty(t, gjson.GetBytes(raw, "tools").Array())
						res := deepSeekFixtureResponse(`{"error":{"message":"Service unavailable"}}`)
						res.StatusCode = 502
						return res, nil
					}
					require.Empty(t, gjson.GetBytes(raw, "tools").Array())
					require.False(t, gjson.GetBytes(raw, "tool_choice").Exists(), "plain fallback must omit tool_choice")
					r.Body = io.NopCloser(strings.NewReader(string(raw)))
					return deepSeekFixtureModelResponse(t, stream, deepSeekModelFixtureBody(t, r, "", "")), nil
				})
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 2, models)
				require.Contains(t, response, "Final answer")
			})
		}
	}

}
