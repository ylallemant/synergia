package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/rs/zerolog/log"
)

// ModelInfo contains metadata about an available model file.
type ModelInfo struct {
	Name         string `json:"name"`
	Quantisation string `json:"quantisation"`
	Filename     string `json:"filename"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256,omitempty"`
}

// Store is the interface for model storage backends.
type Store interface {
	// List returns all available model files.
	List(ctx context.Context) ([]ModelInfo, error)

	// ServeDownload streams the model file to the HTTP response.
	// Supports Range requests for resumable downloads.
	ServeDownload(ctx context.Context, filename string, w http.ResponseWriter, r *http.Request) error

	// FileHash computes and returns the SHA256 hex hash of the specified model file.
	FileHash(ctx context.Context, filename string) (string, error)
}

// NewFilesystemStore creates a store backed by a local directory.
func NewFilesystemStore(path string) (*FilesystemStore, error) {
	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, fmt.Errorf("create model directory: %w", err)
	}
	log.Info().Str("backend", "filesystem").Str("path", path).Msg("model store initialized")
	return &FilesystemStore{path: path}, nil
}

// FilesystemStore serves model files from a local directory.
type FilesystemStore struct {
	path string
}

func (s *FilesystemStore) List(ctx context.Context) ([]ModelInfo, error) {
	entries, err := os.ReadDir(s.path)
	if err != nil {
		return nil, fmt.Errorf("read model directory: %w", err)
	}

	var models []ModelInfo
	for _, entry := range entries {
		if entry.IsDir() || !isModelFile(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		name, quant := parseModelFilename(entry.Name())
		models = append(models, ModelInfo{
			Name:         name,
			Quantisation: quant,
			Filename:     entry.Name(),
			Size:         info.Size(),
		})
	}
	return models, nil
}

func (s *FilesystemStore) ServeDownload(ctx context.Context, filename string, w http.ResponseWriter, r *http.Request) error {
	// Sanitize filename to prevent path traversal
	filename = filepath.Base(filename)
	filePath := filepath.Join(s.path, filename)

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return fmt.Errorf("model not found: %s", filename)
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	http.ServeFile(w, r, filePath)
	return nil
}

func (s *FilesystemStore) FileHash(ctx context.Context, filename string) (string, error) {
	filename = filepath.Base(filename)
	filePath := filepath.Join(s.path, filename)

	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open model file %s: %w", filename, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash model file %s: %w", filename, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// NewS3Store creates a store backed by S3-compatible object storage.
func NewS3Store(endpoint, bucket, region, accessKey, secretKey string, useSSL bool) (*S3Store, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
		Region: region,
	})
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}

	log.Info().Str("backend", "s3").Str("endpoint", endpoint).Str("bucket", bucket).Msg("model store initialized")
	return &S3Store{client: client, bucket: bucket}, nil
}

// S3Store serves model files from S3-compatible storage (MinIO, AWS S3, Scaleway Object Storage, etc.).
type S3Store struct {
	client *minio.Client
	bucket string
}

func (s *S3Store) List(ctx context.Context) ([]ModelInfo, error) {
	var models []ModelInfo

	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list objects: %w", obj.Err)
		}
		if !isModelFile(obj.Key) {
			continue
		}
		name, quant := parseModelFilename(obj.Key)
		models = append(models, ModelInfo{
			Name:         name,
			Quantisation: quant,
			Filename:     obj.Key,
			Size:         obj.Size,
		})
	}
	return models, nil
}

func (s *S3Store) ServeDownload(ctx context.Context, filename string, w http.ResponseWriter, r *http.Request) error {
	// Sanitize — only the filename, no path separators
	filename = filepath.Base(filename)

	// Get object info first
	info, err := s.client.StatObject(ctx, s.bucket, filename, minio.StatObjectOptions{})
	if err != nil {
		return fmt.Errorf("model not found: %s", filename)
	}

	// Support Range header for resumable downloads
	var opts minio.GetObjectOptions
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		opts.Set("Range", rangeHeader)
	}

	obj, err := s.client.GetObject(ctx, s.bucket, filename, opts)
	if err != nil {
		return fmt.Errorf("get object: %w", err)
	}
	defer obj.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	w.Header().Set("Accept-Ranges", "bytes")

	if rangeHeader != "" {
		w.WriteHeader(http.StatusPartialContent)
	}

	if _, err := io.Copy(w, obj); err != nil {
		log.Warn().Str("filename", filename).Err(err).Msg("model download interrupted")
	}
	return nil
}

func (s *S3Store) FileHash(ctx context.Context, filename string) (string, error) {
	filename = filepath.Base(filename)

	obj, err := s.client.GetObject(ctx, s.bucket, filename, minio.GetObjectOptions{})
	if err != nil {
		return "", fmt.Errorf("get object %s: %w", filename, err)
	}
	defer obj.Close()

	h := sha256.New()
	if _, err := io.Copy(h, obj); err != nil {
		return "", fmt.Errorf("hash model file %s: %w", filename, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// isModelFile checks if the filename looks like a GGUF model file.
func isModelFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".gguf") || strings.HasSuffix(lower, ".bin")
}

// parseModelFilename extracts model name and quantisation from a GGUF filename.
// Example: "mistral-small-3.2-24b-instruct-2506-Q4_K_M.gguf" → ("mistral-small-3.2-24b-instruct-2506", "Q4_K_M")
func parseModelFilename(filename string) (name, quantisation string) {
	// Strip extension
	base := strings.TrimSuffix(filename, filepath.Ext(filename))

	// Look for common quantisation suffixes
	quantPatterns := []string{
		"Q2_K", "Q3_K_S", "Q3_K_M", "Q3_K_L",
		"Q4_0", "Q4_1", "Q4_K_S", "Q4_K_M",
		"Q5_0", "Q5_1", "Q5_K_S", "Q5_K_M",
		"Q6_K", "Q8_0", "F16", "F32",
	}

	for _, q := range quantPatterns {
		if strings.HasSuffix(base, "-"+q) || strings.HasSuffix(base, "_"+q) {
			name = strings.TrimSuffix(base, "-"+q)
			name = strings.TrimSuffix(name, "_"+q)
			return name, q
		}
	}

	return base, "unknown"
}
