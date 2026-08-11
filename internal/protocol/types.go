package protocol

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	APIVersion           = "lifecycle.embeddedcluster.replicated.com/v1alpha1"
	MaxConfigurationSize = 1 << 20
)

type Operation string

const (
	OperationBackup  Operation = "backup"
	OperationRestore Operation = "restore"
)

type Phase string

const (
	PhaseBootstrap Phase = "bootstrap"
	PhaseInCluster Phase = "in-cluster"
)

type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCanceled  State = "canceled"
)

type OperationRequest struct {
	APIVersion      string              `json:"apiVersion"`
	OperationID     string              `json:"operationId"`
	Operation       Operation           `json:"operation"`
	Phase           Phase               `json:"phase"`
	RecoveryPointID string              `json:"recoveryPointId,omitempty"`
	StateArchive    *ArtifactDescriptor `json:"stateArchive,omitempty"`
	BackupPolicy    *BackupPolicy       `json:"backupPolicy,omitempty"`
	Configuration   json.RawMessage     `json:"configuration,omitempty"`
	Headless        bool                `json:"headless,omitempty"`
	Deadline        *time.Time          `json:"deadline,omitempty"`
}

type BackupPolicy struct {
	IncludedNamespaces       []string `json:"includedNamespaces"`
	IncludedClusterResources []string `json:"includedClusterResources,omitempty"`
	VolumeBackup             string   `json:"volumeBackup"`
}

type ArtifactDescriptor struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type OperationResult struct {
	RecoveryPointID string              `json:"recoveryPointId"`
	StateArchive    *ArtifactDescriptor `json:"stateArchive,omitempty"`
}

type OperationStatus struct {
	APIVersion  string           `json:"apiVersion"`
	OperationID string           `json:"operationId"`
	State       State            `json:"state"`
	Progress    *Progress        `json:"progress,omitempty"`
	UI          *UI              `json:"ui,omitempty"`
	Result      *OperationResult `json:"result,omitempty"`
	Error       *Error           `json:"error,omitempty"`
	UpdatedAt   time.Time        `json:"updatedAt"`
}

type Progress struct {
	Phase      string `json:"phase"`
	Message    string `json:"message"`
	Percentage int    `json:"percentage,omitempty"`
}

type UI struct {
	Path string `json:"path"`
}

type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

type RecoveryPoint struct {
	ID               string    `json:"id"`
	CreatedAt        time.Time `json:"createdAt"`
	VeleroBackupName string    `json:"veleroBackupName,omitempty"`
}

type BackupScheduleStatus struct {
	APIVersion  string     `json:"apiVersion"`
	Due         bool       `json:"due"`
	OperationID string     `json:"operationId,omitempty"`
	NextAt      *time.Time `json:"nextAt,omitempty"`
}

func (r OperationRequest) Validate() error {
	if r.APIVersion != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q", r.APIVersion)
	}
	if _, err := uuid.Parse(r.OperationID); err != nil {
		return fmt.Errorf("operationId must be a UUID: %w", err)
	}
	if r.Operation != OperationBackup && r.Operation != OperationRestore {
		return fmt.Errorf("unsupported operation %q", r.Operation)
	}
	if r.Phase != PhaseBootstrap && r.Phase != PhaseInCluster {
		return fmt.Errorf("unsupported phase %q", r.Phase)
	}
	if r.Operation == OperationBackup {
		if r.Phase != PhaseInCluster || r.StateArchive == nil || r.BackupPolicy == nil {
			return fmt.Errorf("backup requires in-cluster phase, state archive, and backup policy")
		}
		if len(r.BackupPolicy.IncludedNamespaces) == 0 || r.BackupPolicy.VolumeBackup != "fileSystem" {
			return fmt.Errorf("backup policy requires namespaces and fileSystem volumes")
		}
	} else if r.BackupPolicy != nil {
		return fmt.Errorf("backupPolicy is valid only for backup operations")
	}
	if r.Operation == OperationRestore && r.Phase == PhaseInCluster && r.RecoveryPointID == "" {
		return fmt.Errorf("in-cluster restore requires recoveryPointId")
	}
	if r.StateArchive != nil {
		if err := r.StateArchive.Validate(); err != nil {
			return err
		}
	}
	if len(r.Configuration) > 0 {
		if r.Operation != OperationRestore || (r.Phase == PhaseBootstrap && !r.Headless) {
			return fmt.Errorf("configuration is valid only for restore operations and headless bootstrap")
		}
		if len(r.Configuration) > MaxConfigurationSize {
			return fmt.Errorf("configuration exceeds %d bytes", MaxConfigurationSize)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(r.Configuration, &object); err != nil || object == nil {
			return fmt.Errorf("configuration must be a JSON object")
		}
	}
	return nil
}

func (a ArtifactDescriptor) Validate() error {
	_, err := hex.DecodeString(a.SHA256)
	if a.Name == "" || strings.ContainsAny(a.Name, `/\\`) || a.Size <= 0 || len(a.SHA256) != 64 || err != nil {
		return fmt.Errorf("invalid artifact descriptor")
	}
	return nil
}
