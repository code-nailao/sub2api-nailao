package service

import (
	"context"
	"strconv"
	"strings"
	"time"
)

type WalletAccountSummary struct {
	AccountID       int64  `json:"account_id"`
	Supported       bool   `json:"supported"`
	Status          string `json:"status"`
	SourceID        string `json:"source_id,omitempty"`
	SourceName      string `json:"source_name,omitempty"`
	WalletID        string `json:"wallet_id,omitempty"`
	Provider        string `json:"provider,omitempty"`
	Automatic       bool   `json:"automatic,omitempty"`
	BindingRevision int64  `json:"binding_revision"`
	SharedCount     int    `json:"shared_count"`
	Balance         *struct {
		AccountID int64     `json:"account_id"`
		Remaining string    `json:"remaining"`
		Used      string    `json:"used"`
		Unit      string    `json:"unit"`
		CheckedAt time.Time `json:"checked_at"`
	} `json:"balance"`
	LastCheckedAt *time.Time `json:"last_checked_at"`
	Stale         bool       `json:"stale"`
	Error         string     `json:"error,omitempty"`
	Querying      bool       `json:"querying,omitempty"`
	AutoRefresh   bool       `json:"auto_refresh"`
	NextCheckAt   *time.Time `json:"next_check_at,omitempty"`
}

func (m *WalletManager) Summaries(ctx context.Context, ids []int64) ([]WalletAccountSummary, error) {
	state, err := m.Repo.Read(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := m.accounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	rows := []WalletAccountSummary{}
	for _, id := range ids {
		a := accounts[id]
		member := state.Members[id]
		item := WalletAccountSummary{AccountID: id, Supported: walletAccountSupported(a), Status: "unbound", BindingRevision: member.Revision}
		if a == nil {
			item.Status = "not_found"
		} else if !item.Supported {
			item.Status = "unsupported"
		} else if member.WalletID != "" {
			wallet := state.Profiles[member.WalletID]
			item.WalletID, item.Provider, item.SourceName = wallet.ID, wallet.Provider, wallet.Name
			item.SharedCount = len(walletMembers(state, wallet.ID))
			if wallet.Provider == "new" {
				item.SourceID = wallet.SourceID
			} else {
				item.Automatic = true
			}
			value := m.Reader.Read(ctx, a, false)
			item.Status, item.Error, item.Stale, item.Querying, item.AutoRefresh, item.NextCheckAt = value.Status, value.Error, value.Stale, value.Querying, value.AutoRefresh, value.NextCheckAt
			if value.CheckedAt != nil && value.Remaining != "" {
				item.LastCheckedAt = value.CheckedAt
				item.Balance = &struct {
					AccountID int64     `json:"account_id"`
					Remaining string    `json:"remaining"`
					Used      string    `json:"used"`
					Unit      string    `json:"unit"`
					CheckedAt time.Time `json:"checked_at"`
				}{Remaining: value.Remaining, Unit: value.Unit, CheckedAt: *value.CheckedAt}
				if wallet.Provider == "new" {
					item.Balance.AccountID = state.Sources[wallet.SourceID].AccountID
				}
			}
		}
		rows = append(rows, item)
	}
	return rows, nil
}

// Portal's authenticated binding endpoint delegates to this shared registry so
// Sub and New memberships cannot race in two independent stores.
func (m *WalletManager) BindSource(ctx context.Context, accountID int64, sourceID string, sourceRevision, bindingRevision, adminID int64) error {
	accounts, err := m.accounts(ctx, []int64{accountID})
	if err != nil {
		return err
	}
	a := accounts[accountID]
	if !walletAccountSupported(a) {
		return ErrWalletUnsupported
	}
	return m.Repo.Change(ctx, func(state *WalletRegistry) error {
		if !state.PortalImported || time.Since(state.PortalSeenAt) > 2*time.Minute {
			return ErrWalletBridge
		}
		member := state.Members[accountID]
		if member.Revision != bindingRevision {
			return ErrWalletRegistryConflict
		}
		if member.WalletID != "" && state.Profiles[member.WalletID].Provider != "new" {
			return ErrWalletMemberLinked
		}
		if sourceID == "" {
			state.Members[accountID] = WalletMember{Revision: member.Revision + 1}
			bumpWalletMembership(state, member.WalletID)
			return nil
		}
		source, ok := state.Sources[sourceID]
		if !ok || source.Revision != sourceRevision {
			return ErrWalletRegistryConflict
		}
		if source.DeletionRevision > 0 {
			return ErrWalletSourceDeleting
		}
		if source.LastBalance == nil || source.LastBalance.AccountID != source.AccountID || source.LastBalance.Unit == "" || (source.Status != "ok" && source.Status != "low") {
			return ErrWalletNotVerified
		}
		id := "new:" + sourceID
		if _, ok := state.Profiles[id]; !ok {
			state.Profiles[id] = ManagedWallet{ID: id, Revision: 1, Name: source.Name, Provider: "new", SourceID: sourceID, Alert: WalletAlertRule{Threshold: source.Threshold, Unit: source.ThresholdUnit, UseDefaultRecipients: true, Recipients: []string{}}, UpdatedBy: adminID, UpdatedAt: time.Now().UTC()}
		}
		state.Members[accountID] = WalletMember{WalletID: id, Identity: AccountWalletIdentity(a), Revision: member.Revision + 1}
		bumpWalletMembership(state, id)
		if member.WalletID != id {
			bumpWalletMembership(state, member.WalletID)
		}
		return nil
	})
}

func ParseWalletAccountIDs(raw string) ([]int64, error) {
	parts := strings.Split(raw, ",")
	if len(parts) == 0 || len(parts) > 100 {
		return nil, ErrWalletSetup
	}
	ids := make([]int64, 0, len(parts))
	seen := map[int64]bool{}
	for _, part := range parts {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != part || seen[id] {
			return nil, ErrWalletSetup
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

func bumpWalletMembership(state *WalletRegistry, id string) {
	if wallet, ok := state.Profiles[id]; ok {
		wallet.Revision++
		wallet.UpdatedAt = time.Now().UTC()
		if len(walletMembers(*state, id)) == 0 {
			wallet.Alert.Enabled = false
		}
		for key, d := range state.Deliveries {
			if d.WalletID == id && (d.Status == "pending" || d.Status == "retry") {
				d.Status = "cancelled"
				d.UpdatedAt = time.Now().UTC()
				state.Deliveries[key] = d
				if wallet.Alarm.Low {
					wallet.Alarm.LastQueued = time.Time{}
				}
			}
		}
		state.Profiles[id] = wallet
	}
}
