package service

import (
	"context"
	"time"
)

// This registry contains display metadata and alert state, never an upstream key.
// One transactional row serializes explicit membership edits and delivery state.
type WalletRegistry struct {
	Version        int                           `json:"version"`
	Profiles       map[string]ManagedWallet      `json:"profiles"`
	Members        map[int64]WalletMember        `json:"members"`
	Sources        map[string]PortalWalletSource `json:"sources"`
	Deliveries     map[string]WalletDelivery     `json:"deliveries"`
	PortalSeenAt   time.Time                     `json:"portal_seen_at"`
	PortalImported bool                          `json:"portal_imported"`
}

func (v *WalletRegistry) Initialize() {
	if v.Version == 0 {
		v.Version = 1
	}
	if v.Profiles == nil {
		v.Profiles = map[string]ManagedWallet{}
	}
	if v.Members == nil {
		v.Members = map[int64]WalletMember{}
	}
	if v.Sources == nil {
		v.Sources = map[string]PortalWalletSource{}
	}
	if v.Deliveries == nil {
		v.Deliveries = map[string]WalletDelivery{}
	}
}

type WalletMember struct {
	WalletID string `json:"wallet_id"`
	Identity string `json:"identity"`
	Revision int64  `json:"revision"`
}

type WalletAlertRule struct {
	Enabled              bool     `json:"enabled"`
	Threshold            string   `json:"threshold"`
	Unit                 string   `json:"unit"`
	UseDefaultRecipients bool     `json:"use_default_recipients"`
	Recipients           []string `json:"recipients"`
	RepeatHours          int      `json:"repeat_hours"`
	NotifyRecovery       bool     `json:"notify_recovery"`
}

type WalletAlarm struct {
	Low           bool      `json:"low"`
	Episode       string    `json:"episode"`
	RecoveryCount int       `json:"recovery_count"`
	LastAttempt   time.Time `json:"last_attempt"`
	LastSuccess   time.Time `json:"last_success"`
	LastQueued    time.Time `json:"last_queued"`
}

type ManagedWallet struct {
	ID               string          `json:"id"`
	Revision         int64           `json:"revision"`
	Name             string          `json:"name"`
	Provider         string          `json:"provider"`
	RepresentativeID int64           `json:"representative_id"`
	SourceID         string          `json:"source_id,omitempty"`
	Alert            WalletAlertRule `json:"alert"`
	Alarm            WalletAlarm     `json:"alarm"`
	UpdatedBy        int64           `json:"updated_by"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type PortalWalletSource struct {
	// Main owns this reservation. Portal snapshots cannot create or remove it
	// unless a strictly newer source revision makes the old local delete fail CAS.
	DeletionRevision int64  `json:"deletion_revision,omitempty"`
	Threshold        string `json:"threshold"`
	ThresholdUnit    string `json:"threshold_unit"`
	ID               string `json:"id"`
	Name             string `json:"name"`
	Origin           string `json:"origin"`
	AccountID        int64  `json:"account_id"`
	Revision         int64  `json:"revision"`
	Status           string `json:"status"`
	Error            string `json:"error,omitempty"`
	LastBalance      *struct {
		AccountID int64     `json:"account_id"`
		Remaining string    `json:"remaining"`
		Unit      string    `json:"unit"`
		CheckedAt time.Time `json:"checked_at"`
	} `json:"last_balance,omitempty"`
	LastCheckedAt time.Time `json:"last_checked_at"`
	NextCheckAt   time.Time `json:"next_check_at"`
}

type PortalWalletSync struct {
	GeneratedAt time.Time            `json:"generated_at"`
	Sources     []PortalWalletSource `json:"sources"`
	Bindings    []struct {
		AccountID int64  `json:"account_id"`
		SourceID  string `json:"source_id"`
		Marker    string `json:"identity_marker"`
		Revision  int64  `json:"revision"`
	} `json:"bindings"`
}

type WalletDelivery struct {
	Amount         string     `json:"amount,omitempty"`
	Unit           string     `json:"unit,omitempty"`
	CheckedAt      *time.Time `json:"checked_at,omitempty"`
	ID             string     `json:"id"`
	WalletID       string     `json:"wallet_id"`
	WalletRevision int64      `json:"wallet_revision"`
	Episode        string     `json:"episode"`
	Kind           string     `json:"kind"`
	Recipient      string     `json:"recipient"`
	Subject        string     `json:"subject"`
	HTML           string     `json:"html,omitempty"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	NextAttempt    time.Time  `json:"next_attempt"`
	Error          string     `json:"error,omitempty"`
}

type WalletRegistryRepository interface {
	Read(context.Context) (WalletRegistry, error)
	Change(context.Context, func(*WalletRegistry) error) error
}

type WalletEmailSender interface {
	SendEmail(context.Context, string, string, string) error
}

type WalletCandidate struct {
	Platform          string `json:"platform"`
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	Site              string `json:"site"`
	WalletID          string `json:"wallet_id,omitempty"`
	Verified          bool   `json:"verified"`
	NeedsConfirmation bool   `json:"needs_confirmation"`
}

type WalletSettingsInput struct {
	ID               string          `json:"id"`
	Revision         int64           `json:"revision"`
	Name             string          `json:"name"`
	Provider         string          `json:"provider"`
	RepresentativeID int64           `json:"representative_id"`
	SourceID         string          `json:"source_id"`
	Members          []int64         `json:"members"`
	ConfirmShared    bool            `json:"confirm_shared"`
	Alert            WalletAlertRule `json:"alert"`
}

type WalletSettingsView struct {
	Wallet            ManagedWallet     `json:"wallet"`
	Balance           AccountWallet     `json:"balance"`
	Members           []WalletCandidate `json:"members"`
	Candidates        []WalletCandidate `json:"candidates"`
	Wallets           []ManagedWallet   `json:"wallets"`
	DefaultRecipients []string          `json:"default_recipients"`
	Deliveries        []WalletDelivery  `json:"deliveries"`
	BridgeReady       bool              `json:"bridge_ready"`
}
