package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type walletObservation struct {
	Value AccountWallet
	Names []string
	Valid bool
}

type WalletMailPreview struct {
	Subject string `json:"subject"`
	HTML    string `json:"html"`
}

func BuildWalletMail(wallet ManagedWallet, amount string, names []string, checked time.Time, recovery bool) WalletMailPreview {
	state := "余额不足提醒"
	description := "上游共享账户的余额已达到提醒门槛，请及时检查余额。"
	if recovery {
		state = "余额恢复通知"
		description = "上游共享账户的余额已连续两次高于提醒门槛。"
	}
	subject := sanitizeEmailHeader("[Sources] " + state + " · " + wallet.Name)
	body := fmt.Sprintf(`<div style="font-family:Arial,'PingFang SC',sans-serif;color:#202b3b;max-width:600px;margin:auto;padding:28px"><h2 style="font-size:20px">%s</h2><p>%s</p><h3>%s</h3><p style="font-size:28px;font-weight:600">%s <span style="font-size:14px">%s</span></p><p>提醒门槛：≤ %s %s</p><p>影响渠道（%d）：%s</p><p style="color:#667085;font-size:12px">余额查询时间：%s</p><p style="color:#667085;font-size:12px">同一个钱包合并发送。可在主站账号管理的“余额与提醒”中调整规则。</p></div>`, html.EscapeString(state), html.EscapeString(description), html.EscapeString(wallet.Name), html.EscapeString(amount), html.EscapeString(wallet.Alert.Unit), html.EscapeString(wallet.Alert.Threshold), html.EscapeString(wallet.Alert.Unit), len(names), html.EscapeString(strings.Join(names, "、")), checked.UTC().Format("2006-01-02 15:04:05 UTC"))
	return WalletMailPreview{Subject: subject, HTML: body}
}

func walletAmountComparison(value AccountWallet, rule WalletAlertRule, now time.Time) (int, bool) {
	if value.Status != "ready" || value.Stale || value.CheckedAt == nil || value.Unit != rule.Unit || now.Sub(*value.CheckedAt) > 12*time.Minute || value.CheckedAt.After(now.Add(time.Minute)) {
		return 0, false
	}
	amount, ok := new(big.Rat).SetString(value.Remaining)
	if !ok {
		return 0, false
	}
	threshold, ok := new(big.Rat).SetString(rule.Threshold)
	if !ok || threshold.Sign() < 0 {
		return 0, false
	}
	return amount.Cmp(threshold), true
}

func (m *WalletManager) observation(ctx context.Context, state WalletRegistry, wallet ManagedWallet) walletObservation {
	ids := walletMembers(state, wallet.ID)
	accounts, err := m.accounts(ctx, ids)
	if err != nil {
		return walletObservation{}
	}
	observation := walletObservation{Names: []string{}}
	for _, id := range ids {
		a := accounts[id]
		if a != nil && walletAccountSupported(a) && state.Members[id].Identity == AccountWalletIdentity(a) {
			observation.Names = append(observation.Names, a.Name)
		}
	}
	if len(observation.Names) == 0 {
		return observation
	}
	if wallet.Provider == "sub" {
		a := accounts[wallet.RepresentativeID]
		if a == nil || state.Members[a.ID].Identity != AccountWalletIdentity(a) {
			return observation
		}
		observation.Value = m.Reader.readRaw(ctx, a, false)
	} else {
		observation.Value = m.ExternalBalance(ctx, wallet)
	}
	observation.Valid = true
	return observation
}

func walletRecipients(rule WalletAlertRule, defaults []string) []string {
	all := append([]string{}, rule.Recipients...)
	if rule.UseDefaultRecipients {
		all = append(all, defaults...)
	}
	seen := map[string]bool{}
	result := []string{}
	for _, email := range all {
		key := strings.ToLower(email)
		if !seen[key] {
			seen[key] = true
			result = append(result, email)
		}
	}
	return result
}

func (m *WalletManager) Tick(ctx context.Context) {
	read, cancel := context.WithTimeout(ctx, 5*time.Second)
	state, err := m.Repo.Read(read)
	cancel()
	if err != nil {
		return
	}
	defaults := m.DefaultRecipients(ctx)
	for _, wallet := range state.Profiles {
		if ctx.Err() != nil {
			return
		}
		if !wallet.Alert.Enabled {
			continue
		}
		call, done := context.WithTimeout(ctx, 5*time.Second)
		observation := m.observation(call, state, wallet)
		if observation.Valid {
			_ = m.observe(call, wallet, observation, walletRecipients(wallet.Alert, defaults), time.Now().UTC())
		}
		done()
	}
	for i := 0; i < 5 && ctx.Err() == nil; i++ {
		if !m.deliverOne(ctx) {
			break
		}
	}
}

func (m *WalletManager) observe(ctx context.Context, expected ManagedWallet, observation walletObservation, recipients []string, now time.Time) error {
	return m.Repo.Change(ctx, func(state *WalletRegistry) error {
		wallet, ok := state.Profiles[expected.ID]
		if !ok || wallet.Revision != expected.Revision || !wallet.Alert.Enabled {
			return nil
		}
		value := observation.Value
		cmp, valid := walletAmountComparison(value, wallet.Alert, now)
		if !valid {
			if value.LastAttemptAt != nil && value.LastAttemptAt.After(wallet.Alarm.LastAttempt) {
				wallet.Alarm.RecoveryCount = 0
				wallet.Alarm.LastAttempt = *value.LastAttemptAt
				state.Profiles[wallet.ID] = wallet
			}
			return nil
		}
		if !value.CheckedAt.After(wallet.Alarm.LastSuccess) {
			return nil
		}
		wallet.Alarm.LastSuccess = *value.CheckedAt
		if value.LastAttemptAt != nil {
			wallet.Alarm.LastAttempt = *value.LastAttemptAt
		}
		kind := ""
		if cmp <= 0 {
			wallet.Alarm.RecoveryCount = 0
			if !wallet.Alarm.Low {
				wallet.Alarm.Low = true
				wallet.Alarm.Episode = uuid.NewString()
				wallet.Alarm.LastQueued = time.Time{}
			}
			if wallet.Alarm.LastQueued.IsZero() || wallet.Alert.RepeatHours > 0 && now.Sub(wallet.Alarm.LastQueued) >= time.Duration(wallet.Alert.RepeatHours)*time.Hour {
				kind = "low"
			}
		} else if wallet.Alarm.Low {
			wallet.Alarm.RecoveryCount++
			if wallet.Alarm.RecoveryCount >= 2 {
				wallet.Alarm.Low = false
				wallet.Alarm.RecoveryCount = 0
				if wallet.Alert.NotifyRecovery {
					kind = "recovery"
				}
			}
		}
		if kind != "" && len(recipients) > 0 {
			// Keep finished history bounded; pending/uncertain entries are never
			// erased to make room for another send.
			for id, d := range state.Deliveries {
				if (d.Status == "sent" || d.Status == "cancelled") && now.Sub(d.UpdatedAt) > 30*24*time.Hour {
					delete(state.Deliveries, id)
				}
			}
			if len(state.Deliveries)+len(recipients) > 5000 {
				return fmt.Errorf("wallet_delivery_capacity")
			}
			mail := BuildWalletMail(wallet, value.Remaining, observation.Names, *value.CheckedAt, kind == "recovery")
			for _, recipient := range recipients {
				material := fmt.Sprintf("%s:%d:%s:%s:%s:%s", wallet.ID, wallet.Revision, wallet.Alarm.Episode, kind, value.CheckedAt.UTC().Format(time.RFC3339Nano), strings.ToLower(recipient))
				sum := sha256.Sum256([]byte(material))
				id := hex.EncodeToString(sum[:])
				if _, exists := state.Deliveries[id]; exists {
					continue
				}
				state.Deliveries[id] = WalletDelivery{ID: id, WalletID: wallet.ID, WalletRevision: wallet.Revision, Episode: wallet.Alarm.Episode, Kind: kind, Recipient: recipient, Subject: mail.Subject, Amount: value.Remaining, Unit: value.Unit, CheckedAt: value.CheckedAt, Status: "pending", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
			}
			wallet.Alarm.LastQueued = now
		}
		state.Profiles[wallet.ID] = wallet
		return nil
	})
}

func deliveryStillCurrent(wallet ManagedWallet, delivery WalletDelivery) bool {
	return wallet.Alert.Enabled && wallet.Revision == delivery.WalletRevision && wallet.Alarm.Episode == delivery.Episode && ((delivery.Kind == "low" && wallet.Alarm.Low) || (delivery.Kind == "recovery" && !wallet.Alarm.Low))
}

func (m *WalletManager) deliverOne(ctx context.Context) bool {
	if m.Mail == nil {
		return false
	}
	read, cancel := context.WithTimeout(ctx, 5*time.Second)
	state, err := m.Repo.Read(read)
	cancel()
	if err != nil {
		return false
	}
	now := time.Now().UTC()
	ids := []string{}
	for id, d := range state.Deliveries {
		if ((d.Status == "pending" || d.Status == "retry") && !d.NextAttempt.After(now)) || d.Status == "sending" && now.Sub(d.UpdatedAt) > 5*time.Minute {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return false
		}
		delivery := state.Deliveries[id]
		wallet := state.Profiles[delivery.WalletID]
		allowedRecipient := false
		for _, recipient := range walletRecipients(wallet.Alert, m.DefaultRecipients(ctx)) {
			if strings.EqualFold(recipient, delivery.Recipient) {
				allowedRecipient = true
			}
		}
		if !allowedRecipient {
			_ = m.Repo.Change(ctx, func(current *WalletRegistry) error {
				d := current.Deliveries[id]
				if d.Status == "pending" || d.Status == "retry" {
					d.Status = "cancelled"
					d.UpdatedAt = now
					current.Deliveries[id] = d
				}
				return nil
			})
			continue
		}
		if delivery.Status == "sending" {
			_ = m.Repo.Change(ctx, func(current *WalletRegistry) error {
				d := current.Deliveries[id]
				if d.Status == "sending" && now.Sub(d.UpdatedAt) > 5*time.Minute {
					d.Status = "uncertain"
					d.Error = "delivery_result_unknown"
					d.UpdatedAt = now
					current.Deliveries[id] = d
				}
				return nil
			})
			continue
		}
		observation := m.observation(ctx, state, wallet)
		cmp, valid := walletAmountComparison(observation.Value, wallet.Alert, now)
		if !observation.Valid || !valid || (delivery.Kind == "low" && cmp > 0) || (delivery.Kind == "recovery" && cmp <= 0) {
			continue
		}
		claimed := false
		err = m.Repo.Change(ctx, func(current *WalletRegistry) error {
			d := current.Deliveries[id]
			w := current.Profiles[d.WalletID]
			if d.Status != "pending" && d.Status != "retry" {
				return nil
			}
			if !deliveryStillCurrent(w, d) {
				d.Status = "cancelled"
				d.UpdatedAt = now
				current.Deliveries[id] = d
				return nil
			}
			mail := BuildWalletMail(w, observation.Value.Remaining, observation.Names, *observation.Value.CheckedAt, d.Kind == "recovery")
			d.Subject, d.HTML, d.Status, d.UpdatedAt = mail.Subject, mail.HTML, "sending", now
			d.Attempts++
			d.Amount, d.Unit, d.CheckedAt = observation.Value.Remaining, observation.Value.Unit, observation.Value.CheckedAt
			delivery = d
			d.HTML = ""
			current.Deliveries[id] = d
			claimed = true
			return nil
		})
		if err != nil || !claimed {
			continue
		}
		// A single background owner sends after the transactional claim. If the
		// process dies after SMTP acceptance, recovery marks this uncertain.
		call, done := context.WithTimeout(ctx, 45*time.Second)
		sendErr := m.Mail.SendEmail(call, delivery.Recipient, delivery.Subject, delivery.HTML)
		done()
		finish, stop := context.WithTimeout(context.Background(), 5*time.Second)
		_ = m.Repo.Change(finish, func(current *WalletRegistry) error {
			d := current.Deliveries[id]
			if d.Status != "sending" || d.Attempts != delivery.Attempts {
				return nil
			}
			d.UpdatedAt = time.Now().UTC()
			if sendErr == nil {
				d.Status = "sent"
				d.Error = ""
			} else {
				// Only failures before submitting message data are safe to retry.
				safe := false
				for _, prefix := range []string{"smtp dial:", "tls dial:", "starttls:", "new smtp client:", "smtp auth:", "smtp mail:", "smtp rcpt:", "smtp data:", "smtp server does not support STARTTLS", "smtp connection"} {
					if strings.HasPrefix(sendErr.Error(), prefix) {
						safe = true
					}
				}
				d.Status = "uncertain"
				d.Error = "delivery_result_unknown"
				if safe {
					d.Status = "failed"
					d.Error = "mail_delivery_failed"
					if d.Attempts < 3 {
						d.Status = "retry"
						d.NextAttempt = d.UpdatedAt.Add(time.Duration(d.Attempts*d.Attempts) * time.Minute)
					}
				}
			}
			current.Deliveries[id] = d
			return nil
		})
		stop()
		return true
	}
	return false
}
