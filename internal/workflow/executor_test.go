package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/protocol"
	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/recovery"
)

const testOperationID = "e5bda050-2942-4c69-a485-a92c11b1621a"

type fakeStore struct {
	steps     []string
	manifest  recovery.Manifest
	manifests []recovery.Manifest
}

func (s *fakeStore) Test(context.Context) error { s.steps = append(s.steps, "test"); return nil }
func (s *fakeStore) PutEncryptedFile(_ context.Context, key, source, recoveryKey string) error {
	if key == "" || source == "" || recoveryKey == "" {
		panic("missing encrypted file input")
	}
	s.steps = append(s.steps, "state")
	return nil
}
func (s *fakeStore) GetDecryptedFile(_ context.Context, _ string, destination, _ string) (*protocol.ArtifactDescriptor, error) {
	s.steps = append(s.steps, "state")
	data := []byte("recovered-state")
	if err := os.WriteFile(destination, data, 0600); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	return &protocol.ArtifactDescriptor{Name: "ec-state-v1.tar.gz", Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}, nil
}
func (s *fakeStore) PutManifest(_ context.Context, manifest recovery.Manifest) error {
	s.steps = append(s.steps, "manifest")
	s.manifest = manifest
	return nil
}
func (s *fakeStore) GetManifest(context.Context, string) (recovery.Manifest, error) {
	s.steps = append(s.steps, "manifest")
	return s.manifest, nil
}
func (s *fakeStore) ListManifests(context.Context) ([]recovery.Manifest, error) {
	return s.manifests, nil
}
func (s *fakeStore) ListRetentionManifests(context.Context) ([]recovery.Manifest, error) {
	return s.manifests, nil
}
func (s *fakeStore) MarkRecoveryPointDeleting(context.Context, recovery.Manifest) error {
	s.steps = append(s.steps, "mark-deleting")
	return nil
}
func (s *fakeStore) DeleteRecoveryPoint(context.Context, recovery.Manifest) error {
	s.steps = append(s.steps, "delete-point")
	return nil
}

type fakeApplication struct {
	steps      *[]string
	backupName string
	restore    string
}

type fakeConfigurationStore struct {
	configuration recovery.Configuration
	schedule      string
	retention     int
	paused        bool
}

func (s *fakeConfigurationStore) LoadConfiguration(context.Context) (recovery.Configuration, error) {
	return s.configuration, nil
}
func (s *fakeConfigurationStore) SaveRestoreConfiguration(_ context.Context, configuration recovery.Configuration) error {
	s.configuration = configuration
	return nil
}
func (s *fakeConfigurationStore) BackupSettings(context.Context) (string, int, bool, error) {
	return s.schedule, s.retention, s.paused, nil
}

func (a *fakeApplication) Backup(_ context.Context, name string, _ protocol.BackupPolicy, _ func(protocol.Progress)) error {
	*a.steps = append(*a.steps, "application")
	a.backupName = name
	return nil
}
func (a *fakeApplication) Restore(_ context.Context, _ string, backupName string, _ func(protocol.Progress)) error {
	*a.steps = append(*a.steps, "application")
	a.restore = backupName
	return nil
}
func (a *fakeApplication) DeleteBackup(context.Context, string) error {
	*a.steps = append(*a.steps, "delete-backup")
	return nil
}

func TestBackupPublishesManifestLast(t *testing.T) {
	store := &fakeStore{}
	application := &fakeApplication{steps: &store.steps}
	executor, err := NewExecutor(func(context.Context, recovery.StorageConfiguration) (Store, error) { return store, nil }, application, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor.now = func() time.Time { return time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC) }
	executor.fromEnv = validEnvironment
	statePath := filepath.Join(t.TempDir(), "state.tar.gz")
	if err := os.WriteFile(statePath, []byte("state"), 0600); err != nil {
		t.Fatal(err)
	}
	result, _, err := executor.Execute(context.Background(), protocol.OperationRequest{
		APIVersion: protocol.APIVersion, OperationID: testOperationID, Operation: protocol.OperationBackup,
		Phase: protocol.PhaseInCluster, BackupPolicy: &protocol.BackupPolicy{IncludedNamespaces: []string{"app"}, VolumeBackup: "fileSystem"},
	}, statePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.RecoveryPointID != testOperationID || store.manifest.RecoveryPointID != testOperationID {
		t.Fatalf("unexpected recovery point: %#v %#v", result, store.manifest)
	}
	if !reflect.DeepEqual(store.steps, []string{"test", "application", "state", "manifest"}) {
		t.Fatalf("steps = %v", store.steps)
	}
}

func TestBootstrapRestoreReturnsRecoveredArchive(t *testing.T) {
	store := &fakeStore{manifest: validManifest()}
	executor, err := NewExecutor(func(context.Context, recovery.StorageConfiguration) (Store, error) { return store, nil }, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, download, err := executor.Execute(context.Background(), protocol.OperationRequest{
		APIVersion: protocol.APIVersion, OperationID: testOperationID, Operation: protocol.OperationRestore,
		Phase: protocol.PhaseBootstrap, RecoveryPointID: testOperationID, Headless: true, Configuration: validConfigurationJSON(),
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.StateArchive == nil || result.StateArchive.Size == 0 {
		t.Fatalf("missing recovered archive descriptor: %#v", result)
	}
	if info, err := os.Stat(download); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("recovered archive is not mode 0600: %v, %v", info, err)
	}
}

func TestApplicationRestoreUsesManifestBackup(t *testing.T) {
	store := &fakeStore{manifest: validManifest()}
	application := &fakeApplication{steps: &store.steps}
	executor, err := NewExecutor(func(context.Context, recovery.StorageConfiguration) (Store, error) { return store, nil }, application, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor.fromEnv = validEnvironment
	result, _, err := executor.Execute(context.Background(), protocol.OperationRequest{
		APIVersion: protocol.APIVersion, OperationID: testOperationID, Operation: protocol.OperationRestore,
		Phase: protocol.PhaseInCluster, RecoveryPointID: testOperationID,
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.RecoveryPointID != testOperationID || application.restore != store.manifest.VeleroBackupName {
		t.Fatalf("unexpected restore result: %#v, backup %q", result, application.restore)
	}
}

func TestBackupScheduleUsesLatestCompleteRecoveryPoint(t *testing.T) {
	store := &fakeStore{manifests: []recovery.Manifest{validManifest()}}
	executor, err := NewExecutor(func(context.Context, recovery.StorageConfiguration) (Store, error) { return store, nil }, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor.SetConfigurationStore(&fakeConfigurationStore{configuration: mustEnvironment(), schedule: "0 * * * *", retention: 30})
	executor.now = func() time.Time { return time.Date(2026, 8, 11, 13, 0, 0, 0, time.UTC) }
	status, err := executor.BackupSchedule(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Due || status.OperationID == "" || status.NextAt == nil {
		t.Fatalf("unexpected schedule status: %#v", status)
	}
}

func TestBackupScheduleMakesInitialBackupDueWithStableID(t *testing.T) {
	store := &fakeStore{}
	executor, err := NewExecutor(func(context.Context, recovery.StorageConfiguration) (Store, error) { return store, nil }, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor.SetConfigurationStore(&fakeConfigurationStore{configuration: mustEnvironment(), schedule: "0 * * * *", retention: 30})
	first, err := executor.BackupSchedule(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := executor.BackupSchedule(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Due || first.OperationID == "" || first.OperationID != second.OperationID {
		t.Fatalf("initial schedule is not stable and due: %#v, %#v", first, second)
	}
}

func TestRetentionHidesAndDeletesOldestBeforePublishing(t *testing.T) {
	newer := validManifest()
	newer.RecoveryPointID = "d516f24f-3b7d-4baa-b4cc-128aff924994"
	newer.StateObjectKey = "recovery-points/" + newer.RecoveryPointID + "/ec-state-v1.tar.gz.age"
	newer.CreatedAt = newer.CreatedAt.Add(time.Hour)
	store := &fakeStore{manifests: []recovery.Manifest{newer, validManifest()}}
	application := &fakeApplication{steps: &store.steps}
	executor, err := NewExecutor(func(context.Context, recovery.StorageConfiguration) (Store, error) { return store, nil }, application, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor.SetConfigurationStore(&fakeConfigurationStore{configuration: mustEnvironment(), retention: 2})
	statePath := filepath.Join(t.TempDir(), "state.tar.gz")
	if err := os.WriteFile(statePath, []byte("state"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executor.Execute(context.Background(), protocol.OperationRequest{
		APIVersion: protocol.APIVersion, OperationID: testOperationID, Operation: protocol.OperationBackup,
		Phase: protocol.PhaseInCluster, BackupPolicy: &protocol.BackupPolicy{IncludedNamespaces: []string{"app"}, VolumeBackup: "fileSystem"},
	}, statePath, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"test", "application", "state", "mark-deleting", "delete-backup", "delete-point", "manifest"}
	if !reflect.DeepEqual(store.steps, want) {
		t.Fatalf("steps = %v, want %v", store.steps, want)
	}
}

func validEnvironment() (recovery.Configuration, error) {
	return mustEnvironment(), nil
}

func mustEnvironment() recovery.Configuration {
	return recovery.Configuration{
		Storage:     recovery.StorageConfiguration{Bucket: "backup", AccessKeyID: "access", SecretAccessKey: "secret"},
		RecoveryKey: "0123456789abcdef",
	}
}

func validConfigurationJSON() json.RawMessage {
	return json.RawMessage(`{"storage":{"bucket":"backup","accessKeyId":"access","secretAccessKey":"secret"},"recoveryKey":"0123456789abcdef"}`)
}

func validManifest() recovery.Manifest {
	return recovery.Manifest{
		FormatVersion: recovery.RecoveryFormatVersion, RecoveryPointID: testOperationID,
		CreatedAt:        time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
		VeleroBackupName: "ec-dr-e5bda05029424c69a485a92c11b1621a",
		StateObjectKey:   "recovery-points/" + testOperationID + "/ec-state-v1.tar.gz.age",
		Status:           "ready",
	}
}
