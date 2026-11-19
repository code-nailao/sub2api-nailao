package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/google/uuid"
)

var ErrWalletRegistryConflict = infraerrors.Conflict("wallet_settings_changed", "Wallet settings changed; reload before saving")
var ErrWalletMemberLinked = infraerrors.Conflict("wallet_member_linked", "A selected channel belongs to another wallet")
var ErrWalletRebind = infraerrors.Conflict("wallet_rebind_required", "Channel identity changed; confirm wallet membership again")
var ErrWalletSetup = infraerrors.BadRequest("wallet_settings_invalid", "Check the wallet, verified query channel, threshold and recipients")
var ErrWalletBridge = infraerrors.New(503, "wallet_bridge_unavailable", "Wallet source synchronization is not ready")

type WalletManager struct {
	Repo           WalletRegistryRepository
	Reader         *AccountWalletReader
	Accounts       AccountRepository
	Settings       SettingRepository
	Mail           WalletEmailSender
	IdentitySecret string
}

func WalletBridgeToken(secret string) string {
	if len(secret) < 32 {
		return ""
	}
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write([]byte("sources-wallet-snapshot-bridge/v1"))
	return hex.EncodeToString(h.Sum(nil))
}

// Kept byte-compatible with the previously deployed Portal binding marker.
func SourcesBalanceMarker(a *Account, secret string) string {
	endpoints := map[string]string{}
	if values, ok := a.Credentials["api_base_urls"].(map[string]any); ok {
		for protocol, v := range values {
			if endpoint, ok := v.(string); ok {
				endpoints[protocol] = strings.TrimSpace(endpoint)
			}
		}
	}
	material, _ := json.Marshal([]any{"sources-balance-identity/v1", a.ID, a.CreatedAt.UTC().UnixNano(), a.Platform, a.Type, strings.TrimSpace(a.GetCredential("base_url")), a.GetCredential("api_key"), strings.TrimSpace(a.GetCredential("account_mode")), a.GetAPIProtocol(), endpoints})
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write(material)
	return "v1:" + hex.EncodeToString(h.Sum(nil))
}

func (m *WalletManager) DefaultRecipients(ctx context.Context) []string {
	if m.Settings == nil {
		return []string{}
	}
	raw, err := m.Settings.GetValue(ctx, SettingKeyAccountQuotaNotifyEmails)
	if err != nil {
		return []string{}
	}
	return filterVerifiedEmails(ParseNotifyEmails(raw))
}

func (m *WalletManager) accounts(ctx context.Context, ids []int64) (map[int64]*Account, error) {
	rows, err := m.Accounts.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := map[int64]*Account{}
	for _, a := range rows {
		if a != nil {
			result[a.ID] = a
		}
	}
	return result, nil
}

func walletMembers(state WalletRegistry, id string) []int64 {
	if id == "" {
		return []int64{}
	}
	ids := []int64{}
	for accountID, member := range state.Members {
		if member.WalletID == id {
			ids = append(ids, accountID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (m *WalletManager) Resolve(ctx context.Context, a *Account) (*Account, *ManagedWallet, error) {
	state, err := m.Repo.Read(ctx)
	if err != nil {
		return nil, nil, err
	}
	member := state.Members[a.ID]
	if member.WalletID == "" {
		return a, nil, nil
	}
	wallet, ok := state.Profiles[member.WalletID]
	if !ok || member.Identity != AccountWalletIdentity(a) {
		return nil, nil, ErrWalletRebind
	}
	if wallet.Provider == "new" {
		return nil, &wallet, nil
	}
	rows, err := m.accounts(ctx, []int64{wallet.RepresentativeID})
	if err != nil {
		return nil, nil, err
	}
	rep := rows[wallet.RepresentativeID]
	if rep == nil || !walletAccountSupported(rep) || state.Members[rep.ID].WalletID != wallet.ID || state.Members[rep.ID].Identity != AccountWalletIdentity(rep) {
		return nil, nil, ErrWalletRebind
	}
	return rep, &wallet, nil
}

func (m *WalletManager) Scope(ctx context.Context, a *Account) (string, error) {
	target, w, err := m.Resolve(ctx, a)
	if err != nil {
		return "", err
	}
	if w == nil {
		return AccountWalletIdentity(a), nil
	}
	scope := w.ID + ":" + strconv.FormatInt(w.Revision, 10) + ":" + AccountWalletIdentity(a)
	if target != nil {
		scope += ":" + AccountWalletIdentity(target)
	}
	return scope, nil
}

func (m *WalletManager) IsQueryChannel(ctx context.Context, a *Account) bool {
	target, w, err := m.Resolve(ctx, a)
	return err == nil && (w == nil || w.Provider == "sub" && target != nil && target.ID == a.ID)
}

func (m *WalletManager) ExternalBalance(ctx context.Context, wallet ManagedWallet) AccountWallet {
	state, err := m.Repo.Read(ctx)
	if err != nil {
		return walletFailure("wallet_snapshot_unavailable")
	}
	source, ok := state.Sources[wallet.SourceID]
	if !ok {
		return walletFailure("source_not_found")
	}
	value := AccountWallet{Status: source.Status, Error: source.Error, LastAttemptAt: &source.LastCheckedAt, NextCheckAt: &source.NextCheckAt, QueryURL: source.Origin}
	if source.Status == "ok" || source.Status == "low" {
		value.Status = "ready"
	}
	if b := source.LastBalance; b != nil {
		value.Remaining, value.Unit, value.CheckedAt = b.Remaining, b.Unit, &b.CheckedAt
		value.AutoRefresh = true
		value.Stale = value.Status != "ready" || time.Since(b.CheckedAt) > 12*time.Minute
	}
	if time.Since(state.PortalSeenAt) > 2*time.Minute {
		value.Status = "error"
		value.Error = "wallet_bridge_unavailable"
		value.Stale = true
	}
	return value
}

func (m *WalletManager) decorate(ctx context.Context, value AccountWallet, wallet *ManagedWallet) AccountWallet {
	if wallet == nil {
		return value
	}
	if wallet.Alert.Enabled {
		if comparison, valid := walletAmountComparison(value, wallet.Alert, time.Now()); valid && comparison <= 0 {
			value.Status = "low"
		}
	}
	value.WalletID, value.WalletName, value.Provider, value.RepresentativeID = wallet.ID, wallet.Name, wallet.Provider, wallet.RepresentativeID
	if wallet.Provider == "new" {
		value.SourceID = wallet.SourceID
	}
	if state, err := m.Repo.Read(ctx); err == nil {
		value.SharedCount = len(walletMembers(state, wallet.ID))
	}
	return value
}

func (m *WalletManager) View(ctx context.Context, accountID int64) (WalletSettingsView, error) {
	state, err := m.Repo.Read(ctx)
	if err != nil {
		return WalletSettingsView{}, err
	}
	rows, err := m.accounts(ctx, []int64{accountID})
	if err != nil {
		return WalletSettingsView{}, err
	}
	a := rows[accountID]
	if a == nil || !walletAccountSupported(a) {
		return WalletSettingsView{}, ErrWalletUnsupported
	}
	w := state.Profiles[state.Members[accountID].WalletID]
	if w.ID == "" {
		w = ManagedWallet{Name: a.Name, Provider: "sub", RepresentativeID: a.ID, Alert: WalletAlertRule{UseDefaultRecipients: true, Recipients: []string{}}}
	}
	view := WalletSettingsView{Wallet: w, Members: []WalletCandidate{}, Candidates: []WalletCandidate{}, Wallets: []ManagedWallet{}, DefaultRecipients: m.DefaultRecipients(ctx), Deliveries: []WalletDelivery{}, BridgeReady: state.PortalImported}
	view.Balance = m.Reader.Read(ctx, a, false)
	memberAccounts, err := m.accounts(ctx, walletMembers(state, w.ID))
	if err != nil {
		return view, err
	}
	for _, p := range state.Profiles {
		if len(walletMembers(state, p.ID)) == 0 {
			continue
		}
		view.Wallets = append(view.Wallets, p)
	}
	sort.Slice(view.Wallets, func(i, j int) bool { return view.Wallets[i].Name < view.Wallets[j].Name })
	// The native repository bounds each page to 1000. Search stays local to the
	// displayed candidates; no credential is returned in the management DTO.
	for page := 1; page <= 10; page++ {
		candidates, result, err := m.Accounts.List(ctx, pagination.PaginationParams{Page: page, PageSize: 1000, SortBy: "id", SortOrder: "asc"})
		if err != nil {
			return view, err
		}
		for i := range candidates {
			candidate := &candidates[i]
			if !walletAccountSupported(candidate) {
				continue
			}
			member := state.Members[candidate.ID]
			item := WalletCandidate{Platform: candidate.Platform, ID: candidate.ID, Name: candidate.Name, WalletID: member.WalletID}
			if member.WalletID == w.ID && w.ID != "" {
				hydrated := memberAccounts[candidate.ID]
				item.NeedsConfirmation = hydrated == nil || member.Identity != AccountWalletIdentity(hydrated)
			}
			if u, e := url.Parse(accountWalletCallURL(candidate)); e == nil {
				item.Site = u.Host
			}
			if candidate.ID == w.RepresentativeID {
				item.Verified = view.Balance.CheckedAt != nil && view.Balance.Status == "ready"
			}
			view.Candidates = append(view.Candidates, item)
			if member.WalletID == w.ID && w.ID != "" || w.ID == "" && candidate.ID == accountID {
				view.Members = append(view.Members, item)
			}
		}
		if result == nil || page >= result.Pages {
			break
		}
	}
	for _, delivery := range state.Deliveries {
		if delivery.WalletID == w.ID {
			delivery.HTML = ""
			view.Deliveries = append(view.Deliveries, delivery)
		}
	}
	sort.Slice(view.Deliveries, func(i, j int) bool { return view.Deliveries[i].CreatedAt.After(view.Deliveries[j].CreatedAt) })
	if len(view.Deliveries) > 20 {
		view.Deliveries = view.Deliveries[:20]
	}
	return view, nil
}

func normalizeWalletRule(rule WalletAlertRule) (WalletAlertRule, error) {
	rule.Threshold = strings.TrimSpace(rule.Threshold)
	rule.Unit = strings.TrimSpace(rule.Unit)
	if rule.RepeatHours != 0 && rule.RepeatHours != 6 && rule.RepeatHours != 12 && rule.RepeatHours != 24 {
		return rule, ErrWalletSetup
	}
	if rule.Enabled {
		n, ok := exactWalletAmount(json.Number(rule.Threshold))
		if !ok || strings.HasPrefix(n, "-") || len(rule.Unit) == 0 || len(rule.Unit) > 16 {
			return rule, ErrWalletSetup
		}
		rule.Threshold = n
	}
	if len(rule.Recipients) > 10 {
		return rule, ErrWalletSetup
	}
	clean := []string{}
	seen := map[string]bool{}
	for _, raw := range rule.Recipients {
		raw = strings.TrimSpace(raw)
		address, err := mail.ParseAddress(raw)
		if err != nil || address.Address != raw || len(raw) > 254 {
			return rule, ErrWalletSetup
		}
		key := strings.ToLower(raw)
		if !seen[key] {
			seen[key] = true
			clean = append(clean, raw)
		}
	}
	rule.Recipients = clean
	return rule, nil
}

func (m *WalletManager) Save(ctx context.Context, accountID, adminID int64, in WalletSettingsInput) (ManagedWallet, error) {
	var zero ManagedWallet
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 160 || len(in.Members) == 0 || len(in.Members) > 1000 || (in.Provider != "sub" && in.Provider != "new") {
		return zero, ErrWalletSetup
	}
	rule, err := normalizeWalletRule(in.Alert)
	if err != nil {
		return zero, err
	}
	in.Alert = rule
	seen := map[int64]bool{}
	for _, id := range in.Members {
		if id <= 0 || seen[id] {
			return zero, ErrWalletSetup
		}
		seen[id] = true
	}
	if !seen[accountID] {
		return zero, ErrWalletSetup
	}
	accounts, err := m.accounts(ctx, in.Members)
	if err != nil {
		return zero, err
	}
	if len(accounts) != len(in.Members) {
		return zero, ErrWalletSetup
	}
	for _, a := range accounts {
		if !walletAccountSupported(a) {
			return zero, ErrWalletUnsupported
		}
	}
	if in.Provider == "sub" && !seen[in.RepresentativeID] {
		return zero, ErrWalletSetup
	}
	if in.Alert.Enabled && len(in.Alert.Recipients) == 0 && (!in.Alert.UseDefaultRecipients || len(m.DefaultRecipients(ctx)) == 0) {
		return zero, infraerrors.BadRequest("wallet_recipients_required", "Choose a notification recipient")
	}
	var verified AccountWallet
	if in.Provider == "sub" {
		verified = m.Reader.readRaw(ctx, accounts[in.RepresentativeID], false)
	}
	newProfile := in.ID == ""
	if in.Provider == "new" && in.ID != "" && in.ID != "new:"+in.SourceID {
		return zero, ErrWalletSetup
	}
	if in.ID == "" {
		if in.Provider == "new" {
			in.ID = "new:" + in.SourceID
		} else {
			in.ID = "sub:" + uuid.NewString()
		}
	}
	err = m.Repo.Change(ctx, func(state *WalletRegistry) error {
		if !state.PortalImported {
			return ErrWalletBridge
		}
		old, found := state.Profiles[in.ID]
		if found && old.Revision != in.Revision || !found && (in.Revision != 0 || !newProfile) {
			return ErrWalletRegistryConflict
		}
		if found && (old.Provider != in.Provider || old.SourceID != in.SourceID) {
			return ErrWalletSetup
		}

		topologyChanged := !found || old.RepresentativeID != in.RepresentativeID
		identityChanged := false
		for id, member := range state.Members {
			if member.WalletID == in.ID && !seen[id] {
				topologyChanged = true
			}
		}
		for id, a := range accounts {
			member := state.Members[id]
			if member.WalletID != "" && member.WalletID != in.ID {
				return ErrWalletMemberLinked
			}
			if member.WalletID != in.ID {
				topologyChanged = true
			}
			if member.WalletID == in.ID && member.Identity != AccountWalletIdentity(a) {
				identityChanged = true
			}
		}
		rebindRequested := identityChanged && in.ConfirmShared
		updateMembership := topologyChanged || rebindRequested
		if identityChanged && !rebindRequested && in.Alert.Enabled {
			return ErrWalletRebind
		}
		if updateMembership && len(in.Members) > 1 && !in.ConfirmShared {
			return infraerrors.BadRequest("wallet_shared_confirmation_required", "Confirm that these channels share one wallet")
		}
		needsFresh := !found || updateMembership || (in.Alert.Enabled &&
			(!old.Alert.Enabled || old.Alert.Threshold != in.Alert.Threshold || old.Alert.Unit != in.Alert.Unit))
		if in.Provider == "new" {
			source, ok := state.Sources[in.SourceID]
			if ok && source.DeletionRevision > 0 {
				return ErrWalletSourceDeleting
			}
			if ok && source.LastBalance != nil {
				status := "ready"
				if source.Status != "ok" && source.Status != "low" {
					status = "error"
				}
				verified = AccountWallet{Status: status, Remaining: source.LastBalance.Remaining, Unit: source.LastBalance.Unit, CheckedAt: &source.LastBalance.CheckedAt}
			}
			if needsFresh && time.Since(state.PortalSeenAt) > 2*time.Minute {
				return ErrWalletBridge
			}
		}
		if needsFresh {
			if verified.Status != "ready" || verified.CheckedAt == nil || verified.Stale || time.Since(*verified.CheckedAt) > 12*time.Minute {
				return ErrWalletNotVerified
			}
			if in.Alert.Enabled && in.Alert.Unit != verified.Unit {
				return infraerrors.BadRequest("wallet_unit_changed", "Alert unit must match the verified wallet")
			}
		}
		if !found && len(state.Profiles) >= 1000 {
			return ErrWalletSetup
		}
		if updateMembership {
			for id, member := range state.Members {
				if member.WalletID == in.ID && !seen[id] {
					state.Members[id] = WalletMember{Revision: member.Revision + 1}
				}
			}
			for id, a := range accounts {
				member := state.Members[id]
				state.Members[id] = WalletMember{WalletID: in.ID, Identity: AccountWalletIdentity(a), Revision: member.Revision + 1}
			}
		}
		zero = ManagedWallet{ID: in.ID, Revision: old.Revision + 1, Name: in.Name, Provider: in.Provider, RepresentativeID: in.RepresentativeID, SourceID: in.SourceID, Alert: in.Alert, UpdatedBy: adminID, UpdatedAt: time.Now().UTC()}
		oldRule, _ := json.Marshal(old.Alert)
		newRule, _ := json.Marshal(in.Alert)
		if found && old.RepresentativeID == in.RepresentativeID && string(oldRule) == string(newRule) {
			zero.Alarm = old.Alarm
		}
		// A configuration revision must wait for a subsequent successful sample;
		// neither cached low amounts nor stale delivery jobs can trigger mail.
		if verified.CheckedAt != nil {
			zero.Alarm.LastSuccess = *verified.CheckedAt
		}
		for id, d := range state.Deliveries {
			if d.WalletID == in.ID && (d.Status == "pending" || d.Status == "retry") {
				d.Status = "cancelled"
				if zero.Alarm.Low {
					zero.Alarm.LastQueued = time.Time{}
				}
				d.UpdatedAt = time.Now().UTC()
				state.Deliveries[id] = d
			}
		}
		state.Profiles[in.ID] = zero
		return nil
	})
	return zero, err
}

func (m *WalletManager) Unlink(ctx context.Context, accountID int64, walletID string, revision int64) error {
	if walletID == "" || revision <= 0 {
		return ErrWalletSetup
	}
	err := m.Repo.Change(ctx, func(state *WalletRegistry) error {
		member := state.Members[accountID]
		wallet := state.Profiles[member.WalletID]
		if member.WalletID != walletID || wallet.Revision != revision {
			return ErrWalletRegistryConflict
		}
		if wallet.Provider == "sub" && wallet.RepresentativeID == accountID && len(walletMembers(*state, walletID)) > 1 {
			return infraerrors.BadRequest("wallet_representative_required", "Choose another query channel first")
		}
		state.Members[accountID] = WalletMember{Revision: member.Revision + 1}
		wallet.Revision++
		if len(walletMembers(*state, walletID)) == 0 {
			wallet.Alert.Enabled = false
		}
		for id, d := range state.Deliveries {
			if d.WalletID == walletID && (d.Status == "pending" || d.Status == "retry") {
				d.Status = "cancelled"
				state.Deliveries[id] = d
				if wallet.Alarm.Low {
					wallet.Alarm.LastQueued = time.Time{}
				}
			}
		}
		wallet.UpdatedAt = time.Now().UTC()
		state.Profiles[walletID] = wallet
		return nil
	})
	return err
}

func (m *WalletManager) SyncPortal(ctx context.Context, in PortalWalletSync) error {
	if len(in.Sources) > 1000 || len(in.Bindings) > 10000 || time.Since(in.GeneratedAt) > 2*time.Minute || time.Until(in.GeneratedAt) > time.Minute {
		return ErrWalletSetup
	}
	ids := []int64{}
	for _, b := range in.Bindings {
		if b.AccountID > 0 && b.SourceID != "" {
			ids = append(ids, b.AccountID)
		}
	}
	accounts, err := m.accounts(ctx, ids)
	if err != nil {
		return err
	}
	return m.Repo.Change(ctx, func(state *WalletRegistry) error {
		if !in.GeneratedAt.After(state.PortalSeenAt) {
			return nil
		}
		sources := map[string]PortalWalletSource{}
		for _, source := range in.Sources {
			if source.ID == "" || len(source.ID) > 80 || source.Revision <= 0 || len(source.Name) > 160 || len(source.Error) > 128 || sources[source.ID].ID != "" {
				return ErrWalletSetup
			}
			if b := source.LastBalance; b != nil {
				if _, ok := exactWalletAmount(json.Number(b.Remaining)); !ok || b.AccountID <= 0 || b.AccountID != source.AccountID || b.Unit == "" || len(b.Unit) > 16 || b.CheckedAt.After(time.Now().Add(time.Minute)) {
					return ErrWalletSetup
				}
			}
			sources[source.ID] = mergePortalWalletSource(state.Sources[source.ID], source)
		}
		state.Sources = sources
		state.PortalSeenAt = in.GeneratedAt
		if !state.PortalImported {
			for _, b := range in.Bindings {
				a, source := accounts[b.AccountID], sources[b.SourceID]
				if a == nil || source.ID == "" || !walletAccountSupported(a) {
					continue
				}
				id := "new:" + source.ID
				if _, ok := state.Profiles[id]; !ok {
					state.Profiles[id] = ManagedWallet{ID: id, Revision: 1, Name: source.Name, Provider: "new", SourceID: source.ID, Alert: WalletAlertRule{Threshold: source.Threshold, Unit: source.ThresholdUnit, UseDefaultRecipients: true, Recipients: []string{}}, UpdatedAt: time.Now().UTC()}
				}
				identity := "rebind_required"
				if hmac.Equal([]byte(b.Marker), []byte(SourcesBalanceMarker(a, m.IdentitySecret))) {
					identity = AccountWalletIdentity(a)
				}
				if state.Members[a.ID].WalletID == "" {
					state.Members[a.ID] = WalletMember{WalletID: id, Identity: identity, Revision: b.Revision}
				}
			}
			state.PortalImported = true
		}
		return nil
	})
}

func walletRegistryError(err error) bool {
	return errors.Is(err, ErrWalletRebind) || errors.Is(err, ErrWalletRegistryConflict)
}
