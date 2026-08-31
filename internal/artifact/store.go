package artifact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var ErrDisabled = errors.New("artifact storage is not configured")

type Config struct {
	Endpoint, AccessKey, SecretKey, Bucket, Region string
	UseSSL                                         bool
}

type Item struct {
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
}

// Staged identifies an upload that is not visible through the artifact API.
// Its fields are intentionally private so only an artifact Store can create a
// promotable handle.
type Staged struct {
	sourceKey      string
	destinationKey string
}

type Store interface {
	Enabled() bool
	Stage(context.Context, string, string, io.Reader, int64) (Staged, error)
	Commit(context.Context, Staged) error
	Discard(context.Context, Staged) error
	List(context.Context, string) ([]Item, error)
	Open(context.Context, string, string) (io.ReadCloser, Item, error)
	Delete(context.Context, string) error
}

type disabledStore struct{}

func Disabled() Store               { return disabledStore{} }
func (disabledStore) Enabled() bool { return false }
func (disabledStore) Stage(context.Context, string, string, io.Reader, int64) (Staged, error) {
	return Staged{}, ErrDisabled
}
func (disabledStore) Commit(context.Context, Staged) error         { return ErrDisabled }
func (disabledStore) Discard(context.Context, Staged) error        { return nil }
func (disabledStore) List(context.Context, string) ([]Item, error) { return nil, nil }
func (disabledStore) Open(context.Context, string, string) (io.ReadCloser, Item, error) {
	return nil, Item{}, ErrDisabled
}
func (disabledStore) Delete(context.Context, string) error { return nil }

type minioStore struct {
	client *minio.Client
	bucket string
}

func Open(cfg Config) (Store, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return Disabled(), nil
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("S3 access key and secret key are required")
	}
	if cfg.Bucket == "" {
		cfg.Bucket = "gpuflow-artifacts"
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("check artifact bucket: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("create artifact bucket: %w", err)
		}
	}
	return &minioStore{client: client, bucket: cfg.Bucket}, nil
}

func (s *minioStore) Enabled() bool { return true }

func objectName(jobID, name string) (string, error) {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "" || name == ".." {
		return "", errors.New("invalid artifact name")
	}
	return jobID + "/" + name, nil
}

const stagingDirectory = ".gpuflow-staging"

func stagedObjectName(jobID, name string) (Staged, error) {
	destinationKey, err := objectName(jobID, name)
	if err != nil {
		return Staged{}, err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return Staged{}, fmt.Errorf("create artifact staging key: %w", err)
	}
	return Staged{
		sourceKey:      jobID + "/" + stagingDirectory + "/" + hex.EncodeToString(random),
		destinationKey: destinationKey,
	}, nil
}

func (s *minioStore) Stage(ctx context.Context, jobID, name string, r io.Reader, size int64) (Staged, error) {
	staged, err := stagedObjectName(jobID, name)
	if err != nil {
		return Staged{}, err
	}
	_, err = s.client.PutObject(ctx, s.bucket, staged.sourceKey, r, size, minio.PutObjectOptions{ContentType: "application/gzip"})
	return staged, err
}

// Commit promotes a fully uploaded staging object with a single S3 CopyObject
// operation. S3-compatible stores replace the destination object atomically,
// so a failed or fenced upload never partially overwrites the prior artifact.
func (s *minioStore) Commit(ctx context.Context, staged Staged) error {
	if staged.sourceKey == "" || staged.destinationKey == "" {
		return errors.New("invalid staged artifact")
	}
	_, err := s.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: s.bucket, Object: staged.destinationKey},
		minio.CopySrcOptions{Bucket: s.bucket, Object: staged.sourceKey},
	)
	return err
}

func (s *minioStore) Discard(ctx context.Context, staged Staged) error {
	if staged.sourceKey == "" {
		return nil
	}
	return s.client.RemoveObject(ctx, s.bucket, staged.sourceKey, minio.RemoveObjectOptions{})
}

func (s *minioStore) List(ctx context.Context, jobID string) ([]Item, error) {
	items := []Item{}
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: jobID + "/", Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		name := strings.TrimPrefix(obj.Key, jobID+"/")
		if strings.HasPrefix(name, stagingDirectory+"/") {
			continue
		}
		items = append(items, Item{Name: name, Size: obj.Size, LastModified: obj.LastModified})
	}
	return items, nil
}

func (s *minioStore) Open(ctx context.Context, jobID, name string) (io.ReadCloser, Item, error) {
	key, err := objectName(jobID, name)
	if err != nil {
		return nil, Item{}, err
	}
	stat, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, Item{}, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, Item{}, err
	}
	return obj, Item{Name: name, Size: stat.Size, LastModified: stat.LastModified}, nil
}

func (s *minioStore) Delete(ctx context.Context, jobID string) error {
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: jobID + "/", Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if err := s.client.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}
	return nil
}
