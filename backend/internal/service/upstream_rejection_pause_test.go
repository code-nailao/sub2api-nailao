//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The operator's pause switch of the deadline release.

type rejectionPauseSettings struct {
	SettingRepository
	value string
	err   error
	reads int
	// pauseAfter switches the value to "true" once this many reads were served.
	pauseAfter int
}

func (s *rejectionPauseSettings) GetValue(_ context.Context, key string) (string, error) {
	if key != SettingKeyUpstreamRejectionDeadlineReleasePaused {
		return "", errors.New("unexpected setting key")
	}
	if s.pauseAfter > 0 && s.reads >= s.pauseAfter {
		s.value = "true"
	}
	s.reads++
	return s.value, s.err
}

func TestUpstreamRejectionDeadlineReleasePauseSwitch(t *testing.T) {
	run := func(settings *rejectionPauseSettings) (*dueReleaserStub, *UpstreamRejectionRecovery) {
		stub := &dueReleaserStub{rows: dueRows(3), finalize: applied}
		w := &UpstreamRejectionRecovery{dueRepo: stub}
		if settings != nil {
			w.settings = settings
		}
		w.releaseDueReservations(context.Background())
		return stub, w
	}

	// No settings source, a missing key and "false" behave exactly as before the switch.
	stub, _ := run(nil)
	require.Len(t, stub.finalized, 3)
	stub, w := run(&rejectionPauseSettings{err: ErrSettingNotFound})
	require.Len(t, stub.finalized, 3)
	require.False(t, w.releasePaused)
	for _, off := range []string{"false", " false\n"} {
		stub, _ = run(&rejectionPauseSettings{value: off})
		require.Len(t, stub.finalized, 3, "%q", off)
	}

	// Paused: nothing is listed, nothing is released, the holds stay.
	stub, w = run(&rejectionPauseSettings{value: "true"})
	require.Zero(t, stub.lists)
	require.Empty(t, stub.finalized)
	require.True(t, w.releasePaused)

	// Anything that is not a clear "not paused" keeps the holds, an empty value included.
	for _, unclear := range []string{"", "  ", "TRUE", "False", "1", "0", "yes", "paused"} {
		stub, w = run(&rejectionPauseSettings{value: unclear})
		require.Empty(t, stub.finalized, "%q", unclear)
		require.True(t, w.releasePaused, "%q", unclear)
	}

	// An unreadable switch skips the run without recording a pause.
	stub, w = run(&rejectionPauseSettings{err: errors.New("database unavailable")})
	require.Empty(t, stub.finalized)
	require.Zero(t, stub.lists)
	require.False(t, w.releasePaused)
}

func TestUpstreamRejectionDeadlineReleasePauseStopsARunUnderWay(t *testing.T) {
	// The switch is read before every release. The operator sets it while a
	// backlog is being drained: not one more hold is released.
	rows := dueRows(rejectionRecoveryPageSize + 8)
	settings := &rejectionPauseSettings{value: "false", pauseAfter: 5}
	stub := &dueReleaserStub{rows: rows, finalize: applied}
	w := &UpstreamRejectionRecovery{dueRepo: stub, settings: settings}
	w.releaseDueReservations(context.Background())
	require.Len(t, stub.finalized, 5, "five reads said run; the sixth said pause")
	require.Equal(t, 6, settings.reads)
	require.True(t, w.releasePaused)
	require.Nil(t, w.dueCursor, "a page stopped mid-way is resumed, not skipped")

	// Still paused: the next run does nothing at all.
	w.releaseDueReservations(context.Background())
	require.Len(t, stub.finalized, 5)
	require.Equal(t, 1, stub.lists)

	// Resumed: the released rows are no longer due, the rest is drained.
	settings.pauseAfter, settings.value = 0, "false"
	stub.rows = rows[5:]
	w.releaseDueReservations(context.Background())
	require.Len(t, stub.finalized, len(rows))
	require.False(t, w.releasePaused)
	seen := map[string]bool{}
	for _, id := range stub.finalized {
		require.False(t, seen[id], "no hold is released twice")
		seen[id] = true
	}

	// A switch that becomes unreadable mid-run stops the run as well.
	failing := &rejectionPauseSettings{value: "false"}
	stub = &dueReleaserStub{rows: dueRows(4)}
	stub.finalize = func(context.Context, *UpstreamRejectionFinalizeCommand) (*UpstreamRejectionBillingResult, error) {
		failing.err = errors.New("database unavailable")
		return &UpstreamRejectionBillingResult{Applied: true}, nil
	}
	w = &UpstreamRejectionRecovery{dueRepo: stub, settings: failing}
	w.releaseDueReservations(context.Background())
	require.Len(t, stub.finalized, 1)
	require.False(t, w.releasePaused)
}

func TestUpstreamRejectionDeadlineReleasePauseLeavesTheOtherRecoveryStepsRunning(t *testing.T) {
	// Paused deadline release; the unsent recovery, the usage projection and the
	// task reconciliation of the same run are untouched.
	var projected, pendingScans int
	stub := &dueReleaserStub{rows: dueRows(2), finalize: applied}
	w := &UpstreamRejectionRecovery{
		dueRepo:  stub,
		settings: &rejectionPauseSettings{value: "true"},
		repo: &rejectionRecoverySourceStub{page: func(context.Context, *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
			pendingScans++
			return &UpstreamRejectionPendingPage{}, nil
		}},
		projectionRepo: &rejectionProjectionSourceStub{page: func(context.Context, *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
			return &UpstreamRejectionPendingPage{Items: []UpstreamRejectionReservation{{State: UpstreamRejectionRejected}}}, nil
		}},
		project:       func(context.Context, *UpstreamRejectionReservation) error { projected++; return nil },
		releaseUnsent: func(context.Context, *UpstreamRejectionReservation) error { return nil },
		resolveKey:    func(context.Context, *UpstreamRejectionReservation) (*APIKey, error) { return nil, nil },
		reconcile:     func(context.Context, *UpstreamRejectionReservation, *APIKey) (bool, error) { return false, nil },
	}
	w.runOnce(context.Background())
	require.Empty(t, stub.finalized)
	require.Equal(t, 1, projected, "usage projection still runs")
	require.Equal(t, 2, pendingScans, "the unsent recovery and the task scan still run")
}

func TestUpstreamRejectionPauseSwitchReadsTheGatewaySettingsStore(t *testing.T) {
	require.Nil(t, rejectionPauseSettingReader(nil))
	require.Nil(t, rejectionPauseSettingReader(&OpenAIGatewayService{}))
	require.Nil(t, rejectionPauseSettingReader(&OpenAIGatewayService{settingService: &SettingService{}}))
	// A nil reader must be a nil interface, or the sweep would call into it.
	w := &UpstreamRejectionRecovery{settings: rejectionPauseSettingReader(&OpenAIGatewayService{})}
	require.False(t, w.deadlineReleasePaused(context.Background()))

	store := &rejectionPauseSettings{value: "true"}
	reader := rejectionPauseSettingReader(&OpenAIGatewayService{settingService: &SettingService{settingRepo: store}})
	require.NotNil(t, reader)
	w = &UpstreamRejectionRecovery{settings: reader}
	require.True(t, w.deadlineReleasePaused(context.Background()))
	require.Equal(t, 1, store.reads)
}

// The lines an operator relies on must survive a deployment that logs at warn
// and above: the summary of a run that released holds, and every change of the
// pause switch. Only a run in which a release failed is an error.
func TestUpstreamRejectionDeadlineReleaseLogLevels(t *testing.T) {
	sink, restore := captureStructuredLog(t)
	defer restore()
	levels := func(substr string) []string {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		var found []string
		for _, ev := range sink.events {
			if ev != nil && strings.Contains(ev.Message, substr) {
				found = append(found, strings.ToLower(ev.Level))
			}
		}
		return found
	}
	ctx := context.Background()

	// A run that released holds is a warning, not an error, although the line
	// names its "failed" counter.
	w := &UpstreamRejectionRecovery{dueRepo: &dueReleaserStub{rows: dueRows(2), finalize: applied}}
	w.releaseDueReservations(ctx)
	require.Equal(t, []string{"warn"}, levels("deadline release: listed=2 released=2 amount=0.20000000 lost_races=0 failed=0"))

	// A run in which a release failed is an error.
	failing := &dueReleaserStub{rows: dueRows(1), finalize: func(context.Context, *UpstreamRejectionFinalizeCommand) (*UpstreamRejectionBillingResult, error) {
		return nil, errors.New("database unavailable")
	}}
	w = &UpstreamRejectionRecovery{dueRepo: failing}
	w.releaseDueReservations(ctx)
	require.Equal(t, []string{"error"}, levels("deadline release: listed=1 released=0 amount=0.00000000 lost_races=0 failed=1"))

	// Pausing is said once, then resuming, then an unreadable switch.
	settings := &rejectionPauseSettings{value: "true"}
	w = &UpstreamRejectionRecovery{dueRepo: &dueReleaserStub{rows: dueRows(1), finalize: applied}, settings: settings}
	w.releaseDueReservations(ctx)
	w.releaseDueReservations(ctx)
	require.Equal(t, []string{"warn"}, levels("deadline release paused by setting "+SettingKeyUpstreamRejectionDeadlineReleasePaused))
	settings.value = "false"
	w.releaseDueReservations(ctx)
	require.Equal(t, []string{"warn"}, levels("deadline release resumed"))
	settings.err = errors.New("database unavailable")
	w.releaseDueReservations(ctx)
	require.Equal(t, []string{"warn"}, levels("deadline release skipped: pause switch unreadable"))

	// Every line of the deadline release carries its component and none is
	// below warn.
	sink.mu.Lock()
	defer sink.mu.Unlock()
	seen := 0
	for _, ev := range sink.events {
		if ev == nil || !strings.HasPrefix(ev.Message, "deadline release") {
			continue
		}
		seen++
		require.Contains(t, []string{"warn", "error"}, strings.ToLower(ev.Level), ev.Message)
		require.Equal(t, "service.rejection_recovery", ev.Fields["component"], ev.Message)
	}
	require.Equal(t, 6, seen)
}
