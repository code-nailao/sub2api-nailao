//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"sources.local/mediaengine"
)

// The legacy values in these tests are deliberately fake. With them configured
// every value must equal what earlier releases wrote in that place.
const (
	testMediaEngineLegacyName       = "legacyname"
	testMediaEngineLegacyTaskPrefix = "legacytask"
)

// useSourcesMediaEngineLegacyName publishes a legacy name together with a task
// prefix, as a configured deployment always has both.
func useSourcesMediaEngineLegacyName(t *testing.T, name string) mediaengine.Namespace {
	t.Helper()
	prefix := ""
	switch name {
	case "":
	case testMediaEngineLegacyName:
		prefix = testMediaEngineLegacyTaskPrefix
	default:
		prefix = "othertask"
	}
	SetSourcesMediaEngineLegacy(name, prefix)
	t.Cleanup(func() { SetSourcesMediaEngineLegacy("", "") })
	ns, err := sourcesMediaEngineNamespace()
	require.NoError(t, err)
	return ns
}

type sourcesMediaEngineNamespaceCase struct {
	name, legacy                                          string
	selector, billing, priceHigh, priceStandard, priceMid string
	inputKey, resultKey, taskID                           string
}

var sourcesMediaEngineNamespaceCases = []sourcesMediaEngineNamespaceCase{
	{
		name: "neutral", legacy: "",
		selector:      "media",
		billing:       "media-engine-image:mediatask_x",
		priceHigh:     "internal/media-engine/gpt-image-2/high",
		priceStandard: "internal/media-engine/gpt-image-2/standard",
		priceMid:      "internal/media-engine/gpt-image-medium",
		inputKey:      "sources-media-engine/input-edit-hash-0.png",
		resultKey:     "sources-media-engine/result-mediatask_x-0.png",
		taskID:        `^mediatask_[a-f0-9]{32}$`,
	},
	{
		name: "legacy", legacy: testMediaEngineLegacyName,
		selector:      "legacyname",
		billing:       "legacyname-image:mediatask_x",
		priceHigh:     "internal/legacyname/gpt-image-2/high",
		priceStandard: "internal/legacyname/gpt-image-2/standard",
		priceMid:      "internal/legacyname/gpt-image-medium",
		inputKey:      "sources-legacyname/input-edit-hash-0.png",
		resultKey:     "sources-legacyname/result-mediatask_x-0.png",
		taskID:        `^legacytask_[a-f0-9]{32}$`,
	},
}

func TestSourcesMediaEnginePersistedValues(t *testing.T) {
	for _, tc := range sourcesMediaEngineNamespaceCases {
		t.Run(tc.name, func(t *testing.T) {
			ns := useSourcesMediaEngineLegacyName(t, tc.legacy)
			require.Equal(t, tc.selector, ns.Selector())
			require.Equal(t, tc.billing, sourcesMediaEngineBillingRequestID(ns, "mediatask_x"))
			require.Equal(t, tc.priceHigh, sourcesMediaEnginePricingModel(ns, "gpt-image-2", "high"))
			require.Equal(t, tc.priceStandard, sourcesMediaEnginePricingModel(ns, "gpt-image-2", "medium"))
			require.Equal(t, tc.priceMid, sourcesMediaEnginePricingModel(ns, "gpt-image-medium", "high"))
			require.Equal(t, "gemini-3-pro-image", sourcesMediaEnginePricingModel(ns, "gemini-3-pro-image", "high"))

			storage := &sourcesMediaEngineRecordingStorage{sourcesObjectFixture: sourcesObjectFixture{id: "store"}}
			svc := &SourcesMediaEngine{ns: ns, images: NewImageTaskServiceWithUploader(nil, NewImageResultUploader(storage, "", 0, nil), time.Hour, time.Minute)}
			ref := sourcesMediaEngineTestPNG(t)
			_, objects, err := svc.stageInputs(context.Background(), &sourcesMediaEngineRequest{Hash: "hash", refs: []string{ref}}, "edit")
			require.NoError(t, err)
			require.Len(t, objects, 1)
			require.Equal(t, tc.inputKey, objects[0].Key)
			_, err = svc.StoreResult(context.Background(), &mediaengine.Task{ID: "mediatask_x"}, []string{ref})
			require.NoError(t, err)
			require.Equal(t, []string{tc.inputKey, tc.resultKey}, storage.keys)

			// Task ids: generated with the namespace prefix, read back, and
			// recognised by the prefix check the handler and ledger use.
			store, err := mediaengine.Open(filepath.Join(t.TempDir(), "tasks"), bytes.Repeat([]byte{7}, 32), ns)
			require.NoError(t, err)
			defer store.Close()
			producer, err := mediaengine.NewProducer(store, mediaengine.Options{})
			require.NoError(t, err)
			task, err := producer.Create(mediaengine.Input{Owner: mediaengine.Owner{UserID: 1, KeyID: 2, GroupID: 3}, IdempotencyKey: "idem", PayloadHash: strings.Repeat("a", 64), Body: []byte(`{}`), Ticket: []byte(`{}`), Credential: mediaengine.Credential{AccountID: 4}})
			require.NoError(t, err)
			require.Regexp(t, tc.taskID, task.ID)
			got, err := producer.Get(task.Owner, task.ID)
			require.NoError(t, err)
			require.Equal(t, task.ID, got.ID)
			require.True(t, HasSourcesMediaEngineTaskPrefix(task.ID))
			require.Equal(t, sourcesMediaEngineBillingPrefix(ns.Name())+task.ID, sourcesMediaEngineBillingRequestID(ns, task.ID), "billing ids embed the full task id")
		})
	}
}

func TestSourcesMediaEngineAcceptsBothForms(t *testing.T) {
	t.Run("legacy configured", func(t *testing.T) {
		ns := useSourcesMediaEngineLegacyName(t, testMediaEngineLegacyName)
		for _, id := range []string{"legacyname-image:mediatask_x", "media-engine-image:mediatask_x"} {
			require.True(t, IsSourcesMediaEngineBillingRequestID(id, "mediatask_x"), id)
		}
		for _, id := range []string{"otherlegacy-image:mediatask_x", "legacyname-image:mediatask_y", "mediatask_x"} {
			require.False(t, IsSourcesMediaEngineBillingRequestID(id, "mediatask_x"), id)
		}
		for _, model := range []string{"internal/legacyname/gpt-image-2/high", "internal/media-engine/gpt-image-2/high"} {
			require.True(t, isSourcesMediaEnginePriceModel(ns, model), model)
		}
		require.False(t, isSourcesMediaEnginePriceModel(ns, "gpt-image-2"))
		require.True(t, ns.IsSelector("legacyname"))
		require.True(t, ns.IsSelector("media"))
		require.False(t, ns.IsSelector("sub2api"))
		for _, id := range []string{"legacytask_x", "mediatask_x"} {
			require.True(t, HasSourcesMediaEngineTaskPrefix(id), id)
		}
		for _, id := range []string{"othertask_x", "legacytaskx", "x"} {
			require.False(t, HasSourcesMediaEngineTaskPrefix(id), id)
		}
	})
	t.Run("neutral", func(t *testing.T) {
		ns := useSourcesMediaEngineLegacyName(t, "")
		require.True(t, IsSourcesMediaEngineBillingRequestID("media-engine-image:mediatask_x", "mediatask_x"))
		require.False(t, IsSourcesMediaEngineBillingRequestID("legacyname-image:mediatask_x", "mediatask_x"))
		require.True(t, isSourcesMediaEnginePriceModel(ns, "internal/media-engine/gpt-image-medium"))
		require.False(t, isSourcesMediaEnginePriceModel(ns, "internal/legacyname/gpt-image-medium"))
		require.True(t, ns.IsSelector("media"))
		require.False(t, ns.IsSelector("legacyname"))
		require.True(t, HasSourcesMediaEngineTaskPrefix("mediatask_x"))
		require.False(t, HasSourcesMediaEngineTaskPrefix("legacytask_x"))
	})
	t.Run("an invalid published name matches nothing", func(t *testing.T) {
		useSourcesMediaEngineLegacyName(t, "")
		SetSourcesMediaEngineLegacy("Not-Valid", testMediaEngineLegacyTaskPrefix)
		require.False(t, IsSourcesMediaEngineBillingRequestID("media-engine-image:mediatask_x", "mediatask_x"))
		require.False(t, HasSourcesMediaEngineTaskPrefix("mediatask_x"))
		_, err := sourcesMediaEngineNamespace()
		require.Error(t, err)
		require.NotContains(t, err.Error(), "Not-Valid")
	})
	t.Run("only one of the two published matches nothing", func(t *testing.T) {
		useSourcesMediaEngineLegacyName(t, "")
		for _, v := range [][2]string{{testMediaEngineLegacyName, ""}, {"", testMediaEngineLegacyTaskPrefix}} {
			SetSourcesMediaEngineLegacy(v[0], v[1])
			_, err := sourcesMediaEngineNamespace()
			require.Error(t, err)
			require.NotContains(t, err.Error(), testMediaEngineLegacyName)
			require.NotContains(t, err.Error(), testMediaEngineLegacyTaskPrefix)
			require.False(t, HasSourcesMediaEngineTaskPrefix("mediatask_x"))
		}
	})
}

// TestSourcesMediaEngineStartupNamespace drives the real constructor: which
// registry values select the engine, and that a store written under one name
// is refused under another instead of silently starting empty.
func TestSourcesMediaEngineStartupNamespace(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "media-engine.key")
	require.NoError(t, os.WriteFile(key, []byte(strings.Repeat("ab", 32)), 0600))
	clearSourcesMediaEngineOptionalEnv(t)
	t.Setenv("SOURCES_MEDIA_ENGINE_KEY_FILE", key)

	start := func(t *testing.T, legacy, engine, data string) (*SourcesMediaEngine, error) {
		t.Helper()
		registry := filepath.Join(t.TempDir(), "image-engines.json")
		require.NoError(t, os.WriteFile(registry, []byte(`[{"group_id":7,"engine":"`+engine+`","models":["gpt-image-2"],"account_ids":[9]}]`), 0600))
		t.Setenv("SOURCES_IMAGE_ENGINES_FILE", registry)
		t.Setenv("SOURCES_MEDIA_ENGINE_DATA_DIR", data)
		useSourcesMediaEngineLegacyName(t, legacy)
		svc, err := NewSourcesMediaEngine(newSourcesMediaEngineStartupGateway(), nil, &ImageTaskService{}, &APIKeyService{})
		if svc != nil {
			svc.Stop()
		}
		return svc, err
	}

	for _, tc := range []struct{ legacy, engine, want string }{
		{"", "media", "media"},
		{testMediaEngineLegacyName, "media", "legacyname"},
		{testMediaEngineLegacyName, "legacyname", "legacyname"},
	} {
		svc, err := start(t, tc.legacy, tc.engine, filepath.Join(t.TempDir(), "tasks"))
		require.NoError(t, err, tc)
		require.Equal(t, tc.want, svc.routes[7].Engine, tc)
		require.True(t, svc.IsGroup(7), tc)
	}
	_, err := start(t, "", "legacyname", filepath.Join(t.TempDir(), "tasks"))
	require.ErrorContains(t, err, "unsupported image engine", "a legacy registry needs the legacy name")

	legacyData := filepath.Join(t.TempDir(), "tasks")
	_, err = start(t, testMediaEngineLegacyName, "legacyname", legacyData)
	require.NoError(t, err)
	_, err = start(t, testMediaEngineLegacyName, "media", legacyData)
	require.NoError(t, err, "the same name reopens the store")
	_, err = start(t, "", "media", legacyData)
	require.ErrorContains(t, err, "task store identity or key check failed", "a legacy store is refused without its name")
	_, err = start(t, "otherlegacy", "media", legacyData)
	require.ErrorContains(t, err, "task store identity or key check failed", "a legacy store is refused under another name")
}

// TestSourcesMediaEngineRequiresItsSettings covers a deployment whose registry
// names the engine but whose environment still lacks the engine's settings.
func TestSourcesMediaEngineRequiresItsSettings(t *testing.T) {
	clearSourcesMediaEngineOptionalEnv(t)
	useSourcesMediaEngineLegacyName(t, "")
	registry := filepath.Join(t.TempDir(), "image-engines.json")
	require.NoError(t, os.WriteFile(registry, []byte(`[{"group_id":7,"engine":"media","models":["gpt-image-2"],"account_ids":[9]}]`), 0600))
	t.Setenv("SOURCES_IMAGE_ENGINES_FILE", registry)
	t.Setenv("SOURCES_MEDIA_ENGINE_KEY_FILE", "")
	t.Setenv("SOURCES_MEDIA_ENGINE_DATA_DIR", "")
	svc, err := NewSourcesMediaEngine(newSourcesMediaEngineStartupGateway(), nil, &ImageTaskService{}, &APIKeyService{})
	require.Nil(t, svc)
	require.ErrorContains(t, err, "owner-only media engine key and independent data directory required")
}

func clearSourcesMediaEngineOptionalEnv(t *testing.T) {
	for _, name := range []string{
		"SOURCES_MEDIA_ENGINE_ADMISSION_MODE", "SOURCES_MEDIA_ENGINE_ADMISSION_STATE_FILE",
		"SOURCES_MEDIA_ENGINE_LEGACY_ORIGIN", "SOURCES_MEDIA_ENGINE_RECOVERY_POLICY_FILE",
		"SOURCES_MEDIA_ENGINE_RECOVERY_ENABLE_FILE",
	} {
		t.Setenv(name, "")
	}
}

func newSourcesMediaEngineStartupGateway() *OpenAIGatewayService {
	repo := &laneMediaEngineStartupRepo{result: &UsageBillingApplyResult{Applied: true}}
	return &OpenAIGatewayService{
		usageBillingRepo:    repo,
		usageLogRepo:        &laneMediaEngineStartupUsageLogs{},
		accountRepo:         &laneMediaEngineStartupAccounts{},
		billingService:      &BillingService{},
		billingCacheService: &BillingCacheService{},
		concurrencyService:  &ConcurrencyService{},
		cfg:                 &config.Config{RunMode: config.RunModeStandard},
	}
}

type sourcesMediaEngineRecordingStorage struct {
	sourcesObjectFixture
	keys []string
}

func (s *sourcesMediaEngineRecordingStorage) Save(_ context.Context, key, _ string, _ []byte) (string, error) {
	s.keys = append(s.keys, key)
	return "https://fixture.example/object", nil
}

func sourcesMediaEngineTestPNG(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}
