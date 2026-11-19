package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"

	"sources.local/mediaengine"
)

// The policy contains no credentials and is mounted explicitly by the operator.
// Absence disables automatic customer closure; invalid configuration fails closed.
func loadSourcesMediaEngineRecoveryPolicy(path string) (*mediaengine.RecoveryPolicy, error) {
	if path == "" {
		return nil, nil
	}
	// #nosec G703 -- Fixed operator-controlled configuration path, never request input.
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("media engine recovery policy unavailable")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<10 || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("invalid media engine recovery policy file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (128<<10)+1))
	if err != nil || len(raw) > 128<<10 {
		return nil, errors.New("invalid media engine recovery policy size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var policy mediaengine.RecoveryPolicy
	if decoder.Decode(&policy) != nil {
		return nil, errors.New("invalid media engine recovery policy JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid trailing media engine recovery policy data")
	}
	if err = policy.Validate(); err != nil {
		return nil, err
	}
	return &policy, nil
}

// The immutable policy can be mounted before handoff. A separate, atomically
// written marker activates it afterwards without restarting the sole owner.
// Removal stops new automated decisions; existing release intents still finish.
func sourcesMediaEngineRecoveryEnabled(path, policyID string) bool {
	if path == "" || policyID == "" {
		return false
	}
	// #nosec G703 -- Fixed operator-controlled marker path, never request input.
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(policyID)) || info.Mode().Perm()&0022 != 0 {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(len(policyID))+1))
	return err == nil && string(raw) == policyID
}

func (s *SourcesMediaEngine) UnknownReleaseAmount(t *mediaengine.Task) (int64, error) {
	if t == nil {
		return 0, mediaengine.ErrConflict
	}
	ticket, err := sourcesImageTicket(t)
	if err != nil {
		return 0, err
	}
	if QuantizeUsageBillingAmount(ticket.Amount) != ticket.Amount {
		return 0, mediaengine.ErrConflict
	}
	return mediaengine.ParseRecoveryAmount(strconv.FormatFloat(ticket.Amount, 'f', 8, 64))
}

func (s *SourcesMediaEngine) RecoverySummary(t *mediaengine.Task) mediaengine.RecoverySummary {
	if s == nil {
		return mediaengine.RecoverySummary{Status: "disabled", ManualActionRequired: true}
	}
	producer := s.taskProducer()
	return producer.RecoverySummary(t)
}
