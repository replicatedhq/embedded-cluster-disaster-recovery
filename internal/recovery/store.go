package recovery

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/protocol"
)

const RecoveryFormatVersion = "1"

var recoveryPointPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Manifest struct {
	FormatVersion    string    `json:"formatVersion"`
	RecoveryPointID  string    `json:"recoveryPointId"`
	CreatedAt        time.Time `json:"createdAt"`
	VeleroBackupName string    `json:"veleroBackupName"`
	StateObjectKey   string    `json:"stateObjectKey"`
	Status           string    `json:"status"`
}

type ObjectStore struct {
	client       s3Client
	bucket       string
	prefix       string
	newRecipient func(string) (age.Recipient, error)
}

type s3Client interface {
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

func NewObjectStore(ctx context.Context, configuration StorageConfiguration) (*ObjectStore, error) {
	region := configuration.Region
	if region == "" {
		region = "auto"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if configuration.CustomCAPEM != "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system certificate pool: %w", err)
		}
		if !roots.AppendCertsFromPEM([]byte(configuration.CustomCAPEM)) {
			return nil, fmt.Errorf("storage custom CA does not contain a certificate")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	if configuration.ProxyURL != "" {
		proxy, err := url.Parse(configuration.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse storage proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(proxy)
	}
	awsConfiguration, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(configuration.AccessKeyID, configuration.SecretAccessKey, "")),
		awsconfig.WithHTTPClient(&http.Client{Transport: transport, Timeout: 5 * time.Minute}),
	)
	if err != nil {
		return nil, fmt.Errorf("initialize S3 client: %w", err)
	}
	client := s3.NewFromConfig(awsConfiguration, func(options *s3.Options) {
		options.UsePathStyle = configuration.ForcePathStyle
		if configuration.Endpoint != "" {
			options.BaseEndpoint = aws.String(strings.TrimRight(configuration.Endpoint, "/"))
		}
	})
	return &ObjectStore{
		client: client,
		bucket: configuration.Bucket,
		prefix: strings.Trim(configuration.Prefix, "/"),
		newRecipient: func(recoveryKey string) (age.Recipient, error) {
			return age.NewScryptRecipient(recoveryKey)
		},
	}, nil
}

func (s *ObjectStore) Test(ctx context.Context) error {
	_, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), MaxKeys: aws.Int32(1), Prefix: aws.String(s.objectKey("recovery-points/"))})
	if err != nil {
		return fmt.Errorf("list recovery bucket: %w", err)
	}
	return nil
}

func (s *ObjectStore) TestWritable(ctx context.Context) error {
	key := s.objectKey(".checks/" + uuid.NewString())
	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), Body: strings.NewReader("embedded-cluster-dr"),
	}); err != nil {
		return fmt.Errorf("write recovery bucket: %w", err)
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}); err != nil {
		return fmt.Errorf("remove recovery bucket write check: %w", err)
	}
	return nil
}

func (s *ObjectStore) PutEncryptedFile(ctx context.Context, objectKey, sourcePath, recoveryKey string) error {
	recipient, err := s.newRecipient(recoveryKey)
	if err != nil {
		return fmt.Errorf("initialize recovery encryption: %w", err)
	}
	encrypted, size, err := stageEncryptedFile(sourcePath, recipient)
	if err != nil {
		return fmt.Errorf("encrypt EC state archive: %w", err)
	}
	encryptedPath := encrypted.Name()
	defer func() {
		_ = encrypted.Close()
		_ = os.Remove(encryptedPath)
	}()
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(s.objectKey(objectKey)), Body: encrypted,
		ContentLength: aws.Int64(size), ContentType: aws.String("application/age"),
	})
	if err != nil {
		return fmt.Errorf("upload encrypted EC state archive: %w", err)
	}
	return nil
}

func stageEncryptedFile(sourcePath string, recipient age.Recipient) (_ *os.File, size int64, returnedErr error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return nil, 0, err
	}
	defer source.Close()

	encrypted, err := os.CreateTemp(filepath.Dir(sourcePath), ".ec-state-*.age")
	if err != nil {
		return nil, 0, err
	}
	encryptedPath := encrypted.Name()
	defer func() {
		if returnedErr != nil {
			_ = encrypted.Close()
			_ = os.Remove(encryptedPath)
		}
	}()
	if err := encrypted.Chmod(0600); err != nil {
		return nil, 0, err
	}

	writer, err := age.Encrypt(encrypted, recipient)
	if err != nil {
		return nil, 0, err
	}
	if _, err := io.Copy(writer, source); err != nil {
		_ = writer.Close()
		return nil, 0, err
	}
	if err := writer.Close(); err != nil {
		return nil, 0, err
	}
	position, err := encrypted.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, 0, err
	}
	if _, err := encrypted.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	return encrypted, position, nil
}

func (s *ObjectStore) GetDecryptedFile(ctx context.Context, objectKey, destination, recoveryKey string) (*protocol.ArtifactDescriptor, error) {
	response, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.objectKey(objectKey))})
	if err != nil {
		return nil, fmt.Errorf("download encrypted EC state archive: %w", err)
	}
	defer response.Body.Close()
	identity, err := age.NewScryptIdentity(recoveryKey)
	if err != nil {
		return nil, fmt.Errorf("initialize recovery decryption: %w", err)
	}
	decrypted, err := age.Decrypt(response.Body, identity)
	if err != nil {
		return nil, fmt.Errorf("decrypt EC state archive: %w", err)
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("create recovered EC state archive: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(destination)
		}
	}()
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), decrypted)
	closeErr := file.Close()
	if copyErr != nil {
		return nil, fmt.Errorf("write recovered EC state archive: %w", copyErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close recovered EC state archive: %w", closeErr)
	}
	committed = true
	return &protocol.ArtifactDescriptor{Name: "ec-state-v1.tar.gz", Size: written, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func (s *ObjectStore) PutManifest(ctx context.Context, manifest Manifest) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal recovery manifest: %w", err)
	}
	key := path.Join("recovery-points", manifest.RecoveryPointID, "manifest.json")
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.objectKey(key)), Body: strings.NewReader(string(data)), ContentType: aws.String("application/json")})
	if err != nil {
		return fmt.Errorf("publish recovery manifest: %w", err)
	}
	return nil
}

func (s *ObjectStore) GetManifest(ctx context.Context, recoveryPointID string) (Manifest, error) {
	manifest, err := s.getManifest(ctx, recoveryPointID)
	if err != nil {
		return Manifest{}, err
	}
	if manifest.Status != "ready" {
		return Manifest{}, fmt.Errorf("recovery point is not ready")
	}
	return manifest, nil
}

func (s *ObjectStore) getManifest(ctx context.Context, recoveryPointID string) (Manifest, error) {
	if !recoveryPointPattern.MatchString(recoveryPointID) {
		return Manifest{}, fmt.Errorf("recovery point ID is invalid")
	}
	key := path.Join("recovery-points", recoveryPointID, "manifest.json")
	response, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.objectKey(key))})
	if err != nil {
		return Manifest{}, fmt.Errorf("get recovery manifest: %w", err)
	}
	defer response.Body.Close()
	var manifest Manifest
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode recovery manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil || manifest.RecoveryPointID != recoveryPointID {
		return Manifest{}, fmt.Errorf("recovery manifest is incomplete or unsupported")
	}
	return manifest, nil
}

func (s *ObjectStore) ListManifests(ctx context.Context) ([]Manifest, error) {
	manifests, err := s.listManifests(ctx)
	if err != nil {
		return nil, err
	}
	ready := manifests[:0]
	for _, manifest := range manifests {
		if manifest.Status == "ready" {
			ready = append(ready, manifest)
		}
	}
	return ready, nil
}

func (s *ObjectStore) ListRetentionManifests(ctx context.Context) ([]Manifest, error) {
	return s.listManifests(ctx)
}

func (s *ObjectStore) listManifests(ctx context.Context) ([]Manifest, error) {
	prefix := strings.TrimRight(s.objectKey("recovery-points"), "/") + "/"
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix)})
	var manifests []Manifest
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list recovery manifests: %w", err)
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			if !strings.HasSuffix(key, "/manifest.json") {
				continue
			}
			relative := strings.TrimPrefix(key, prefix)
			id := strings.TrimSuffix(relative, "/manifest.json")
			if id == "" || strings.Contains(id, "/") {
				continue
			}
			manifest, err := s.getManifest(ctx, id)
			if err == nil {
				manifests = append(manifests, manifest)
			}
		}
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].CreatedAt.After(manifests[j].CreatedAt) })
	return manifests, nil
}

func (s *ObjectStore) MarkRecoveryPointDeleting(ctx context.Context, manifest Manifest) error {
	manifest.Status = "deleting"
	return s.PutManifest(ctx, manifest)
}

func (s *ObjectStore) DeleteRecoveryPoint(ctx context.Context, manifest Manifest) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.objectKey(manifest.StateObjectKey))}); err != nil {
		return fmt.Errorf("delete expired EC state archive: %w", err)
	}
	manifestKey := path.Join("recovery-points", manifest.RecoveryPointID, "manifest.json")
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.objectKey(manifestKey))}); err != nil {
		return fmt.Errorf("delete expired recovery manifest: %w", err)
	}
	return nil
}

func (s *ObjectStore) objectKey(key string) string {
	if s.prefix == "" {
		return strings.TrimLeft(key, "/")
	}
	return path.Join(s.prefix, key)
}

func (m Manifest) Validate() error {
	if m.FormatVersion != RecoveryFormatVersion || !recoveryPointPattern.MatchString(m.RecoveryPointID) || m.VeleroBackupName == "" || m.CreatedAt.IsZero() || (m.Status != "ready" && m.Status != "deleting") {
		return fmt.Errorf("recovery manifest is incomplete or unsupported")
	}
	expectedStateKey := path.Join("recovery-points", m.RecoveryPointID, "ec-state-v1.tar.gz.age")
	if m.StateObjectKey != expectedStateKey {
		return fmt.Errorf("recovery manifest state object is invalid")
	}
	return nil
}
