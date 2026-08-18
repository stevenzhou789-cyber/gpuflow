package artifact

import (
	"context"
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

type Store interface {
	Enabled() bool
	Put(context.Context, string, string, io.Reader, int64) error
	List(context.Context, string) ([]Item, error)
	Open(context.Context, string, string) (io.ReadCloser, Item, error)
	Delete(context.Context, string) error
}

type disabledStore struct{}

func Disabled() Store                                                             { return disabledStore{} }
func (disabledStore) Enabled() bool                                               { return false }
func (disabledStore) Put(context.Context, string, string, io.Reader, int64) error { return ErrDisabled }
func (disabledStore) List(context.Context, string) ([]Item, error)                { return nil, nil }
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

func (s *minioStore) Put(ctx context.Context, jobID, name string, r io.Reader, size int64) error {
	key, err := objectName(jobID, name)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: "application/gzip"})
	return err
}

func (s *minioStore) List(ctx context.Context, jobID string) ([]Item, error) {
	items := []Item{}
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: jobID + "/", Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		items = append(items, Item{Name: strings.TrimPrefix(obj.Key, jobID+"/"), Size: obj.Size, LastModified: obj.LastModified})
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
