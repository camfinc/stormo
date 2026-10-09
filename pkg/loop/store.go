// Package loop is the runtime half of the nap/dream loop: the nap store, snapshots, naps,
// rehydrate, handoff sync, the dream and human review.
package loop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/camfinc/stormo/pkg/manifest"
)

// Store holds naps. Layout (identical on S3 and on disk):
//
//	<agent>/blobs/<sha256>     content-addressed file bodies (dedup across naps)
//	<agent>/naps/<napId>.json  nap manifest
//	<agent>/latest.json        {"nap": <napId>} pointer used by rehydrate
//
// Raw-class blobs (session DBs) live under <agent>/raw/<sha256> instead of blobs/ so a bucket
// policy can deny the dream/reviewer role on that prefix outright. A handoff copies older naps
// without their raw blobs, so a store may hold manifests whose raw blobs are absent by design;
// only the latest nap is ever rehydrated.
type Store interface {
	Put(key string, body []byte) error
	// Get returns nil, nil when the key does not exist.
	Get(key string) ([]byte, error)
	Exists(key string) (bool, error)
	// List returns every key under prefix, sorted.
	List(prefix string) ([]string, error)
}

// FsStore is a directory store (local runs, tests).
type FsStore struct{ Root string }

func (s FsStore) Put(key string, body []byte) error {
	p := filepath.Join(s.Root, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, body, 0o644)
}

func (s FsStore) Get(key string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(s.Root, key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func (s FsStore) Exists(key string) (bool, error) {
	_, err := os.Stat(filepath.Join(s.Root, key))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (s FsStore) List(prefix string) ([]string, error) {
	base := filepath.Join(s.Root, prefix)
	out := []string{}
	if _, err := os.Stat(base); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(s.Root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// LocalStore is an instance's local nap store: agent <id>'s keys live in its own folder,
// agents/<id>/data/store (the same layout without the <id>/ prefix), or in format 0's
// .swarm/store/<id> until MigrateLocalStore moves them.
type LocalStore struct{ Root string }

// LocalStoreDir is where agent id's local naps are now.
func LocalStoreDir(root, id string) string {
	dir := filepath.Join(manifest.DataDir(root, id), "store")
	legacy := filepath.Join(root, ".swarm", "store", id)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	return dir
}

// MigrateLocalStore moves agent id's naps from .swarm/store/<id> into its folder (the agent must
// be stopped); true when it moved them.
func MigrateLocalStore(root, id string) (bool, error) {
	legacy := filepath.Join(root, ".swarm", "store", id)
	if _, err := os.Stat(legacy); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	dir := filepath.Join(manifest.DataDir(root, id), "store")
	if _, err := os.Stat(dir); err == nil {
		return false, fmt.Errorf("both %s and %s exist; keep one", legacy, dir)
	}
	if _, err := manifest.EnsureDataDir(root, id); err != nil {
		return false, err
	}
	return true, os.Rename(legacy, dir)
}

func (s LocalStore) split(key string) (FsStore, string) {
	agent, rest, _ := strings.Cut(key, "/")
	return FsStore{Root: LocalStoreDir(s.Root, agent)}, rest
}

func (s LocalStore) Put(key string, body []byte) error {
	agent, rest, _ := strings.Cut(key, "/")
	dir := LocalStoreDir(s.Root, agent)
	if dir == filepath.Join(manifest.DataDir(s.Root, agent), "store") {
		if _, err := manifest.EnsureDataDir(s.Root, agent); err != nil {
			return err
		}
	}
	return FsStore{Root: dir}.Put(rest, body)
}

func (s LocalStore) Get(key string) ([]byte, error) {
	st, rest := s.split(key)
	return st.Get(rest)
}

func (s LocalStore) Exists(key string) (bool, error) {
	st, rest := s.split(key)
	return st.Exists(rest)
}

// List takes a prefix that starts with an agent id.
func (s LocalStore) List(prefix string) ([]string, error) {
	agent, rest, _ := strings.Cut(prefix, "/")
	keys, err := FsStore{Root: LocalStoreDir(s.Root, agent)}.List(rest)
	for i, k := range keys {
		keys[i] = agent + "/" + k
	}
	return keys, err
}

// S3Store is a bucket. Credentials come from the default chain (the ECS task role in the sidecar,
// the shell's profile on a laptop), never from the manifest.
type S3Store struct {
	Bucket string
	client *s3.Client
}

// NewS3Store opens a bucket; region "" uses AWS_REGION / the profile's region.
func NewS3Store(bucket, region string) (*S3Store, error) {
	opts := []func(*config.LoadOptions) error{}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, err
	}
	return &S3Store{Bucket: bucket, client: s3.NewFromConfig(cfg)}, nil
}

func (s *S3Store) Put(key string, body []byte) error {
	_, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{Bucket: &s.Bucket, Key: &key, Body: bytes.NewReader(body)})
	return err
}

func (s *S3Store) Get(key string) ([]byte, error) {
	out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{Bucket: &s.Bucket, Key: &key})
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (s *S3Store) Exists(key string) (bool, error) {
	_, err := s.client.HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: &s.Bucket, Key: &key})
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return false, nil
	}
	return err == nil, err
}

func (s *S3Store) List(prefix string) ([]string, error) {
	keys := []string{}
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.Bucket, Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(context.Background())
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// Open maps `s3://bucket` to an S3Store and anything else to an FsStore rooted at that path.
func Open(uri, region string) (Store, error) {
	if b, ok := strings.CutPrefix(uri, "s3://"); ok {
		return NewS3Store(strings.TrimSuffix(b, "/"), region)
	}
	return FsStore{Root: uri}, nil
}
