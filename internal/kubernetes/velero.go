package kubernetes

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/protocol"
)

type Velero struct {
	client            *Client
	namespace         string
	pollInterval      time.Duration
	backupSyncTimeout time.Duration
}

func NewVelero(client *Client, namespace string) *Velero {
	return &Velero{client: client, namespace: namespace, pollInterval: 5 * time.Second, backupSyncTimeout: 5 * time.Minute}
}

func (v *Velero) Backup(ctx context.Context, name string, policy protocol.BackupPolicy, progress func(protocol.Progress)) error {
	resource := map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": name, "namespace": v.namespace, "labels": map[string]any{"app.kubernetes.io/managed-by": "embedded-cluster-disaster-recovery"}},
		"spec": map[string]any{
			"includedNamespaces":             policy.IncludedNamespaces,
			"includedClusterScopedResources": policy.IncludedClusterResources,
			"defaultVolumesToFsBackup":       true,
			"snapshotVolumes":                false,
		},
	}
	path := v.collectionPath("backups")
	status, err := v.client.Do(ctx, http.MethodPost, path, resource, nil)
	if err != nil && status != http.StatusConflict {
		return fmt.Errorf("create Velero backup: %w", err)
	}
	return v.wait(ctx, "backups", name, progress)
}

func (v *Velero) Restore(ctx context.Context, name, backupName string, progress func(protocol.Progress)) error {
	progress(protocol.Progress{Phase: "velero-sync", Message: "Waiting for Velero backup storage synchronization"})
	syncCtx, cancel := context.WithTimeout(ctx, v.backupSyncTimeout)
	defer cancel()
	if err := v.waitForBackup(syncCtx, backupName); err != nil {
		return fmt.Errorf("wait for Velero backup %q to synchronize: %w", backupName, err)
	}
	resource := map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Restore",
		"metadata": map[string]any{"name": name, "namespace": v.namespace, "labels": map[string]any{"app.kubernetes.io/managed-by": "embedded-cluster-disaster-recovery"}},
		"spec":     map[string]any{"backupName": backupName, "existingResourcePolicy": "update", "restorePVs": true},
	}
	path := v.collectionPath("restores")
	status, err := v.client.Do(ctx, http.MethodPost, path, resource, nil)
	if err != nil && status != http.StatusConflict {
		return fmt.Errorf("create Velero restore: %w", err)
	}
	return v.wait(ctx, "restores", name, progress)
}

func (v *Velero) waitForBackup(ctx context.Context, name string) error {
	path := v.collectionPath("backups") + "/" + url.PathEscape(name)
	ticker := time.NewTicker(v.pollInterval)
	defer ticker.Stop()
	for {
		status, err := v.client.Do(ctx, http.MethodGet, path, nil, nil)
		if err == nil {
			return nil
		}
		if status != http.StatusNotFound {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (v *Velero) DeleteBackup(ctx context.Context, backupName string) error {
	name := "delete-" + backupName
	resource := map[string]any{
		"apiVersion": "velero.io/v1", "kind": "DeleteBackupRequest",
		"metadata": map[string]any{"name": name, "namespace": v.namespace, "labels": map[string]any{"app.kubernetes.io/managed-by": "embedded-cluster-disaster-recovery"}},
		"spec":     map[string]any{"backupName": backupName},
	}
	path := v.collectionPath("deletebackuprequests")
	status, err := v.client.Do(ctx, http.MethodPost, path, resource, nil)
	if err != nil && status != http.StatusConflict {
		return fmt.Errorf("request Velero backup deletion: %w", err)
	}
	ticker := time.NewTicker(v.pollInterval)
	defer ticker.Stop()
	for {
		var object struct {
			Status struct {
				Phase  string   `json:"phase"`
				Errors []string `json:"errors"`
			} `json:"status"`
		}
		if _, err := v.client.Do(ctx, http.MethodGet, path+"/"+url.PathEscape(name), nil, &object); err != nil {
			return fmt.Errorf("read Velero backup deletion: %w", err)
		}
		if object.Status.Phase == "Processed" {
			if len(object.Status.Errors) > 0 {
				return fmt.Errorf("Velero backup deletion failed: %s", strings.Join(object.Status.Errors, "; "))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (v *Velero) wait(ctx context.Context, resource, name string, progress func(protocol.Progress)) error {
	ticker := time.NewTicker(v.pollInterval)
	defer ticker.Stop()
	for {
		var object struct {
			Status struct {
				Phase            string   `json:"phase"`
				Errors           int      `json:"errors"`
				Warnings         int      `json:"warnings"`
				ValidationErrors []string `json:"validationErrors"`
				FailureReason    string   `json:"failureReason"`
			} `json:"status"`
		}
		path := v.collectionPath(resource) + "/" + url.PathEscape(name)
		if _, err := v.client.Do(ctx, http.MethodGet, path, nil, &object); err != nil {
			return fmt.Errorf("read Velero %s %q: %w", strings.TrimSuffix(resource, "s"), name, err)
		}
		phase := object.Status.Phase
		progress(protocol.Progress{Phase: "velero-" + strings.ToLower(phase), Message: fmt.Sprintf("Velero %s %s", strings.TrimSuffix(resource, "s"), phase)})
		switch phase {
		case "Completed":
			if object.Status.Errors > 0 {
				return fmt.Errorf("Velero %s completed with %d errors", strings.TrimSuffix(resource, "s"), object.Status.Errors)
			}
			return nil
		case "Failed", "PartiallyFailed", "FailedValidation":
			reason := object.Status.FailureReason
			if reason == "" && len(object.Status.ValidationErrors) > 0 {
				reason = strings.Join(object.Status.ValidationErrors, "; ")
			}
			return fmt.Errorf("Velero %s %s: %s", strings.TrimSuffix(resource, "s"), phase, reason)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (v *Velero) collectionPath(resource string) string {
	return "/apis/velero.io/v1/namespaces/" + url.PathEscape(v.namespace) + "/" + resource
}
