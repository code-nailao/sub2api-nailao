package websearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type exaRoundTripFunc func(*http.Request) (*http.Response, error)

func (f exaRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func exaTestClient(status int, body string, seen *http.Request, seenBody *[]byte) *http.Client {
	return &http.Client{Transport: exaRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		*seen = *r
		b, _ := io.ReadAll(r.Body)
		*seenBody = b
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}
}

func TestExaProvider_Name(t *testing.T) {
	require.Equal(t, "exa", NewExaProvider("key", nil).Name())
}

func TestManagerBuildsExaProvider(t *testing.T) {
	p := (&Manager{}).buildProvider(ProviderConfig{Type: ProviderTypeExa, APIKey: "k"}, nil)
	require.IsType(t, &ExaProvider{}, p)
}

func TestExaProvider_Search(t *testing.T) {
	var seen http.Request
	var seenBody []byte
	resp := `{"requestId":"r1","results":[` +
		`{"url":"https://go.dev","title":"Go","publishedDate":"2026-10-01T00:00:00.000Z","highlights":["Go is a language.","It compiles fast."]},` +
		`{"url":"https://example.com","title":"Example","publishedDate":null,"highlights":[]}]}`
	p := NewExaProvider("exa-key", exaTestClient(http.StatusOK, resp, &seen, &seenBody))

	out, err := p.Search(context.Background(), SearchRequest{Query: "golang", MaxResults: 3})

	require.NoError(t, err)
	require.Equal(t, http.MethodPost, seen.Method)
	require.Equal(t, "https://api.exa.ai/search", seen.URL.String())
	require.Equal(t, "exa-key", seen.Header.Get("x-api-key"))
	require.Equal(t, "application/json", seen.Header.Get("Content-Type"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(seenBody, &body))
	require.Equal(t, map[string]any{
		"query":      "golang",
		"numResults": float64(3),
		"contents":   map[string]any{"highlights": true},
	}, body)

	require.Equal(t, "golang", out.Query)
	require.Equal(t, []SearchResult{
		{URL: "https://go.dev", Title: "Go", Snippet: "Go is a language.\nIt compiles fast.", PageAge: "2026-10-01T00:00:00.000Z"},
		{URL: "https://example.com", Title: "Example"},
	}, out.Results)
}

func TestExaProvider_SearchDefaultsNumResults(t *testing.T) {
	var seen http.Request
	var seenBody []byte
	p := NewExaProvider("k", exaTestClient(http.StatusOK, `{"results":[]}`, &seen, &seenBody))

	_, err := p.Search(context.Background(), SearchRequest{Query: "q"})

	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(seenBody, &body))
	require.Equal(t, float64(defaultMaxResults), body["numResults"])
}

func TestExaProvider_SearchHTTPError(t *testing.T) {
	var seen http.Request
	var seenBody []byte
	p := NewExaProvider("bad", exaTestClient(http.StatusUnauthorized, `{"error":"invalid api key"}`, &seen, &seenBody))

	_, err := p.Search(context.Background(), SearchRequest{Query: "q"})

	require.ErrorContains(t, err, "exa: status 401")
}

func TestExaProviderDoesNotFollowCrossOriginRedirect(t *testing.T) {
	for _, test := range []struct {
		name     string
		location func(*http.Request) string
	}{
		{"host", func(*http.Request) string { return "https://other.example/search" }},
		{"port", func(r *http.Request) string { return "https://" + r.URL.Hostname() + ":444/search" }},
		{"scheme", func(r *http.Request) string { return "http://" + r.URL.Host + "/search" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: exaRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				if requests == 1 {
					return &http.Response{
						StatusCode: http.StatusTemporaryRedirect,
						Header:     http.Header{"Location": {test.location(r)}},
						Body:       http.NoBody,
					}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"results":[]}`))}, nil
			})}
			provider := NewExaProvider("fixture-key", client)

			_, err := provider.Search(t.Context(), SearchRequest{Query: "fixture"})

			require.Equal(t, 1, requests, "no credential-bearing request should reach another origin")
			require.Error(t, err)
			require.Nil(t, client.CheckRedirect, "the provider must leave the shared client unchanged")
		})
	}
}

func TestExaProviderFollowsSameOriginRedirect(t *testing.T) {
	var paths []string
	client := &http.Client{Transport: exaRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		require.Equal(t, "fixture-key", r.Header.Get("x-api-key"))
		if len(paths) == 1 {
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": {"https://" + r.URL.Hostname() + ":443/redirected"}},
				Body:       http.NoBody,
			}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"results":[]}`))}, nil
	})}

	_, err := NewExaProvider("fixture-key", client).Search(t.Context(), SearchRequest{Query: "fixture"})

	require.NoError(t, err)
	require.Equal(t, []string{"/search", "/redirected"}, paths)
}
