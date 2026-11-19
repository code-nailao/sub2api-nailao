package mediaengine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transport func(*http.Request, Credential) (*http.Response, error)

func (t transport) Do(r *http.Request, c Credential) (*http.Response, error) { return t(r, c) }
func TestProtocolLogicalImageAndStatusAliases(t *testing.T) {
	for _, status := range []string{"completed", "complete", "succeeded", "success", "done"} {
		client := Client{Transport: transport(func(r *http.Request, c Credential) (*http.Response, error) {
			if r.URL.String() != "https://vendor.example/v1/videos/ref_1" || r.Header.Get("Authorization") != "Bearer test" {
				t.Fatal("bad request")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"` + status + `","url":"https://cdn.example/image?a=1","content_url":"https://cdn.example/image?a=2","data":[{"url":"/owned-result?a=3"}]}`))}, nil
		})}
		p, err := client.Poll(context.Background(), Credential{BaseURL: "https://vendor.example/v1/", APIKey: "test"}, "ref_1")
		if err != nil || len(p.URLs) != 1 || p.URLs[0] != "https://vendor.example/owned-result?a=3" {
			t.Fatalf("%+v %v", p, err)
		}
	}
}
func TestProtocolRejectsBadEndpointsAndMultipleResults(t *testing.T) {
	for _, base := range []string{"http://vendor.example", "https://secret@vendor.example", "https://vendor.example/v1beta", "https://vendor.example/?q=1"} {
		if _, err := endpoint(base, ""); err == nil {
			t.Fatal(base)
		}
	}
	for _, body := range []string{`{"status":"done","data":[{"url":"https://a.example/1"},{"url":"https://a.example/2"}]}`, `{"status":"done","url":"file:///etc/passwd"}`, `{"status":"unknown","url":"https://a.example/1"}`} {
		c := Client{Transport: transport(func(*http.Request, Credential) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		if _, err := c.Poll(context.Background(), Credential{BaseURL: "https://vendor.example", APIKey: "test"}, "ref_1"); err == nil {
			t.Fatal(body)
		}
	}
}

func TestSubmitDisablesTransportReplayAndPreservesDeferral(t *testing.T) {
	client := Client{Transport: transport(func(r *http.Request, _ Credential) (*http.Response, error) {
		if r.Method != "POST" || r.GetBody != nil || r.Header.Get("Idempotency-Key") == "" {
			t.Fatal("unsafe transport replay")
		}
		return nil, ErrDeferred
	})}
	_, err := client.Submit(context.Background(), Credential{BaseURL: "https://vendor.example", APIKey: "test"}, []byte(`{"model":"test"}`), "idem")
	if !errors.Is(err, ErrDeferred) {
		t.Fatalf("%v", err)
	}
}
func TestKnownRejectionVersusAmbiguousFailure(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 413, 422, 429, 500, 502} {
		client := Client{Transport: transport(func(*http.Request, Credential) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"error":"unavailable"}`))}, nil
		})}
		_, err := client.Submit(context.Background(), Credential{BaseURL: "https://vendor.example", APIKey: "test"}, []byte(`{}`), "idem")
		known := status != 429 && status < 500
		if err == nil || errors.Is(err, ErrRejected) != known {
			t.Fatalf("status=%d err=%v", status, err)
		}
	}
}
