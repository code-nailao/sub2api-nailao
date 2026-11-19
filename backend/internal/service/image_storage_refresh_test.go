//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"sources.local/mediaengine"
)

func imageStorageRefreshSettings() ImageStorageSettings {
	return ImageStorageSettings{
		Enabled: true, Endpoint: "https://old.example", Region: "auto",
		Bucket: "images", AccessKeyID: "old-key", SecretAccessKey: "old-secret",
		Prefix: "images/", PresignExpiry: 24, MaxDownloadBytes: defaultImageMaxDownloadBytes,
	}
}

// The peer shares only persistence and test factories, not a service cache.
func imageStorageRefreshPeer(s *ImageStorageSettingService) *ImageStorageSettingService {
	backup := NewBackupService(s.settingRepo, &config.Config{
		Totp: config.TotpConfig{EncryptionKeyConfigured: true},
	}, s.encryptor, nil, nil)
	return NewImageStorageSettingService(s.settingRepo, s.encryptor, backup, s.factory, s.fallback)
}

func TestImageStorageSettingsRefreshAcrossInstances(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serving, _, built := newImageStorageFixture(t, config.ImageStorageConfig{})
		owner := imageStorageRefreshPeer(serving)
		ctx := context.Background()
		_, enabled := serving.Resolver()()
		require.False(t, enabled)

		settings := imageStorageRefreshSettings()
		_, err := owner.Update(ctx, settings)
		require.NoError(t, err)
		_, enabled = serving.resolve()
		require.False(t, enabled, "another instance's update does not invalidate this cache")
		time.Sleep(imageStorageRefreshInterval)
		original, enabled := serving.resolve()
		require.True(t, enabled)
		require.Len(t, *built, 1)

		settings.Enabled = false
		_, err = owner.Update(ctx, settings)
		require.NoError(t, err)
		time.Sleep(imageStorageRefreshInterval)
		uploader, enabled := serving.resolve()
		require.False(t, enabled)
		require.Nil(t, uploader, "disabled storage must not expose the remembered client")

		settings.Enabled = true
		_, err = owner.Update(ctx, settings)
		require.NoError(t, err)
		time.Sleep(imageStorageRefreshInterval)
		uploader, enabled = serving.resolve()
		require.True(t, enabled)
		require.Same(t, original, uploader)
		require.Len(t, *built, 1, "reenabling unchanged storage reuses the client")

		settings.AccessKeyID, settings.SecretAccessKey = "new-key", "new-secret"
		settings.Bucket, settings.Endpoint, settings.Prefix = "new-images", "https://new.example", "new/"
		_, err = owner.Update(ctx, settings)
		require.NoError(t, err)
		time.Sleep(imageStorageRefreshInterval)
		uploader, enabled = serving.resolve()
		require.True(t, enabled)
		require.NotSame(t, original, uploader)
		require.Len(t, *built, 2)
		require.Equal(t, "new-key", (*built)[1].AccessKeyID)
		require.Equal(t, "new-secret", (*built)[1].SecretAccessKey)
		require.Equal(t, "new-images", (*built)[1].Bucket)
		require.Equal(t, "https://new.example", (*built)[1].Endpoint)
		require.Equal(t, "new/", uploader.prefix)
		require.Equal(t, "images/", original.prefix, "refresh never mutates borrowed uploaders")

		for range 3 {
			time.Sleep(imageStorageRefreshInterval)
			current, available := serving.resolve()
			require.True(t, available)
			require.Same(t, uploader, current)
		}
		require.Len(t, *built, 2, "unchanged effective configuration does not recreate clients")
	})
}

func TestImageStorageSettingsRefreshBackupCredentialsAcrossInstances(t *testing.T) {
	for _, bucket := range []string{"", "separate-images"} {
		t.Run("image_bucket="+bucket, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				serving, _, built := newImageStorageFixture(t, config.ImageStorageConfig{})
				owner := imageStorageRefreshPeer(serving)
				ctx := context.Background()
				backupCfg := BackupS3Config{
					Endpoint: "https://old.example", Region: "auto", Bucket: "backup",
					AccessKeyID: "backup-key", SecretAccessKey: "backup-secret", Prefix: "backups/",
				}
				_, err := owner.backup.UpdateS3Config(ctx, backupCfg)
				require.NoError(t, err)
				_, err = owner.Update(ctx, ImageStorageSettings{Enabled: true, ReuseBackupS3: true, Bucket: bucket})
				require.NoError(t, err)
				original, enabled := serving.resolve()
				require.True(t, enabled)

				backupCfg.Prefix = "different-backup-prefix/"
				_, err = owner.backup.UpdateS3Config(ctx, backupCfg)
				require.NoError(t, err)
				time.Sleep(imageStorageRefreshInterval)
				uploader, enabled := serving.resolve()
				require.True(t, enabled)
				require.Same(t, original, uploader, "backup-only settings do not rebuild image clients")
				require.Len(t, *built, 1)

				backupCfg.AccessKeyID, backupCfg.SecretAccessKey = "rotated-key", "rotated-secret"
				backupCfg.Endpoint, backupCfg.Region = "https://new.example", "new-region"
				backupCfg.Bucket, backupCfg.ForcePathStyle = "new-backup", true
				_, err = owner.backup.UpdateS3Config(ctx, backupCfg)
				require.NoError(t, err)
				time.Sleep(imageStorageRefreshInterval)
				uploader, enabled = serving.resolve()
				require.True(t, enabled)
				require.NotSame(t, original, uploader)
				require.Len(t, *built, 2)
				got := (*built)[1]
				require.Equal(t, "rotated-key", got.AccessKeyID)
				require.Equal(t, "rotated-secret", got.SecretAccessKey)
				require.Equal(t, "https://new.example", got.Endpoint)
				require.Equal(t, "new-region", got.Region)
				require.True(t, got.ForcePathStyle)
				require.Equal(t, "images/", got.Prefix)
				if bucket == "" {
					require.Equal(t, "new-backup", got.Bucket)
				} else {
					require.Equal(t, bucket, got.Bucket)
				}
			})
		})
	}
}

type imageStorageReadOverride struct {
	SettingRepository
	read func(context.Context, string) (string, error)
}

func (r *imageStorageReadOverride) GetValue(ctx context.Context, key string) (string, error) {
	return r.read(ctx, key)
}

func TestImageStorageSettingsRefreshFailuresDisableAndRecover(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		raw  string
		err  error
	}{
		{name: "image_read", key: settingKeyImageStorageConfig, err: errors.New("database unavailable")},
		{name: "image_parse", key: settingKeyImageStorageConfig, raw: "{"},
		{name: "image_incomplete", key: settingKeyImageStorageConfig, raw: `{"enabled":true,"bucket":"images"}`},
		{name: "backup_read", key: settingKeyBackupS3Config, err: errors.New("database unavailable")},
		{name: "backup_parse", key: settingKeyBackupS3Config, raw: "{"},
		{name: "backup_missing", key: settingKeyBackupS3Config, err: ErrSettingNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// A valid enabled fallback must never hide a database failure.
				fallback := config.ImageStorageConfig{Enabled: true, Bucket: "fallback", AccessKeyID: "key", SecretAccessKey: "secret"}
				serving, repo, built := newImageStorageFixture(t, fallback)
				seedBackupS3(t, repo, BackupS3Config{Bucket: "backup", AccessKeyID: "key", SecretAccessKey: "secret"})
				_, err := serving.Update(context.Background(), ImageStorageSettings{Enabled: true, ReuseBackupS3: true})
				require.NoError(t, err)
				fail := false
				failingRepo := &imageStorageReadOverride{SettingRepository: repo, read: func(ctx context.Context, key string) (string, error) {
					if fail && key == tc.key {
						return tc.raw, tc.err
					}
					return repo.GetValue(ctx, key)
				}}
				serving.settingRepo = failingRepo
				serving.backup.settingRepo = failingRepo
				original, enabled := serving.resolve()
				require.True(t, enabled)

				fail = true
				time.Sleep(imageStorageRefreshInterval)
				uploader, enabled := serving.resolve()
				require.False(t, enabled)
				require.Nil(t, uploader)
				uploader, enabled = serving.resolve()
				require.False(t, enabled)
				require.Nil(t, uploader, "a cached failure cannot expose the last good client")
				require.Len(t, *built, 1)

				fail = false
				time.Sleep(imageStorageRefreshInterval)
				uploader, enabled = serving.resolve()
				require.True(t, enabled)
				require.Same(t, original, uploader)
				require.Len(t, *built, 1, "temporary read errors do not discard a reusable client")
			})
		})
	}
}

func TestImageStorageSettingsMissingRowStillUsesFallback(t *testing.T) {
	svc, repo, built := newImageStorageFixture(t, config.ImageStorageConfig{
		Enabled: true, Bucket: "fallback", AccessKeyID: "key", SecretAccessKey: "secret",
	})
	svc.settingRepo = &imageStorageReadOverride{SettingRepository: repo, read: func(context.Context, string) (string, error) {
		return "", ErrSettingNotFound
	}}
	_, enabled := svc.resolve()
	require.True(t, enabled)
	require.Len(t, *built, 1)
	require.Equal(t, "fallback", (*built)[0].Bucket)
}

func TestImageStorageSettingsRefreshReadTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, repo, built := newImageStorageFixture(t, config.ImageStorageConfig{
			Enabled: true, Bucket: "fallback", AccessKeyID: "key", SecretAccessKey: "secret",
		})
		reads := 0
		svc.settingRepo = &imageStorageReadOverride{SettingRepository: repo, read: func(ctx context.Context, _ string) (string, error) {
			reads++
			<-ctx.Done()
			return "", ctx.Err()
		}}
		started := time.Now()
		uploader, enabled := svc.resolve()
		require.False(t, enabled)
		require.Nil(t, uploader)
		require.Equal(t, imageStorageRefreshTimeout, time.Since(started))
		for range 10 {
			svc.resolve()
		}
		require.Equal(t, 1, reads, "failed reads are throttled until the next refresh")
		require.Empty(t, *built)
	})
}

func TestImageStorageSettingsRefreshBuildFailureRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serving, _, _ := newImageStorageFixture(t, config.ImageStorageConfig{})
		builds, fail := 0, false
		serving.factory = func(context.Context, *config.ImageStorageConfig) (ImageStorage, error) {
			builds++
			if fail {
				return nil, errors.New("client initialization failed")
			}
			return &recordingStorage{}, nil
		}
		owner := imageStorageRefreshPeer(serving)
		settings := imageStorageRefreshSettings()
		_, err := owner.Update(context.Background(), settings)
		require.NoError(t, err)
		original, enabled := serving.resolve()
		require.True(t, enabled)
		settings.SecretAccessKey = "rotated-secret"
		_, err = owner.Update(context.Background(), settings)
		require.NoError(t, err)
		fail = true
		time.Sleep(imageStorageRefreshInterval)
		uploader, enabled := serving.resolve()
		require.False(t, enabled)
		require.Nil(t, uploader, "cannot use stale credentials when a replacement fails")
		require.Equal(t, 2, builds)
		fail = false
		svcUploader, available := serving.resolve()
		require.False(t, available)
		require.Nil(t, svcUploader)
		require.Equal(t, 2, builds)
		time.Sleep(imageStorageRefreshInterval)
		uploader, enabled = serving.resolve()
		require.True(t, enabled)
		require.NotSame(t, original, uploader)
		require.Equal(t, 3, builds)
	})
}

type imageStorageHeldSave struct {
	entered chan struct{}
	release chan struct{}
	baseURL string
}

func (s *imageStorageHeldSave) Save(_ context.Context, key, _ string, _ []byte) (string, error) {
	if s.entered != nil {
		close(s.entered)
		<-s.release
	}
	return s.baseURL + "/" + key, nil
}

func TestImageStorageSettingsRefreshPreservesInflightUpload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serving, _, _ := newImageStorageFixture(t, config.ImageStorageConfig{})
		oldStorage := &imageStorageHeldSave{entered: make(chan struct{}), release: make(chan struct{}), baseURL: "https://old.example"}
		var releaseOnce sync.Once
		releaseOld := func() { releaseOnce.Do(func() { close(oldStorage.release) }) }
		defer releaseOld()
		serving.factory = func(_ context.Context, cfg *config.ImageStorageConfig) (ImageStorage, error) {
			if cfg.Endpoint == oldStorage.baseURL {
				return oldStorage, nil
			}
			return &imageStorageHeldSave{baseURL: cfg.Endpoint}, nil
		}
		owner := imageStorageRefreshPeer(serving)
		settings := imageStorageRefreshSettings()
		_, err := owner.Update(context.Background(), settings)
		require.NoError(t, err)
		original, enabled := serving.resolve()
		require.True(t, enabled)
		var oldResult json.RawMessage
		var oldErr error
		done := make(chan struct{})
		input := json.RawMessage(`{"data":[{"b64_json":"aW1hZ2U="}]}`)
		go func() {
			oldResult, oldErr = original.Rewrite(context.Background(), "old-task", input)
			close(done)
		}()
		<-oldStorage.entered

		settings.Endpoint, settings.Bucket, settings.Prefix = "https://new.example", "new-images", "new/"
		_, err = owner.Update(context.Background(), settings)
		require.NoError(t, err)
		time.Sleep(imageStorageRefreshInterval)
		current, enabled := serving.resolve()
		require.True(t, enabled)
		newResult, err := current.Rewrite(context.Background(), "new-task", input)
		require.NoError(t, err)
		require.Contains(t, string(newResult), "https://new.example/new/")
		releaseOld()
		<-done
		require.NoError(t, oldErr)
		require.Contains(t, string(oldResult), "https://old.example/images/")
	})
}

func TestImageStorageSettingsConcurrentRefreshBuildsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serving, _, built := newImageStorageFixture(t, config.ImageStorageConfig{})
		owner := imageStorageRefreshPeer(serving)
		settings := imageStorageRefreshSettings()
		_, err := owner.Update(context.Background(), settings)
		require.NoError(t, err)
		_, enabled := serving.resolve()
		require.True(t, enabled)
		settings.SecretAccessKey = "rotated-secret"
		_, err = owner.Update(context.Background(), settings)
		require.NoError(t, err)
		time.Sleep(imageStorageRefreshInterval)
		var wg sync.WaitGroup
		var results [32]*ImageResultUploader
		var available [32]bool
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], available[i] = serving.resolve()
			}()
		}
		wg.Wait()
		for i := range results {
			require.True(t, available[i])
			require.Same(t, results[0], results[i])
		}
		require.Len(t, *built, 2, "concurrent refresh requests share one replacement client")
	})
}

func TestImageStorageSettingsRefreshMediaEngineResultAcrossInstances(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serving, _, _ := newImageStorageFixture(t, config.ImageStorageConfig{})
		serving.factory = func(_ context.Context, cfg *config.ImageStorageConfig) (ImageStorage, error) {
			return &sourcesObjectFixture{id: cfg.Endpoint + "/" + cfg.Bucket}, nil
		}
		owner := imageStorageRefreshPeer(serving)
		settings := imageStorageRefreshSettings()
		_, err := owner.Update(context.Background(), settings)
		require.NoError(t, err)
		_, enabled := serving.resolve()
		require.True(t, enabled)
		oldObject, err := json.Marshal(sourcesImageObject{StoreID: settings.Endpoint + "/" + settings.Bucket, Key: "old-result"})
		require.NoError(t, err)
		settings.Bucket = "new-images"
		_, err = owner.Update(context.Background(), settings)
		require.NoError(t, err)
		ownerMediaEngine := &SourcesMediaEngine{images: NewImageTaskServiceWithResolver(nil, owner.Resolver(), time.Hour, time.Minute)}
		servingMediaEngine := &SourcesMediaEngine{images: NewImageTaskServiceWithResolver(nil, serving.Resolver(), time.Hour, time.Minute)}
		var pngData bytes.Buffer
		require.NoError(t, png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 1, 1))))
		task := &mediaengine.Task{ID: "mediatask_refresh", Phase: "completed", CreatedAt: 1}
		task.Result, err = ownerMediaEngine.StoreResult(context.Background(), task, []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData.Bytes())})
		require.NoError(t, err)
		_, err = servingMediaEngine.Result(context.Background(), task)
		require.ErrorContains(t, err, "original task storage", "the warm serving instance still has its old cache")
		time.Sleep(imageStorageRefreshInterval)
		result, err := servingMediaEngine.Result(context.Background(), task)
		require.NoError(t, err)
		require.Contains(t, string(result), "signature=1")
		task.Result = oldObject
		_, err = servingMediaEngine.Result(context.Background(), task)
		require.ErrorContains(t, err, "original task storage", "never sign an old object's key in the new bucket")
	})
}
