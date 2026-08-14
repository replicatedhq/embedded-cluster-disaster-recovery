package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type fakeS3Client struct {
	listObjectsV2 func(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	putObject     func(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	getObject     func(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

func (f *fakeS3Client) ListObjectsV2(ctx context.Context, input *s3.ListObjectsV2Input, options ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.listObjectsV2 == nil {
		panic("unexpected ListObjectsV2 call")
	}
	return f.listObjectsV2(ctx, input, options...)
}

func (f *fakeS3Client) PutObject(ctx context.Context, input *s3.PutObjectInput, options ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return f.putObject(ctx, input, options...)
}

func (f *fakeS3Client) DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	panic("unexpected DeleteObject call")
}

func (f *fakeS3Client) GetObject(ctx context.Context, input *s3.GetObjectInput, options ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if f.getObject == nil {
		panic("unexpected GetObject call")
	}
	return f.getObject(ctx, input, options...)
}

func TestListManifestsPreservesRecoveryPointsPrefixBoundary(t *testing.T) {
	manifest := Manifest{
		FormatVersion:    RecoveryFormatVersion,
		RecoveryPointID:  "6de67faf-207e-413f-af7f-190723f04d46",
		CreatedAt:        time.Date(2026, time.August, 14, 0, 37, 54, 0, time.UTC),
		VeleroBackupName: "ec-dr-6de67faf207e413faf7f190723f04d46",
		StateObjectKey:   "recovery-points/6de67faf-207e-413f-af7f-190723f04d46/ec-state-v1.tar.gz.age",
		Status:           "ready",
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	const manifestKey = "test-prefix/recovery-points/6de67faf-207e-413f-af7f-190723f04d46/manifest.json"
	client := &fakeS3Client{
		listObjectsV2: func(_ context.Context, input *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
			if got := aws.ToString(input.Prefix); got != "test-prefix/recovery-points/" {
				t.Fatalf("list prefix = %q, want recovery-points directory boundary", got)
			}
			return &s3.ListObjectsV2Output{Contents: []types.Object{{Key: aws.String(manifestKey)}}}, nil
		},
		getObject: func(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
			if got := aws.ToString(input.Key); got != manifestKey {
				t.Fatalf("manifest key = %q, want %q", got, manifestKey)
			}
			return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(manifestJSON))}, nil
		},
	}
	store := &ObjectStore{client: client, bucket: "bucket", prefix: "test-prefix"}

	manifests, err := store.ListManifests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 || manifests[0] != manifest {
		t.Fatalf("manifests = %#v, want %#v", manifests, []Manifest{manifest})
	}
}

func TestPutEncryptedFileUploadsSeekableBodyAndRemovesStagingFile(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "ec-state.tar.gz")
	source := []byte("embedded cluster state")
	if err := os.WriteFile(sourcePath, source, 0600); err != nil {
		t.Fatal(err)
	}

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	var encryptedBody []byte
	var stagingPath string
	client := &fakeS3Client{putObject: func(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
		file, ok := input.Body.(*os.File)
		if !ok {
			t.Fatal("S3 retries require a seekable file body")
		}
		stagingPath = file.Name()
		position, err := file.Seek(0, io.SeekCurrent)
		if err != nil {
			t.Fatal(err)
		}
		if position != 0 {
			t.Fatalf("upload body starts at byte %d, want 0", position)
		}
		if got := aws.ToString(input.Bucket); got != "bucket" {
			t.Fatalf("bucket = %q, want bucket", got)
		}
		if got := aws.ToString(input.Key); got != "prefix/recovery-points/test/ec-state.age" {
			t.Fatalf("key = %q, want prefixed recovery state key", got)
		}

		encryptedBody, err = io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := aws.ToInt64(input.ContentLength); got != int64(len(encryptedBody)) {
			t.Fatalf("content length = %d, want %d", got, len(encryptedBody))
		}
		return &s3.PutObjectOutput{}, nil
	}}
	store := &ObjectStore{
		client: client,
		bucket: "bucket",
		prefix: "prefix",
		newRecipient: func(string) (age.Recipient, error) {
			return identity.Recipient(), nil
		},
	}

	if err := store.PutEncryptedFile(context.Background(), "recovery-points/test/ec-state.age", sourcePath, "unused"); err != nil {
		t.Fatal(err)
	}
	if stagingPath == "" {
		t.Fatal("upload did not receive a staging file")
	}
	_, err = os.Stat(stagingPath)
	if !os.IsNotExist(err) {
		t.Fatalf("encrypted staging file was not removed: %v", err)
	}

	decrypted, err := age.Decrypt(bytes.NewReader(encryptedBody), identity)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := io.ReadAll(decrypted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, source) {
		t.Fatalf("restored bytes = %q, want %q", restored, source)
	}
}
