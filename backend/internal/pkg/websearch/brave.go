package websearch

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

const (
	braveSearchEndpoint = "https://api.search.brave.com/res/v1/web/search"
	braveMaxCount       = 20
	braveProviderName   = "brave"
)

// braveSearchURL is pre-parsed at init time; url.Parse cannot fail on a constant literal.
var braveSearchURL, _ = url.Parse(braveSearchEndpoint) //nolint:errcheck

// BraveProvider implements web search via the Brave Search API.
type BraveProvider struct {
	apiKey     string
	httpClient *http.Client
}

// NewBraveProvider creates a Brave Search provider.
// The caller is responsible for configuring the http.Client with proxy/timeouts.
func NewBraveProvider(apiKey string, httpClient *http.Client) *BraveProvider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &BraveProvider{apiKey: apiKey, httpClient: httpClient}
}

func (b *BraveProvider) Name() string { return braveProviderName }

func (b *BraveProvider) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	count := req.MaxResults
	if count <= 0 {
		count = defaultMaxResults
	}
	if count > braveMaxCount {
		count = braveMaxCount
	}

	u := *braveSearchURL // copy the pre-parsed URL
	q := u.Query()
	q.Set("q", req.Query)
	q.Set("count", strconv.Itoa(count))
	u.RawQuery = q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("brave: build request: %w", err)
	}
	httpReq.Header.Set("X-Subscription-Token", b.apiKey)
	httpReq.Header.Set("Accept", "application/json")

	var raw braveResponse
	if err := searchJSON(b.httpClient, httpReq, braveProviderName, &raw); err != nil {
		return nil, err
	}

	results := make([]SearchResult, 0, len(raw.Web.Results))
	for _, r := range raw.Web.Results {
		results = append(results, SearchResult{
			URL:     r.URL,
			Title:   r.Title,
			Snippet: r.Description,
			PageAge: r.Age,
		})
	}

	return &SearchResponse{Results: results, Query: req.Query}, nil
}

// braveResponse is the minimal structure of the Brave Search API response.
type braveResponse struct {
	Web struct {
		Results []braveResult `json:"results"`
	} `json:"web"`
}

type braveResult struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Age         string `json:"age"`
}
