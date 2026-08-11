package settings

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/kubernetes"
	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/recovery"
)

type fakeKubernetesClient struct {
	objects    map[string]map[string]any
	deployment map[string]any
	writes     []string
}

func newFakeKubernetesClient() *fakeKubernetesClient {
	return &fakeKubernetesClient{
		objects: map[string]map[string]any{},
		deployment: map[string]any{
			"metadata": map[string]any{"generation": float64(1)},
			"spec": map[string]any{
				"replicas": float64(1),
				"template": map[string]any{"metadata": map[string]any{}},
			},
			"status": map[string]any{"observedGeneration": float64(1), "updatedReplicas": float64(1), "availableReplicas": float64(1)},
		},
	}
}

func (f *fakeKubernetesClient) Do(_ context.Context, method, path string, body, destination any) (int, error) {
	if strings.HasSuffix(path, "/deployments/velero") {
		switch method {
		case http.MethodGet:
			copyJSON(destination, f.deployment)
			return http.StatusOK, nil
		case http.MethodPut:
			f.deployment = cloneMap(body)
			f.deployment["metadata"].(map[string]any)["generation"] = float64(2)
			f.deployment["status"] = map[string]any{"observedGeneration": float64(2), "updatedReplicas": float64(1), "availableReplicas": float64(1)}
			f.writes = append(f.writes, "Deployment/velero")
			copyJSON(destination, f.deployment)
			return http.StatusOK, nil
		}
	}
	if method == http.MethodGet {
		object := f.objects[path]
		if object == nil {
			return http.StatusNotFound, &kubernetes.APIError{StatusCode: http.StatusNotFound, Message: "not found"}
		}
		copyJSON(destination, object)
		return http.StatusOK, nil
	}
	object := cloneMap(body)
	metadata := object["metadata"].(map[string]any)
	name := metadata["name"].(string)
	metadata["resourceVersion"] = "1"
	if stringData, ok := object["stringData"].(map[string]any); ok {
		data := map[string]any{}
		for key, value := range stringData {
			data[key] = base64.StdEncoding.EncodeToString([]byte(value.(string)))
		}
		object["data"] = data
		delete(object, "stringData")
	}
	collection := path
	if method == http.MethodPut {
		collection = path[:strings.LastIndex(path, "/")]
	}
	itemPath := collection + "/" + name
	f.objects[itemPath] = object
	f.writes = append(f.writes, fmt.Sprint(object["kind"])+"/"+name)
	copyJSON(destination, object)
	if method == http.MethodPost {
		return http.StatusCreated, nil
	}
	return http.StatusOK, nil
}

func TestSaveSetupPublishesConfigurationLast(t *testing.T) {
	client := newFakeKubernetesClient()
	store, err := NewStore(client, "dr")
	if err != nil {
		t.Fatal(err)
	}
	store.pollInterval = time.Millisecond
	settings, generated, err := store.SaveSetup(context.Background(), SetupRequest{
		Storage: recovery.StorageConfiguration{
			Bucket: "recovery", Region: "auto", Endpoint: "https://account.r2.cloudflarestorage.com",
			AccessKeyID: "access", SecretAccessKey: "secret", ForcePathStyle: true,
			CustomCAPEM: "certificate", ProxyURL: "https://proxy.internal:8443",
		},
		Schedule: "0 2 * * *", RetentionCount: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !generated || len(settings.RecoveryKey) < 32 {
		t.Fatalf("recovery key was not generated: %#v", settings)
	}
	wantWrites := []string{
		"Secret/velero-repo-credentials", "Secret/embedded-cluster-dr-proxy",
		"BackupStorageLocation/default", "Deployment/velero", "Secret/embedded-cluster-dr",
	}
	if fmt.Sprint(client.writes) != fmt.Sprint(wantWrites) {
		t.Fatalf("writes = %v, want %v", client.writes, wantWrites)
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Schedule != "0 2 * * *" || loaded.RetentionCount != 7 || loaded.RecoveryKey != settings.RecoveryKey {
		t.Fatalf("unexpected loaded settings: %#v", loaded)
	}
}

func TestSaveSetupPreservesStoredSecretsAndRestorePausesSchedule(t *testing.T) {
	client := newFakeKubernetesClient()
	store, _ := NewStore(client, "dr")
	store.pollInterval = time.Millisecond
	first, _, err := store.SaveSetup(context.Background(), SetupRequest{
		Storage: recovery.StorageConfiguration{
			Bucket: "recovery", AccessKeyID: "access", SecretAccessKey: "secret",
			CustomCAPEM: "certificate", ProxyURL: "https://proxy.internal:8443",
		},
		Schedule: "0 2 * * *", RetentionCount: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, generated, err := store.SaveSetup(context.Background(), SetupRequest{
		Storage: recovery.StorageConfiguration{Bucket: "recovery"}, Schedule: "0 3 * * *", RetentionCount: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if generated || updated.RecoveryKey != first.RecoveryKey || updated.Storage.SecretAccessKey != "secret" || updated.Storage.CustomCAPEM != "certificate" || updated.Storage.ProxyURL == "" {
		t.Fatalf("stored secrets were not preserved: %#v", updated)
	}
	if _, err := store.SaveRestore(context.Background(), updated.Configuration); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !restored.SchedulePaused || restored.Schedule != "" {
		t.Fatalf("restore did not pause scheduled backups: %#v", restored)
	}
}

func TestBackupSettingsAllowsUnconfiguredExtension(t *testing.T) {
	store, _ := NewStore(newFakeKubernetesClient(), "dr")
	schedule, retention, paused, err := store.BackupSettings(context.Background())
	if err != nil || schedule != "" || retention != 0 || paused {
		t.Fatalf("unexpected unconfigured settings: %q %d %v %v", schedule, retention, paused, err)
	}
}

func cloneMap(value any) map[string]any {
	data, _ := json.Marshal(value)
	var result map[string]any
	_ = json.Unmarshal(data, &result)
	return result
}

func copyJSON(destination, source any) {
	if destination == nil {
		return
	}
	data, _ := json.Marshal(source)
	_ = json.Unmarshal(data, destination)
}
