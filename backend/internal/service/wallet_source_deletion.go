package service

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var ErrWalletSourceLinked = infraerrors.Conflict("wallet_source_linked", "Unlink every channel from this source before deleting it")
var ErrWalletSourceDeleting = infraerrors.Conflict("wallet_source_delete_reserved", "Source deletion is reserved; finish deletion or edit the source before linking it")

type WalletSourceDeletion struct {
	SourceID       string `json:"source_id"`
	SourceRevision int64  `json:"source_revision"`
	Reserved       bool   `json:"reserved"`
}

// ReserveSourceDeletion serializes source deletion against both native wallet
// membership edits and Portal bindings. The source stays mirrored until Portal
// durably deletes this exact revision. An unknown local outcome never releases
// the reservation: an explicit retry or a newer source edit can recover it.
func (m *WalletManager) ReserveSourceDeletion(ctx context.Context, sourceID string, revision int64) (WalletSourceDeletion, error) {
	var result WalletSourceDeletion
	if sourceID == "" || len(sourceID) > 80 || revision <= 0 {
		return result, ErrWalletSetup
	}
	err := m.Repo.Change(ctx, func(state *WalletRegistry) error {
		if !state.PortalImported || time.Since(state.PortalSeenAt) > 2*time.Minute {
			return ErrWalletBridge
		}
		source, ok := state.Sources[sourceID]
		if !ok || source.Revision != revision {
			return ErrWalletRegistryConflict
		}
		for _, member := range state.Members {
			if member.WalletID == "" {
				continue
			}
			wallet := state.Profiles[member.WalletID]
			if wallet.SourceID == sourceID || member.WalletID == "new:"+sourceID {
				return ErrWalletSourceLinked
			}
		}
		if source.DeletionRevision != 0 && source.DeletionRevision != revision {
			return ErrWalletRegistryConflict
		}
		source.DeletionRevision = revision
		state.Sources[sourceID] = source
		result = WalletSourceDeletion{SourceID: sourceID, SourceRevision: revision, Reserved: true}
		return nil
	})
	return result, err
}

// mergePortalWalletSource protects Main-owned deletion state from metadata
// synchronization. Same-revision refreshes may update balances but cannot revoke
// a reservation. Older snapshots cannot roll source identity/revision backward.
// A strictly newer revision can clear it because Portal's old delete must then
// fail its durable-store revision check.
func mergePortalWalletSource(previous, incoming PortalWalletSource) PortalWalletSource {
	incoming.DeletionRevision = 0
	if previous.ID != incoming.ID {
		return incoming
	}
	if incoming.Revision < previous.Revision {
		return previous
	}
	if incoming.Revision <= previous.DeletionRevision {
		incoming.DeletionRevision = previous.DeletionRevision
	}
	return incoming
}
