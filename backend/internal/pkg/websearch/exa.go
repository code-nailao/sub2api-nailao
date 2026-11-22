package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	exaSearchEndpoint = "https://api.exa.ai/search"
	exaProviderName   = "exa"
)

// ExaProvider implements web search via the Exa Search API.
type ExaProvider struct {
	apiKey     string
	httpClient *http.Client
}

// NewExaProvider creates an Exa Search provider.
// The caller is responsible for configuring the http.Client with proxy/timeouts.
func NewExaProvider(apiKey string, httpClient *http.Client) *ExaProvider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	// Other providers may share this client. Keep the transport, timeout and
	// any caller-supplied redirect policy while restricting Exa's credentials.
	client := *httpClient
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if previousRedirect != nil {
			if err := previousRedirect(req, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		origin := via[0].URL
		if req.URL.Scheme != origin.Scheme || !strings.EqualFold(req.URL.Hostname(), origin.Hostname()) ||
			(req.URL.Port() != "" && req.URL.Port() != "443") || req.URL.User != nil {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &ExaProvider{apiKey: apiKey, httpClient: &client}
}

func (e *ExaProvider) Name() string { return exaProviderName }

func (e *ExaProvider) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	numResults := req.MaxResults
	if numResults <= 0 {
		numResults = defaultMaxResults
	}

	// Search type and highlight length are left to Exa's defaults.
	payload := exaRequest{
		Query:      req.Query,
		NumResults: numResults,
		Contents:   exaContents{Highlights: true},
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("exa: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, exaSearchEndpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("exa: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", e.apiKey)

	var raw exaResponse
	if err := searchJSON(e.httpClient, httpReq, exaProviderName, &raw); err != nil {
		return nil, err
	}

	results := make([]SearchResult, 0, len(raw.Results))
	for _, r := range raw.Results {
		results = append(results, SearchResult{
			URL:     r.URL,
			Title:   r.Title,
			Snippet: strings.Join(r.Highlights, "\n"),
			PageAge: r.PublishedDate,
		})
	}

	return &SearchResponse{Results: results, Query: req.Query}, nil
}

type exaRequest struct {
	Query      string      `json:"query"`
	NumResults int         `json:"numResults"`
	Contents   exaContents `json:"contents"`
}

type exaContents struct {
	Highlights bool `json:"highlights"`
}

type exaResponse struct {
	Results []exaResult `json:"results"`
}

type exaResult struct {
	URL           string   `json:"url"`
	Title         string   `json:"title"`
	PublishedDate string   `json:"publishedDate"`
	Highlights    []string `json:"highlights"`
}
