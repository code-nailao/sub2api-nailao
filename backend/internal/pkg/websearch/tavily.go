package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const (
	tavilySearchEndpoint   = "https://api.tavily.com/search"
	tavilyProviderName     = "tavily"
	tavilySearchDepthBasic = "basic"
)

// TavilyProvider implements web search via the Tavily Search API.
type TavilyProvider struct {
	apiKey     string
	httpClient *http.Client
}

// NewTavilyProvider creates a Tavily Search provider.
// The caller is responsible for configuring the http.Client with proxy/timeouts.
func NewTavilyProvider(apiKey string, httpClient *http.Client) *TavilyProvider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &TavilyProvider{apiKey: apiKey, httpClient: httpClient}
}

func (t *TavilyProvider) Name() string { return tavilyProviderName }

func (t *TavilyProvider) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}

	payload := tavilyRequest{
		APIKey:      t.apiKey,
		Query:       req.Query,
		MaxResults:  maxResults,
		SearchDepth: tavilySearchDepthBasic,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("tavily: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, tavilySearchEndpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("tavily: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	var raw tavilyResponse
	if err := searchJSON(t.httpClient, httpReq, tavilyProviderName, &raw); err != nil {
		return nil, err
	}

	results := make([]SearchResult, 0, len(raw.Results))
	for _, r := range raw.Results {
		results = append(results, SearchResult{
			URL:     r.URL,
			Title:   r.Title,
			Snippet: r.Content,
		})
	}

	return &SearchResponse{Results: results, Query: req.Query}, nil
}

type tavilyRequest struct {
	APIKey      string `json:"api_key"`
	Query       string `json:"query"`
	MaxResults  int    `json:"max_results"`
	SearchDepth string `json:"search_depth"`
}

type tavilyResponse struct {
	Results []tavilyResult `json:"results"`
}

type tavilyResult struct {
	URL     string  `json:"url"`
	Title   string  `json:"title"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}
