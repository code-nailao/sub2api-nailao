package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/processrole"
	"github.com/stretchr/testify/require"
	"sources.local/mediaengine"
)

func TestSourcesAdmissionProducerJournalDoesNotRequireRetiredLegacyOrigin(t *testing.T) {
	for _, mode := range []string{processrole.AdmissionEnabled, processrole.AdmissionHeld, processrole.AdmissionLegacy} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Chmod(dir, 0700))
			path := filepath.Join(dir, "admission.json")
			raw, err := json.Marshal(processrole.AdmissionState{Schema: 1, Revision: 7, Mode: mode, HoldToken: strings.Repeat("a", 40)})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, raw, 0600))
			gate, legacy, err := openSourcesAdmission(path, "")
			if mode == processrole.AdmissionLegacy {
				require.Error(t, err)
				require.Nil(t, gate)
				gate, legacy, err = openSourcesAdmission(path, "http://127.0.0.1:8080")
				require.NoError(t, err)
				require.NotNil(t, legacy)
			} else {
				require.NoError(t, err)
				require.Nil(t, legacy)
			}
			require.Equal(t, mode, gate.Status().Mode)
			require.Equal(t, uint64(7), gate.Status().Revision)
			require.NoError(t, gate.Close())
		})
	}
}

func TestSourcesLegacyAdmissionRejectsNonPrivateOrUnfixedDestinations(t *testing.T) {
	for _, origin := range []string{"http://example.com:8080", "http://1.1.1.1:8080", "https://127.0.0.1:8080", "http://127.0.0.1", "http://a:b@127.0.0.1:8080", "http://127.0.0.1:8080/path", "http://127.0.0.1:8080?next=foo"} {
		_, err := newSourcesLegacyAdmission(origin)
		require.Error(t, err, origin)
	}
}

func TestSourcesLegacyAdmissionConfirmsDurableTaskAndRestrictsForwardedHeaders(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	store, err := mediaengine.Open(dir, bytes.Repeat([]byte{7}, 32), mediaengine.Namespace{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	producer, err := mediaengine.NewProducer(store, mediaengine.Options{})
	require.NoError(t, err)
	group := int64(3)
	key := &APIKey{ID: 2, UserID: 1, GroupID: &group}
	body := []byte(`{"model":"gpt-image-2","prompt":"fixture"}`)
	parsed, err := parseSourcesMediaEngineRequest("/v1/images/generations/async", "application/json", body)
	require.NoError(t, err)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/images/generations/async" || r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("X-Forwarded-For") != "192.0.2.5" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Operator-Token") != "" {
			t.Error("legacy bridge changed credentials, destination or leaked unrelated headers")
		}
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Equal(raw, body) {
			t.Error("request body changed")
		}
		task, createErr := producer.Create(mediaengine.Input{Owner: mediaengine.Owner{UserID: 1, KeyID: 2, GroupID: 3}, IdempotencyKey: r.Header.Get("Idempotency-Key"), PayloadHash: parsed.Hash, Body: body, Ticket: []byte(`{}`), Credential: mediaengine.Credential{AccountID: 4}})
		if createErr != nil {
			t.Error(createErr)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Location", "/v1/images/tasks/"+task.ID)
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": task.ID, "task_id": task.ID})
	}))
	defer server.Close()
	legacy, err := newSourcesLegacyAdmission(server.URL)
	require.NoError(t, err)
	s := &SourcesMediaEngine{store: store, legacyAdmission: legacy}
	headers := http.Header{"Authorization": {"Bearer fixture"}, "Content-Type": {"application/json"}, "Cookie": {"private-session"}, "X-Forwarded-For": {"spoofed"}, "X-Operator-Token": {"never-forward"}}
	result, known, err := s.ForwardLegacyAdmission(context.Background(), key, "/v1/images/generations/async", headers, body, "same-idempotency", "192.0.2.5")
	require.NoError(t, err)
	require.True(t, known)
	require.Equal(t, 202, result.Status)
	require.Equal(t, int32(1), calls.Load())
	require.Contains(t, result.Headers["Location"], "mediatask_")
}

func TestSourcesLegacyAdmissionDoesNotRetryUnknownPostOrFollowRedirect(t *testing.T) {
	for _, outcome := range []string{"disconnect", "redirect", "failure"} {
		t.Run(outcome, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				switch outcome {
				case "disconnect":
					conn, _, err := http.NewResponseController(w).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
				case "redirect":
					w.Header().Set("Location", "/unexpected")
					w.WriteHeader(http.StatusTemporaryRedirect)
					_, _ = w.Write([]byte(`{"error":"redirect"}`))
				default:
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"error":"outcome unknown"}`))
				}
			}))
			defer server.Close()
			legacy, err := newSourcesLegacyAdmission(server.URL)
			require.NoError(t, err)
			group := int64(3)
			s := &SourcesMediaEngine{legacyAdmission: legacy}
			_, known, _ := s.ForwardLegacyAdmission(context.Background(), &APIKey{ID: 2, UserID: 1, GroupID: &group}, "/v1/images/generations/async", http.Header{"Content-Type": {"application/json"}}, []byte(`{"prompt":"test"}`), "never-replay", "192.0.2.1")
			require.False(t, known)
			require.Equal(t, int32(1), calls.Load())
		})
	}
}
