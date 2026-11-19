package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestIntelligenceDrawingRotationPersistsAndDoesNotReplay(t *testing.T) {
	svc, provider, owner, now := intelligenceFixture(t)
	plan := seedIntelligencePlan(t, svc, true)
	_ = svc.Store.change(func(st *intelligenceState) error {
		p := st.Plans[plan.ID]
		p.Plan.CreativeEveryFive = true
		p.Plan.CandySeconds = 0
		st.Plans[plan.ID] = p
		return nil
	})
	for i := 1; i <= 10; i++ {
		operation := fmt.Sprintf("rotation-request-%02d", i)
		run, err := svc.Enqueue(plan.ID, "pelican", operation, false)
		if err != nil {
			t.Fatal(err)
		}
		expected := "pelican"
		if i%5 == 0 {
			expected = "creative"
		}
		if run.Benchmark != expected {
			t.Fatalf("round %d: %s", i, run.Benchmark)
		}
		duplicate, err := svc.Enqueue(plan.ID, "pelican", operation, false)
		if err != nil || duplicate.ID != run.ID || svc.Store.snapshot().Plans[plan.ID].Plan.DrawingSequence != uint64(i) {
			t.Fatal("retry advanced rotation or created a second job", err)
		}
		svc.submit(context.Background(), run.ID)
		if provider.submissions[i-1].Benchmark != expected {
			t.Fatal("provider received wrong benchmark")
		}
		// Failed slots also count. No compensating paid call is created.
		svc.fail(run.ID, "failed", "test_failed")
		*now = now.Add(20 * time.Minute)
		if i == 4 {
			store, err := OpenIntelligenceStore(owner)
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := NewIntelligenceService(store, "https://main.example", testIntelligenceProviderOrigin)
			if err != nil {
				t.Fatal(err)
			}
			reopened.provider, reopened.now = provider, svc.now
			svc = reopened
			if intelligenceNextDrawing(svc.Store.snapshot().Plans[plan.ID].Plan) != "creative" {
				t.Fatal("restart reset the fifth slot")
			}
		}
	}
	if provider.submits != 10 {
		t.Fatal("rotation added requests")
	}
	p := svc.Store.snapshot().Plans[plan.ID].Plan
	if p.KeyID != plan.KeyID || p.Quota != plan.Quota || p.DailyLimit != plan.DailyLimit || p.PelicanSeconds != 1200 {
		t.Fatal("rotation changed billing or interval")
	}
}

func TestIntelligenceDrawingSharesSlotAndManualPreservesRotation(t *testing.T) {
	svc, _, _, _ := intelligenceFixture(t)
	plan := seedIntelligencePlan(t, svc, false)
	_ = svc.Store.change(func(st *intelligenceState) error {
		p := st.Plans[plan.ID]
		p.Plan.CreativeEveryFive = true
		p.Plan.DrawingSequence = 4
		st.Plans[plan.ID] = p
		return nil
	})
	svc.schedule()
	if len(svc.Store.snapshot().Runs) != 0 {
		t.Fatal("paused group scheduled a job")
	}
	manual, err := svc.Enqueue(plan.ID, "creative", "manual-creative-request", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Enqueue(plan.ID, "pelican", "manual-pelican-request", true); !errors.Is(err, ErrBusy) {
		t.Fatal("drawing types occupied two slots", err)
	}
	p := svc.Store.snapshot().Plans[plan.ID].Plan
	if p.DrawingSequence != 4 || !p.NextPelican.IsZero() || p.Enabled {
		t.Fatal("manual run changed schedule or enabled paused group")
	}
	svc.fail(manual.ID, "failed", "test_failed")
}

func TestIntelligenceMixedRetentionSharesTwentyResults(t *testing.T) {
	svc, _, _, now := intelligenceFixture(t)
	plan := seedIntelligencePlan(t, svc, false)
	_ = svc.Store.change(func(st *intelligenceState) error {
		p := st.Plans[plan.ID]
		p.Plan.CreativeEveryFive = true
		p.Plan.DrawingSequence = 99
		st.Plans[plan.ID] = p
		for i := 0; i < 25; i++ {
			kind := "pelican"
			if i%5 == 4 {
				kind = "creative"
			}
			id := fmt.Sprintf("%032x", i+1)
			st.Runs[id] = intelligenceRunRecord{Run: IntelligenceRun{ID: id, PlanID: plan.ID, Benchmark: kind, Status: "succeeded", StartedAt: now.Add(time.Duration(i-25) * time.Minute)}}
		}
		return nil
	})
	if err := svc.Store.prune(*now); err != nil {
		t.Fatal(err)
	}
	st := svc.Store.snapshot()
	if len(st.Runs) != 20 || st.Plans[plan.ID].Plan.DrawingSequence != 99 {
		t.Fatal("retention split types or reset the sequence")
	}
}

func TestIntelligenceCreativeToggleKeepsNativeKeyAndPausedPlan(t *testing.T) {
	s, native := intelligenceAPIFixture(t)
	res := request(s, "POST", "/api/intelligence-tests/plans", intelligenceInputJSON(0, false), "fixture-admin", s.PublicOrigin)
	var p IntelligencePlan
	_ = json.Unmarshal(res.Body.Bytes(), &p)
	body := strings.TrimSuffix(intelligenceInputJSON(p.Revision, false), "}") + `,"creative_every_five":true}`
	res = request(s, "PUT", "/api/intelligence-tests/plans/"+p.ID, body, "fixture-admin", s.PublicOrigin)
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body.String())
	}
	_ = json.Unmarshal(res.Body.Bytes(), &p)
	if !p.CreativeEveryFive || p.Enabled || native.creates != 1 || native.updates != 0 {
		t.Fatal("toggle changed keys, quotas or pause")
	}
	res = request(s, "PUT", "/api/intelligence-tests/plans/"+p.ID, intelligenceInputJSON(p.Revision, false), "fixture-admin", s.PublicOrigin)
	_ = json.Unmarshal(res.Body.Bytes(), &p)
	if res.Code != 200 || !p.CreativeEveryFive {
		t.Fatal("old client cleared omitted toggle")
	}
}

func TestIntelligenceCreativeResultResumesWithoutAnotherSubmit(t *testing.T) {
	svc, provider, owner, now := intelligenceFixture(t)
	plan := seedIntelligencePlan(t, svc, false)
	run, err := svc.Enqueue(plan.ID, "creative", "realistic-creative-operation", true)
	if err != nil {
		t.Fatal(err)
	}
	svc.tick()
	store, err := OpenIntelligenceStore(owner)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewIntelligenceService(store, "https://main.example", testIntelligenceProviderOrigin)
	if err != nil {
		t.Fatal(err)
	}
	reopened.provider, reopened.now = provider, svc.now
	*now = now.Add(6 * time.Second)
	provider.result.Status, provider.result.Benchmark = "succeeded", "creative"
	provider.result.Result.HTML = "<html><body>Creative result</body></html>"
	provider.result.Assessment.Quality = "normal"
	reopened.tick()
	result, err := store.result(run.ID)
	if err != nil || result.HTML != provider.result.Result.HTML || !store.snapshot().Runs[run.ID].Run.HasHTML || provider.submits != 1 || provider.polls != 1 {
		t.Fatal("creative result or restart contract failed", err)
	}
	if provider.submissions[0].APIKey != "sk-private-monitor-fixture" || provider.submissions[0].Effort != "medium" || provider.submissions[0].BaseURL != "https://main.example/v1" {
		t.Fatal("existing credentials or model settings changed")
	}
}
