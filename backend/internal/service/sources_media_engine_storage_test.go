package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"image"
	"image/png"
	"sources.local/mediaengine"
	"testing"
	"time"
)

type sourcesObjectFixture struct {
	calls int
	id    string
}

func (s *sourcesObjectFixture) Save(context.Context, string, string, []byte) (string, error) {
	return "https://fixture.example/object", nil
}
func (s *sourcesObjectFixture) SourcesImageStoreID() string { return s.id }
func (s *sourcesObjectFixture) SourcesImageURL(_ context.Context, key string, ttl time.Duration) (string, error) {
	s.calls++
	return fmt.Sprintf("https://fixture.example/%s?signature=%d&ttl=%d", key, s.calls, int(ttl.Seconds())), nil
}
func TestSourcesMediaEngineResultRenewsOwnedObjectURL(t *testing.T) {
	storage := &sourcesObjectFixture{id: "original-store"}
	svc := &SourcesMediaEngine{images: NewImageTaskServiceWithUploader(nil, NewImageResultUploader(storage, "", 0, nil), time.Hour, time.Minute)}
	object, _ := json.Marshal(sourcesImageObject{StoreID: storage.id, Key: "sources-media-engine/result-fixture"})
	task := &mediaengine.Task{ID: "mediatask_fixture", Phase: "completed", CreatedAt: 1, Result: object}
	a, err := svc.Result(context.Background(), task)
	require.NoError(t, err)
	b, err := svc.Result(context.Background(), task)
	require.NoError(t, err)
	require.NotEqual(t, string(a), string(b))
	require.NotContains(t, string(a), "store_id")
	require.Contains(t, string(a), "signature=1")
	storage.id = "different-bucket"
	_, err = svc.Result(context.Background(), task)
	require.ErrorContains(t, err, "original task storage")
}

func TestSourcesMediaEngineRejectsTruncatedImagePixels(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	service := &SourcesMediaEngine{}
	raw := buf.Bytes()
	_, _, err := service.imageBytes(context.Background(), "data:image/png;base64,"+base64.StdEncoding.EncodeToString(raw), 1<<20)
	require.NoError(t, err)
	_, _, err = service.imageBytes(context.Background(), "data:image/png;base64,"+base64.StdEncoding.EncodeToString(raw[:33]), 1<<20)
	require.ErrorContains(t, err, "incomplete")
}
