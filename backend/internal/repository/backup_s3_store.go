package repository

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// S3BackupStore implements service.BackupObjectStore using AWS S3 compatible storage
type S3BackupStore struct {
	client *s3.Client
	bucket string
}

// NewS3BackupStoreFactory returns a BackupObjectStoreFactory that creates S3-backed stores
func NewS3BackupStoreFactory() service.BackupObjectStoreFactory {
	return func(ctx context.Context, cfg *service.BackupS3Config) (service.BackupObjectStore, error) {
		client, err := newS3Client(ctx, s3ClientParams{
			Endpoint:        cfg.Endpoint,
			Region:          cfg.Region,
			AccessKeyID:     cfg.AccessKeyID,
			SecretAccessKey: cfg.SecretAccessKey,
			ForcePathStyle:  cfg.ForcePathStyle,
		})
		if err != nil {
			return nil, err
		}
		return &S3BackupStore{client: client, bucket: cfg.Bucket}, nil
	}
}

func (s *S3BackupStore) Upload(ctx context.Context, key string, body io.Reader, contentType string) (int64, error) {
	// 读取全部内容以获取大小（S3 PutObject 需要知道内容长度）
	// 注意：阿里云 OSS 不兼容 s3manager 分片上传的签名方式，因此使用 PutObject
	data, err := io.ReadAll(body)
	if err != nil {
		return 0, fmt.Errorf("read body: %w", err)
	}

	finish := servertiming.ObserveDependency(ctx, "s3")
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &key,
		Body:        bytes.NewReader(data),
		ContentType: &contentType,
	})
	finish()
	if err != nil {
		return 0, fmt.Errorf("S3 PutObject: %w", err)
	}
	return int64(len(data)), nil
}

func (s *S3BackupStore) UploadFile(ctx context.Context, key string, filePath string, contentType string) (int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("open upload file: %w", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat upload file: %w", err)
	}
	sizeBytes := info.Size()

	finish := servertiming.ObserveDependency(ctx, "s3")
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           &key,
		Body:          file,
		ContentLength: &sizeBytes,
		ContentType:   &contentType,
	})
	finish()
	if err != nil {
		return 0, fmt.Errorf("S3 PutObject file: %w", err)
	}
	return sizeBytes, nil
}

// UploadFileRange uploads a seekable view of one archive, allowing SDK retries
// to rewind without buffering a part in RAM or materializing another file.
func (s *S3BackupStore) UploadFileRange(ctx context.Context, key, filePath string, offset, length int64, contentType string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if offset < 0 || length <= 0 {
		return 0, fmt.Errorf("invalid backup upload range")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("open upload archive: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat upload archive: %w", err)
	}
	if !info.Mode().IsRegular() || offset > info.Size() || length > info.Size()-offset {
		return 0, fmt.Errorf("backup upload range outside archive")
	}
	finish := servertiming.ObserveDependency(ctx, "s3")
	defer finish()
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket, Key: &key, Body: io.NewSectionReader(file, offset, length),
		ContentLength: &length, ContentType: &contentType,
	})
	if err != nil {
		return 0, fmt.Errorf("S3 PutObject file range: %w", err)
	}
	return length, nil
}

func (s *S3BackupStore) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	finish := servertiming.ObserveDependency(ctx, "s3")
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	finish()
	if err != nil {
		return nil, fmt.Errorf("S3 GetObject: %w", err)
	}
	return result.Body, nil
}

func (s *S3BackupStore) Delete(ctx context.Context, key string) error {
	finish := servertiming.ObserveDependency(ctx, "s3")
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	finish()
	return err
}

func (s *S3BackupStore) PresignURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s.client)
	// 强制 attachment disposition：浏览器同页导航该 URL 时直接触发下载而非渲染，
	// 前端无需依赖会被弹窗拦截的新标签页。
	disposition := fmt.Sprintf("attachment; filename=%q", path.Base(key))
	result, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     &s.bucket,
		Key:                        &key,
		ResponseContentDisposition: &disposition,
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", fmt.Errorf("presign url: %w", err)
	}
	return result.URL, nil
}

func (s *S3BackupStore) HeadBucket(ctx context.Context) error {
	finish := servertiming.ObserveDependency(ctx, "s3")
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: &s.bucket,
	})
	finish()
	if err != nil {
		return fmt.Errorf("S3 HeadBucket failed: %w", err)
	}
	return nil
}
