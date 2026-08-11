package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/protocol"
	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/recovery"
	"github.com/robfig/cron/v3"
)

type Store interface {
	Test(context.Context) error
	PutEncryptedFile(context.Context, string, string, string) error
	GetDecryptedFile(context.Context, string, string, string) (*protocol.ArtifactDescriptor, error)
	PutManifest(context.Context, recovery.Manifest) error
	GetManifest(context.Context, string) (recovery.Manifest, error)
	ListManifests(context.Context) ([]recovery.Manifest, error)
	ListRetentionManifests(context.Context) ([]recovery.Manifest, error)
	MarkRecoveryPointDeleting(context.Context, recovery.Manifest) error
	DeleteRecoveryPoint(context.Context, recovery.Manifest) error
}

type StoreFactory func(context.Context, recovery.StorageConfiguration) (Store, error)

type ApplicationBackup interface {
	Backup(context.Context, string, protocol.BackupPolicy, func(protocol.Progress)) error
	Restore(context.Context, string, string, func(protocol.Progress)) error
	DeleteBackup(context.Context, string) error
}

type ConfigurationStore interface {
	LoadConfiguration(context.Context) (recovery.Configuration, error)
	SaveRestoreConfiguration(context.Context, recovery.Configuration) error
	BackupSettings(context.Context) (string, int, bool, error)
}

type Executor struct {
	storeFactory StoreFactory
	application  ApplicationBackup
	tempRoot     string
	now          func() time.Time
	fromEnv      func() (recovery.Configuration, error)
	settings     ConfigurationStore
}

func NewExecutor(storeFactory StoreFactory, application ApplicationBackup, tempRoot string) (*Executor, error) {
	if storeFactory == nil {
		return nil, fmt.Errorf("store factory is required")
	}
	if tempRoot == "" {
		return nil, fmt.Errorf("temporary root is required")
	}
	if err := os.MkdirAll(tempRoot, 0700); err != nil {
		return nil, fmt.Errorf("create workflow temporary root: %w", err)
	}
	if err := os.Chmod(tempRoot, 0700); err != nil {
		return nil, fmt.Errorf("secure workflow temporary root: %w", err)
	}
	return &Executor{
		storeFactory: storeFactory,
		application:  application,
		tempRoot:     tempRoot,
		now:          time.Now,
		fromEnv:      recovery.ConfigurationFromEnvironment,
	}, nil
}

func (e *Executor) SetConfigurationStore(store ConfigurationStore) {
	e.settings = store
}

func (e *Executor) Execute(ctx context.Context, request protocol.OperationRequest, statePath string, progress func(protocol.Progress)) (*protocol.OperationResult, string, error) {
	if progress == nil {
		progress = func(protocol.Progress) {}
	}
	configuration, err := e.configuration(ctx, request)
	if err != nil {
		return nil, "", err
	}
	store, err := e.storeFactory(ctx, configuration.Storage)
	if err != nil {
		return nil, "", fmt.Errorf("open recovery storage: %w", err)
	}
	if err := store.Test(ctx); err != nil {
		return nil, "", err
	}

	switch {
	case request.Operation == protocol.OperationBackup && request.Phase == protocol.PhaseInCluster:
		return e.backup(ctx, store, configuration, request, statePath, progress)
	case request.Operation == protocol.OperationRestore && request.Phase == protocol.PhaseBootstrap:
		return e.bootstrapRestore(ctx, store, configuration, request, progress)
	case request.Operation == protocol.OperationRestore && request.Phase == protocol.PhaseInCluster:
		return e.applicationRestore(ctx, store, request, progress)
	default:
		return nil, "", fmt.Errorf("unsupported lifecycle operation %s/%s", request.Operation, request.Phase)
	}
}

func (e *Executor) ListRecoveryPoints(ctx context.Context, data json.RawMessage) ([]protocol.RecoveryPoint, error) {
	var configuration recovery.Configuration
	var err error
	if len(data) == 0 && e.settings != nil {
		configuration, err = e.settings.LoadConfiguration(ctx)
	} else {
		configuration, err = recovery.ParseConfiguration(data)
	}
	if err != nil {
		return nil, err
	}
	store, err := e.storeFactory(ctx, configuration.Storage)
	if err != nil {
		return nil, fmt.Errorf("open recovery storage: %w", err)
	}
	if err := store.Test(ctx); err != nil {
		return nil, err
	}
	manifests, err := store.ListManifests(ctx)
	if err != nil {
		return nil, err
	}
	points := make([]protocol.RecoveryPoint, 0, len(manifests))
	for _, manifest := range manifests {
		points = append(points, protocol.RecoveryPoint{
			ID: manifest.RecoveryPointID, CreatedAt: manifest.CreatedAt, VeleroBackupName: manifest.VeleroBackupName,
		})
	}
	return points, nil
}

func (e *Executor) BackupSchedule(ctx context.Context) (*protocol.BackupScheduleStatus, error) {
	status := &protocol.BackupScheduleStatus{APIVersion: protocol.APIVersion}
	if e.settings == nil {
		return status, nil
	}
	expression, _, paused, err := e.settings.BackupSettings(ctx)
	if err != nil {
		return nil, err
	}
	if expression == "" || paused {
		return status, nil
	}
	schedule, err := cron.ParseStandard(expression)
	if err != nil {
		return nil, fmt.Errorf("parse backup schedule: %w", err)
	}
	configuration, err := e.settings.LoadConfiguration(ctx)
	if err != nil {
		return nil, err
	}
	store, err := e.storeFactory(ctx, configuration.Storage)
	if err != nil {
		return nil, err
	}
	manifests, err := store.ListManifests(ctx)
	if err != nil {
		return nil, err
	}
	if len(manifests) == 0 {
		now := e.now().UTC()
		status.Due = true
		status.OperationID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("embedded-cluster-dr/schedule/initial/"+expression)).String()
		status.NextAt = &now
		return status, nil
	}
	latest := manifests[0].CreatedAt
	for _, manifest := range manifests[1:] {
		if manifest.CreatedAt.After(latest) {
			latest = manifest.CreatedAt
		}
	}
	next := schedule.Next(latest).UTC()
	status.NextAt = &next
	if !e.now().Before(next) {
		status.Due = true
		status.OperationID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("embedded-cluster-dr/schedule/"+next.Format(time.RFC3339))).String()
	}
	return status, nil
}

func (e *Executor) backup(ctx context.Context, store Store, configuration recovery.Configuration, request protocol.OperationRequest, statePath string, progress func(protocol.Progress)) (*protocol.OperationResult, string, error) {
	if e.application == nil {
		return nil, "", fmt.Errorf("application backup is unavailable outside the cluster")
	}
	if statePath == "" {
		return nil, "", fmt.Errorf("Embedded Cluster state archive was not uploaded")
	}
	recoveryPointID, err := normalizeOperationID(request.OperationID)
	if err != nil {
		return nil, "", err
	}
	backupName := "ec-dr-" + strings.ReplaceAll(recoveryPointID, "-", "")
	progress(protocol.Progress{Phase: "application-backup", Message: "Backing up application resources and volumes", Percentage: 10})
	if err := e.application.Backup(ctx, backupName, *request.BackupPolicy, progress); err != nil {
		return nil, "", err
	}
	stateKey := filepath.ToSlash(filepath.Join("recovery-points", recoveryPointID, "ec-state-v1.tar.gz.age"))
	progress(protocol.Progress{Phase: "state-upload", Message: "Encrypting and uploading Embedded Cluster state", Percentage: 80})
	if err := store.PutEncryptedFile(ctx, stateKey, statePath, configuration.RecoveryKey); err != nil {
		return nil, "", err
	}
	manifest := recovery.Manifest{
		FormatVersion: recovery.RecoveryFormatVersion, RecoveryPointID: recoveryPointID,
		CreatedAt: e.now().UTC(), VeleroBackupName: backupName, StateObjectKey: stateKey, Status: "ready",
	}
	if err := e.applyRetention(ctx, store, progress); err != nil {
		return nil, "", err
	}
	progress(protocol.Progress{Phase: "publish", Message: "Publishing complete recovery point", Percentage: 95})
	if err := store.PutManifest(ctx, manifest); err != nil {
		return nil, "", err
	}
	return &protocol.OperationResult{RecoveryPointID: recoveryPointID}, "", nil
}

func (e *Executor) applyRetention(ctx context.Context, store Store, progress func(protocol.Progress)) error {
	if e.settings == nil || e.application == nil {
		return nil
	}
	_, retentionCount, _, err := e.settings.BackupSettings(ctx)
	if err != nil {
		return err
	}
	manifests, err := store.ListRetentionManifests(ctx)
	if err != nil {
		return err
	}
	var deleting, ready []recovery.Manifest
	for _, manifest := range manifests {
		if manifest.Status == "deleting" {
			deleting = append(deleting, manifest)
		} else {
			ready = append(ready, manifest)
		}
	}
	removeCount := len(ready) - retentionCount + 1
	if removeCount > 0 {
		for index := len(ready) - removeCount; index < len(ready); index++ {
			manifest := ready[index]
			if err := store.MarkRecoveryPointDeleting(ctx, manifest); err != nil {
				return err
			}
			manifest.Status = "deleting"
			deleting = append(deleting, manifest)
		}
	}
	for _, manifest := range deleting {
		progress(protocol.Progress{Phase: "retention", Message: "Removing expired recovery point", Percentage: 90})
		if err := e.application.DeleteBackup(ctx, manifest.VeleroBackupName); err != nil {
			return err
		}
		if err := store.DeleteRecoveryPoint(ctx, manifest); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) bootstrapRestore(ctx context.Context, store Store, configuration recovery.Configuration, request protocol.OperationRequest, progress func(protocol.Progress)) (*protocol.OperationResult, string, error) {
	manifest, err := store.GetManifest(ctx, request.RecoveryPointID)
	if err != nil {
		return nil, "", err
	}
	directory := filepath.Join(e.tempRoot, request.OperationID)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, "", fmt.Errorf("create restore directory: %w", err)
	}
	destination := filepath.Join(directory, "ec-state-v1.tar.gz")
	if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
		return nil, "", fmt.Errorf("replace staged EC state archive: %w", err)
	}
	progress(protocol.Progress{Phase: "state-download", Message: "Downloading and decrypting Embedded Cluster state", Percentage: 25})
	descriptor, err := store.GetDecryptedFile(ctx, manifest.StateObjectKey, destination, configuration.RecoveryKey)
	if err != nil {
		return nil, "", err
	}
	return &protocol.OperationResult{RecoveryPointID: manifest.RecoveryPointID, StateArchive: descriptor}, destination, nil
}

func (e *Executor) applicationRestore(ctx context.Context, store Store, request protocol.OperationRequest, progress func(protocol.Progress)) (*protocol.OperationResult, string, error) {
	if e.application == nil {
		return nil, "", fmt.Errorf("application restore is unavailable outside the cluster")
	}
	manifest, err := store.GetManifest(ctx, request.RecoveryPointID)
	if err != nil {
		return nil, "", err
	}
	restoreName := "ec-dr-restore-" + strings.ReplaceAll(request.OperationID, "-", "")
	progress(protocol.Progress{Phase: "application-restore", Message: "Restoring application resources and volumes", Percentage: 10})
	if err := e.application.Restore(ctx, restoreName, manifest.VeleroBackupName, progress); err != nil {
		return nil, "", err
	}
	return &protocol.OperationResult{RecoveryPointID: manifest.RecoveryPointID}, "", nil
}

func (e *Executor) configuration(ctx context.Context, request protocol.OperationRequest) (recovery.Configuration, error) {
	if request.Phase == protocol.PhaseBootstrap {
		return recovery.ParseConfiguration(request.Configuration)
	}
	if len(request.Configuration) > 0 {
		configuration, err := recovery.ParseConfiguration(request.Configuration)
		if err != nil {
			return recovery.Configuration{}, err
		}
		if e.settings == nil {
			return recovery.Configuration{}, fmt.Errorf("in-cluster configuration store is unavailable")
		}
		if err := e.settings.SaveRestoreConfiguration(ctx, configuration); err != nil {
			return recovery.Configuration{}, err
		}
		return configuration, nil
	}
	if e.settings != nil {
		return e.settings.LoadConfiguration(ctx)
	}
	return e.fromEnv()
}

func normalizeOperationID(operationID string) (string, error) {
	parsed, err := uuid.Parse(operationID)
	if err != nil {
		return "", fmt.Errorf("invalid operation ID: %w", err)
	}
	return parsed.String(), nil
}
