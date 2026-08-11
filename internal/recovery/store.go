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
	client *s3.Client
	bucket string
	prefix string
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
	return &ObjectStore{client: client, bucket: configuration.Bucket, prefix: strings.Trim(configuration.Prefix, "/")}, nil
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
	recipient, err := age.NewScryptRecipient(recoveryKey)
	if err != nil {
		return fmt.Errorf("initialize recovery encryption: %w", err)
	}
	reader, writer := io.Pipe()
	encryptionDone := make(chan error, 1)
	go func() {
		source, err := os.Open(sourcePath)
		if err != nil {
			_ = writer.CloseWithError(err)
			encryptionDone <- err
			return
		}
		defer source.Close()
		encrypted, err := age.Encrypt(writer, recipient)
		if err == nil {
			_, err = io.Copy(encrypted, source)
			if closeErr := encrypted.Close(); err == nil && closeErr != nil {
				err = closeErr
			}
		}
		_ = writer.CloseWithError(err)
		encryptionDone <- err
	}()
	_, uploadErr := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.objectKey(objectKey)), Body: reader, ContentType: aws.String("application/age")})
	_ = reader.CloseWithError(uploadErr)
	encryptionErr := <-encryptionDone
	if encryptionErr != nil {
		return fmt.Errorf("encrypt EC state archive: %w", encryptionErr)
	}
	if uploadErr != nil {
		return fmt.Errorf("upload encrypted EC state archive: %w", uploadErr)
	}
	return nil
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
	prefix := s.objectKey("recovery-points/")
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
