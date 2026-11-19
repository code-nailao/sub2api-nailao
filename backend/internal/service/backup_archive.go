package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

const defaultBackupPartSizeBytes int64 = 4 * 1024 * 1024 * 1024

// BackupPart 描述一个 gzip 字节分卷。
type BackupPart struct {
	Index     int    `json:"index"`
	S3Key     string `json:"s3_key"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256,omitempty"`
}

// localBackupPart describes a range of the immutable compressed archive.
// Only metadata is retained: the archive is never copied into temporary parts.
type localBackupPart struct {
	Index     int
	Offset    int64
	SizeBytes int64
	SHA256    string
}

func planBackupFileParts(ctx context.Context, srcPath string, partSize int64) ([]localBackupPart, error) {
	if partSize <= 0 {
		return nil, fmt.Errorf("backup part size must be positive")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("open backup archive: %w", err)
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat backup archive: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return nil, errors.New("backup archive must be a nonempty regular file")
	}
	var parts []localBackupPart
	buffer := make([]byte, 64*1024)
	for offset := int64(0); offset < info.Size(); {
		size := min(partSize, info.Size()-offset)
		hash := sha256.New()
		reader := &backupContextReader{ctx: ctx, reader: io.NewSectionReader(src, offset, size)}
		written, err := io.CopyBuffer(hash, reader, buffer)
		if err != nil {
			return nil, fmt.Errorf("hash backup part %d: %w", len(parts)+1, err)
		}
		if written != size {
			return nil, fmt.Errorf("backup archive truncated while hashing part %d", len(parts)+1)
		}
		parts = append(parts, localBackupPart{Index: len(parts) + 1, Offset: offset, SizeBytes: size, SHA256: hex.EncodeToString(hash.Sum(nil))})
		offset += size
	}
	after, err := src.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat hashed backup archive: %w", err)
	}
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, errors.New("backup archive changed while hashing")
	}
	return parts, nil
}

type backupContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *backupContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func cleanupBackupFiles(paths ...string) error {
	var errs []error
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}
