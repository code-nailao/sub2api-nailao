//go:build unit

package service

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

type walletNativeSMTP struct{ config *SMTPConfig }

func (s walletNativeSMTP) SendEmail(_ context.Context, to, subject, body string) error {
	return (&EmailService{}).SendEmailWithConfig(s.config, to, subject, body)
}
func TestWalletAlertUsesNativeSMTPSenderAndEscapesContent(t *testing.T) {
	m, repo, snap, _ := managedWalletFixture(t)
	server, port := startFakeSMTPServer(t, true, false)
	m.Mail = walletNativeSMTP{config: smtpTestConfig(port, true)}
	wallet, err := m.Save(context.Background(), 1, 9, WalletSettingsInput{Name: "Supplier <script>alert(1)</script>", Provider: "sub", RepresentativeID: 1, Members: []int64{1}, Alert: WalletAlertRule{Enabled: true, Threshold: "100", Unit: "USD", Recipients: []string{"owner@example.invalid"}}})
	require.NoError(t, err)
	preview := BuildWalletMail(wallet, "50", []string{"<script>channel</script>"}, time.Now(), false)
	require.NotContains(t, preview.HTML, "<script>")
	require.Contains(t, preview.HTML, "&lt;script&gt;")
	at := time.Now().UTC().Add(time.Millisecond)
	snap.row.Value.CheckedAt = &at
	snap.row.Value.LastAttemptAt = &at
	m.Tick(context.Background())
	require.True(t, server.sawCommand("DATA"))
	state, _ := repo.Read(context.Background())
	require.Len(t, state.Deliveries, 1)
	for _, d := range state.Deliveries {
		require.Equal(t, "sent", d.Status)
	}
}
