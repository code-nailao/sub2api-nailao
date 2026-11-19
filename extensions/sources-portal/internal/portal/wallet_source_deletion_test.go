package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type deletionTrace struct {
	mu     sync.Mutex
	events []string
}

func (d *deletionTrace) add(event string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, event)
}
func (d *deletionTrace) read() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.events...)
}

func deletionBridgeFixture(t *testing.T, reserve func(*http.Request, Source) (*http.Response, error)) (*Server, Source, *deletionTrace) {
	t.Helper()
	svc, src, _ := balanceFixture(t)
	trace := &deletionTrace{}
	s := &Server{Service: svc, SubOrigin: "https://main.example", PublicOrigin: "https://portal.example"}
	s.AuthClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer fixture-admin" {
			t.Error("native admin session was not forwarded")
			return reply(401, ""), nil
		}
		if r.Method == "GET" && r.URL.Path == "/api/v1/auth/me" {
			return reply(200, `{"code":0,"data":{"id":9,"role":"admin","status":"active"}}`), nil
		}
		if r.Method != http.MethodPost || r.URL.RequestURI() != walletSourceDeletionPath {
			t.Errorf("unreviewed native request: %s %s", r.Method, r.URL.RequestURI())
			return reply(404, ""), nil
		}
		deadline, bounded := r.Context().Deadline()
		if !bounded || time.Until(deadline) > 8*time.Second {
			t.Error("native deletion request was not bounded")
		}
		var input struct {
			SourceID string `json:"source_id"`
			Revision int64  `json:"source_revision"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.SourceID != src.ID || input.Revision != src.Revision {
			t.Error("reservation source/revision differs from local request")
			return reply(409, ""), nil
		}
		trace.add("reserve")
		if reserve != nil {
			return reserve(r, src)
		}
		return deletionReservationReply(src), nil
	})}
	svc.Bridge = NewWalletBridge(s.SubOrigin, "metadata-only-capability")
	svc.Bridge.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.Path != "/internal/sources-wallet/snapshot" || r.Header.Get("Authorization") != "Bearer metadata-only-capability" {
			t.Error("wrong metadata capability or endpoint")
			return reply(401, ""), nil
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil || bytes.Contains(raw, []byte("personal-fixture")) || bytes.Contains(raw, []byte("encrypted_token")) {
			t.Error("invalid metadata snapshot or leaked credential")
		}
		var snapshot struct {
			Sources []Source `json:"sources"`
		}
		if json.Unmarshal(raw, &snapshot) != nil {
			t.Error("invalid snapshot")
		}
		if len(snapshot.Sources) == 0 {
			trace.add("snapshot:absent")
		} else if len(snapshot.Sources) == 1 && snapshot.Sources[0].ID == src.ID {
			trace.add("snapshot:present")
		} else {
			t.Error("unexpected source inventory")
		}
		return reply(200, `{"code":0,"data":{"synchronized":true}}`), nil
	})
	return s, src, trace
}

func deletionReservationReply(src Source) *http.Response {
	return reply(200, fmt.Sprintf(`{"code":0,"data":{"source_id":%q,"source_revision":%d,"reserved":true}}`, src.ID, src.Revision))
}

func deleteSourceRequest(s *Server, src Source) *httptest.ResponseRecorder {
	return request(s, http.MethodDelete, "/api/balance/sources/"+src.ID, fmt.Sprintf(`{"revision":%d}`, src.Revision), "fixture-admin", s.PublicOrigin)
}

func TestWalletSourceDeletionHonorsCanonicalLinks(t *testing.T) {
	s, src, trace := deletionBridgeFixture(t, func(*http.Request, Source) (*http.Response, error) {
		return reply(409, `{"code":409,"reason":"wallet_source_linked","message":"private server context"}`), nil
	})
	before, err := os.ReadFile(s.Service.Store.path)
	if err != nil {
		t.Fatal(err)
	}
	res := deleteSourceRequest(s, src)
	if res.Code != 409 || !strings.Contains(res.Body.String(), ErrSourceLinked.Error()) || strings.Contains(res.Body.String(), "private") {
		t.Fatalf("%d %s", res.Code, res.Body)
	}
	after, err := os.ReadFile(s.Service.Store.path)
	if err != nil || !bytes.Equal(before, after) || len(s.Service.List()) != 1 {
		t.Fatal("linked source changed", err)
	}
	if !reflect.DeepEqual(trace.read(), []string{"snapshot:present", "reserve"}) {
		t.Fatal(trace.read())
	}
}

func TestWalletSourceDeletionAllowsCanonicalUnlinkAndPreservesLegacyFile(t *testing.T) {
	s, src, trace := deletionBridgeFixture(t, nil)
	marker := "v1:" + strings.Repeat("a", 64)
	if err := s.Service.Store.setBinding(s.SubOrigin, 1, BalanceAccountIdentity{1, true, marker}, src.ID, src.Revision, 0); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(s.Service.Store.path), "account-bindings.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	res := deleteSourceRequest(s, src)
	if res.Code != http.StatusNoContent || len(s.Service.List()) != 0 {
		t.Fatalf("%d %s", res.Code, res.Body)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("legacy binding file changed", err)
	}
	if !reflect.DeepEqual(trace.read(), []string{"snapshot:present", "reserve", "snapshot:absent"}) {
		t.Fatal(trace.read())
	}
}

func TestWalletSourceDeletionRequiresMatchingReservation(t *testing.T) {
	for _, kind := range []string{"denied", "wrong_source", "wrong_revision", "unknown_response", "transport"} {
		t.Run(kind, func(t *testing.T) {
			s, src, _ := deletionBridgeFixture(t, func(_ *http.Request, source Source) (*http.Response, error) {
				switch kind {
				case "denied":
					return reply(200, `{"code":0,"data":{"reserved":false}}`), nil
				case "wrong_source":
					source.ID = "another-source"
				case "wrong_revision":
					source.Revision++
				case "unknown_response":
					return reply(200, `{"code":0,"data":{}}`), nil
				case "transport":
					return nil, context.DeadlineExceeded
				}
				return deletionReservationReply(source), nil
			})
			if err := s.Service.Delete(src.ID, src.Revision); !errors.Is(err, ErrSourceDeletionReservation) {
				t.Fatalf("unreserved Service.Delete was accepted: %v", err)
			}
			res := deleteSourceRequest(s, src)
			if res.Code < 400 || len(s.Service.List()) != 1 {
				t.Fatalf("deleted without matching reservation: %d %s", res.Code, res.Body)
			}
		})
	}
}

func TestWalletSourceDeletionConcurrentEditFailsLocalCAS(t *testing.T) {
	reserved, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(resume) }) })
	s, src, trace := deletionBridgeFixture(t, func(r *http.Request, source Source) (*http.Response, error) {
		close(reserved)
		select {
		case <-resume:
			return deletionReservationReply(source), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- deleteSourceRequest(s, src) }()
	select {
	case <-reserved:
	case <-time.After(5 * time.Second):
		t.Fatal("reservation did not start")
	}
	edit := input()
	edit.Name = "Edited while deletion was reserved"
	edit.Revision = src.Revision
	edit.PersonalToken = ""
	updated, err := s.Service.Save(src.ID, edit, 9)
	if err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(resume) })
	res := <-done
	if res.Code != http.StatusConflict || updated.Revision != src.Revision+1 {
		t.Fatalf("%d %s %+v", res.Code, res.Body, updated)
	}
	current := s.Service.List()
	if len(current) != 1 || current[0].Name != edit.Name || current[0].Revision != updated.Revision {
		t.Fatal("concurrent edit was deleted", current)
	}
	if !reflect.DeepEqual(trace.read(), []string{"snapshot:present", "reserve"}) {
		t.Fatal(trace.read())
	}
}

func TestWalletSourceDeletionPersistenceFailureDoesNotReleaseReservation(t *testing.T) {
	var s *Server
	failedPath := filepath.Join(t.TempDir(), "missing", "sources.json")
	server, src, trace := deletionBridgeFixture(t, func(_ *http.Request, source Source) (*http.Response, error) {
		s.Service.Store.mu.Lock()
		s.Service.Store.path = failedPath
		s.Service.Store.mu.Unlock()
		return deletionReservationReply(source), nil
	})
	s = server
	originalPath := s.Service.Store.path
	before, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	res := deleteSourceRequest(s, src)
	after, err := os.ReadFile(originalPath)
	if res.Code < 400 || err != nil || !bytes.Equal(before, after) || len(s.Service.List()) != 1 {
		t.Fatalf("failed persistence changed source: %d %s %v", res.Code, res.Body, err)
	}
	if !reflect.DeepEqual(trace.read(), []string{"snapshot:present", "reserve"}) {
		t.Fatal(trace.read())
	}
}

func TestWalletSourceDeletionForwardingHasExactMethodAndPath(t *testing.T) {
	s := testServer(t, "admin")
	s.AuthClient.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		t.Error("unreviewed deletion request was forwarded")
		return reply(500, ""), nil
	})
	r := httptest.NewRequest(http.MethodDelete, "/api/balance/sources/source", nil)
	for _, pair := range [][2]string{
		{http.MethodGet, walletSourceDeletionPath}, {http.MethodPut, walletSourceDeletionPath},
		{http.MethodDelete, walletSourceDeletionPath}, {http.MethodPost, walletSourceDeletionPath + "/"},
		{http.MethodPost, walletSourceDeletionPath + "?source_id=other"},
		{http.MethodPost, walletSourceDeletionPath + "/../wallet-settings"},
	} {
		if err := s.mainAPI(r, pair[0], pair[1], nil, "", nil); !errors.Is(err, ErrCredential) {
			t.Fatalf("%v accepted: %v", pair, err)
		}
	}
}
