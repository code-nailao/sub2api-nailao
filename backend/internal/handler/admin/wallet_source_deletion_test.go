package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type walletDeletionRegistry struct {
	state   service.WalletRegistry
	calls   int
	bounded bool
}

func (r *walletDeletionRegistry) Read(context.Context) (service.WalletRegistry, error) {
	return r.state, nil
}
func (r *walletDeletionRegistry) Change(ctx context.Context, apply func(*service.WalletRegistry) error) error {
	r.calls++
	deadline, ok := ctx.Deadline()
	r.bounded = ok && time.Until(deadline) > 0 && time.Until(deadline) <= 5*time.Second
	raw, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	var next service.WalletRegistry
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	if err := apply(&next); err != nil {
		return err
	}
	r.state = next
	return nil
}

func TestWalletSourceDeletionHandlerBoundsAndAcknowledgesExactReservation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name, body    string
		linked        bool
		status, calls int
	}{
		{"valid", `{"source_id":"source1","source_revision":3}`, false, 200, 1},
		{"invalid_id", `{"source_id":"","source_revision":3}`, false, 400, 0},
		{"invalid_revision", `{"source_id":"source1","source_revision":0}`, false, 400, 0},
		{"stale_revision", `{"source_id":"source1","source_revision":2}`, false, 409, 1},
		{"linked", `{"source_id":"source1","source_revision":3}`, true, 409, 1},
		{"forged_reservation", `{"source_id":"source1","source_revision":3,"reserved":true}`, false, 400, 0},
		{"multiple_objects", `{"source_id":"source1","source_revision":3}{}`, false, 400, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &walletDeletionRegistry{state: service.WalletRegistry{PortalImported: true, PortalSeenAt: time.Now().UTC()}}
			repo.state.Initialize()
			repo.state.Sources["source1"] = service.PortalWalletSource{ID: "source1", Revision: 3}
			if test.linked {
				repo.state.Profiles["new:source1"] = service.ManagedWallet{ID: "new:source1", SourceID: "source1", Provider: "new"}
				repo.state.Members[7] = service.WalletMember{WalletID: "new:source1"}
			}
			reader := service.NewAccountWalletReader(nil)
			t.Cleanup(reader.Stop)
			reader.Manager = &service.WalletManager{Repo: repo}
			h := &AccountHandler{automaticWallet: reader}
			router := gin.New()
			router.POST("/wallet-source-deletion", h.ReserveManagedWalletSourceDeletion)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/wallet-source-deletion", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(response, request)
			require.Equal(t, test.status, response.Code, response.Body.String())
			require.Equal(t, test.calls, repo.calls)
			if repo.calls > 0 {
				require.True(t, repo.bounded)
			}
			if test.status == 200 {
				var result struct {
					Data service.WalletSourceDeletion `json:"data"`
				}
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
				require.Equal(t, service.WalletSourceDeletion{SourceID: "source1", SourceRevision: 3, Reserved: true}, result.Data)
				require.Equal(t, int64(3), repo.state.Sources["source1"].DeletionRevision)
			} else {
				require.Zero(t, repo.state.Sources["source1"].DeletionRevision)
			}
		})
	}
}
