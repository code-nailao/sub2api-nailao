package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var _ service.SourcesImageObjectStorage = (*S3ImageStorage)(nil)

// The optional extension renews media engine object references without changing Save or
// the upstream ordinary Images delivery contract. Credential rotation is allowed;
// switching buckets/endpoints must not silently sign a different object namespace.
func (s *S3ImageStorage) SourcesImageStoreID() string {
	o := s.client.Options()
	endpoint := ""
	if o.BaseEndpoint != nil {
		endpoint = *o.BaseEndpoint
	}
	sum := sha256.Sum256([]byte(endpoint + "\x00" + o.Region + "\x00" + s.bucket))
	return hex.EncodeToString(sum[:])
}
func (s *S3ImageStorage) SourcesImageURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if s.publicBaseURL != "" {
		return s.publicBaseURL + "/" + strings.TrimLeft(key, "/"), nil
	}
	if ttl <= 0 || ttl > 7*24*time.Hour {
		return "", fmt.Errorf("invalid object URL lifetime")
	}
	result, err := s3.NewPresignClient(s.client).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("sign image object: %w", err)
	}
	return result.URL, nil
}
