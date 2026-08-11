package settings

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/kubernetes"
	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/recovery"
	"github.com/robfig/cron/v3"
)

const (
	configurationSecret = "embedded-cluster-dr"
	repositorySecret    = "velero-repo-credentials"
	proxySecret         = "embedded-cluster-dr-proxy"
)

type Settings struct {
	recovery.Configuration
	Schedule       string `json:"schedule,omitempty"`
	RetentionCount int    `json:"retentionCount"`
	SchedulePaused bool   `json:"schedulePaused"`
}

type SetupRequest struct {
	Storage        recovery.StorageConfiguration `json:"storage"`
	Schedule       string                        `json:"schedule,omitempty"`
	RetentionCount int                           `json:"retentionCount"`
}

type ConfigurationStatus struct {
	Configured      bool   `json:"configured"`
	AccessKeyID     string `json:"accessKeyId,omitempty"`
	Bucket          string `json:"bucket,omitempty"`
	Prefix          string `json:"prefix,omitempty"`
	Region          string `json:"region,omitempty"`
	Endpoint        string `json:"endpoint,omitempty"`
	CustomCA        bool   `json:"customCa"`
	ProxyConfigured bool   `json:"proxyConfigured"`
	Schedule        string `json:"schedule,omitempty"`
	RetentionCount  int    `json:"retentionCount,omitempty"`
	SchedulePaused  bool   `json:"schedulePaused"`
}

type Store struct {
	client       kubernetesClient
	namespace    string
	pollInterval time.Duration
}

type kubernetesClient interface {
	Do(context.Context, string, string, any, any) (int, error)
}

func NewStore(client kubernetesClient, namespace string) (*Store, error) {
	if client == nil || namespace == "" {
		return nil, fmt.Errorf("Kubernetes client and namespace are required")
	}
	return &Store{client: client, namespace: namespace, pollInterval: 2 * time.Second}, nil
}

func (s *Store) Load(ctx context.Context) (Settings, error) {
	var secret struct {
		Data map[string]string `json:"data"`
	}
	_, err := s.client.Do(ctx, http.MethodGet, s.secretPath(configurationSecret), nil, &secret)
	if err != nil {
		return Settings{}, fmt.Errorf("read disaster recovery configuration: %w", err)
	}
	encoded := secret.Data["configuration.json"]
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return Settings{}, fmt.Errorf("disaster recovery configuration secret is invalid")
	}
	var settings Settings
	if err := json.Unmarshal(data, &settings); err != nil {
		return Settings{}, fmt.Errorf("decode disaster recovery configuration: %w", err)
	}
	if err := settings.Validate(); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

func (s *Store) LoadConfiguration(ctx context.Context) (recovery.Configuration, error) {
	settings, err := s.Load(ctx)
	return settings.Configuration, err
}

func (s *Store) ConfigurationStatus(ctx context.Context) (any, error) {
	settings, err := s.Load(ctx)
	if err != nil {
		if isNotFound(err) {
			return ConfigurationStatus{}, nil
		}
		return nil, err
	}
	return ConfigurationStatus{
		Configured: true, AccessKeyID: settings.Storage.AccessKeyID, Bucket: settings.Storage.Bucket, Prefix: settings.Storage.Prefix,
		Region: settings.Storage.Region, Endpoint: settings.Storage.Endpoint,
		CustomCA: settings.Storage.CustomCAPEM != "", ProxyConfigured: settings.Storage.ProxyURL != "",
		Schedule: settings.Schedule, RetentionCount: settings.RetentionCount, SchedulePaused: settings.SchedulePaused,
	}, nil
}

func (s *Store) Configure(ctx context.Context, data json.RawMessage) (string, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request SetupRequest
	if err := decoder.Decode(&request); err != nil {
		return "", false, fmt.Errorf("decode disaster recovery setup: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return "", false, fmt.Errorf("decode disaster recovery setup: expected one JSON object")
		}
		return "", false, fmt.Errorf("decode disaster recovery setup: %w", err)
	}
	if request.Schedule != "" {
		if _, err := cron.ParseStandard(request.Schedule); err != nil {
			return "", false, fmt.Errorf("schedule must be a five-field cron expression: %w", err)
		}
	}
	if current, err := s.Load(ctx); err == nil {
		mergeStoredStorage(&request.Storage, current.Storage)
	} else if !isNotFound(err) {
		return "", false, err
	}
	if err := (recovery.Configuration{Storage: request.Storage, RecoveryKey: "configuration-validation-key"}).Validate(); err != nil {
		return "", false, err
	}
	probe, err := recovery.NewObjectStore(ctx, request.Storage)
	if err != nil {
		return "", false, err
	}
	if err := probe.Test(ctx); err != nil {
		return "", false, err
	}
	if err := probe.TestWritable(ctx); err != nil {
		return "", false, err
	}
	settings, generated, err := s.SaveSetup(ctx, request)
	if err != nil {
		return "", false, err
	}
	if generated {
		return settings.RecoveryKey, true, nil
	}
	return "", false, nil
}

func (s *Store) BackupSettings(ctx context.Context) (string, int, bool, error) {
	settings, err := s.Load(ctx)
	if err != nil {
		if isNotFound(err) {
			return "", 0, false, nil
		}
		return "", 0, false, err
	}
	return settings.Schedule, settings.RetentionCount, settings.SchedulePaused, nil
}

func (s *Store) SaveRestoreConfiguration(ctx context.Context, configuration recovery.Configuration) error {
	_, err := s.SaveRestore(ctx, configuration)
	return err
}

func (s *Store) AcceptRestoreConfiguration(ctx context.Context, data json.RawMessage) error {
	configuration, err := recovery.ParseConfiguration(data)
	if err != nil {
		return err
	}
	return s.SaveRestoreConfiguration(ctx, configuration)
}

func (s *Store) SaveSetup(ctx context.Context, request SetupRequest) (Settings, bool, error) {
	if request.RetentionCount < 1 || request.RetentionCount > 1000 {
		return Settings{}, false, fmt.Errorf("retentionCount must be between 1 and 1000")
	}
	recoveryKey := ""
	generated := false
	current, currentErr := s.Load(ctx)
	if currentErr == nil {
		recoveryKey = current.RecoveryKey
		mergeStoredStorage(&request.Storage, current.Storage)
	} else if !isNotFound(currentErr) {
		return Settings{}, false, currentErr
	}
	if recoveryKey == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return Settings{}, false, fmt.Errorf("generate recovery key: %w", err)
		}
		recoveryKey = base64.RawURLEncoding.EncodeToString(key)
		generated = true
	}
	settings := Settings{
		Configuration: recovery.Configuration{Storage: request.Storage, RecoveryKey: recoveryKey},
		Schedule:      strings.TrimSpace(request.Schedule), RetentionCount: request.RetentionCount,
	}
	if err := settings.Validate(); err != nil {
		return Settings{}, false, err
	}
	if err := s.save(ctx, settings); err != nil {
		return Settings{}, false, err
	}
	return settings, generated, nil
}

func mergeStoredStorage(request *recovery.StorageConfiguration, current recovery.StorageConfiguration) {
	if request.AccessKeyID == "" {
		request.AccessKeyID = current.AccessKeyID
	}
	if request.SecretAccessKey == "" {
		request.SecretAccessKey = current.SecretAccessKey
	}
	if request.CustomCAPEM == "" {
		request.CustomCAPEM = current.CustomCAPEM
	}
	if request.ProxyURL == "" {
		request.ProxyURL = current.ProxyURL
	}
}

func (s *Store) SaveRestore(ctx context.Context, configuration recovery.Configuration) (Settings, error) {
	if err := configuration.Validate(); err != nil {
		return Settings{}, err
	}
	settings := Settings{Configuration: configuration, RetentionCount: 30, SchedulePaused: true}
	if err := s.save(ctx, settings); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

func (s Settings) Validate() error {
	if err := s.Configuration.Validate(); err != nil {
		return err
	}
	if s.Schedule != "" {
		if _, err := cron.ParseStandard(s.Schedule); err != nil {
			return fmt.Errorf("schedule must be a five-field cron expression: %w", err)
		}
	}
	if s.RetentionCount < 1 || s.RetentionCount > 1000 {
		return fmt.Errorf("retentionCount must be between 1 and 1000")
	}
	return nil
}

func (s *Store) save(ctx context.Context, settings Settings) error {
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode disaster recovery configuration: %w", err)
	}
	configuration := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": configurationSecret, "namespace": s.namespace},
		"type":     "Opaque",
		"stringData": map[string]any{
			"configuration.json": string(data),
			"access-key-id":      settings.Storage.AccessKeyID, "secret-access-key": settings.Storage.SecretAccessKey,
			"recovery-key": settings.RecoveryKey, "custom-ca.pem": settings.Storage.CustomCAPEM,
			"proxy-url": settings.Storage.ProxyURL,
			"cloud":     fmt.Sprintf("[default]\naws_access_key_id=%s\naws_secret_access_key=%s\n", settings.Storage.AccessKeyID, settings.Storage.SecretAccessKey),
		},
	}
	repository := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": repositorySecret, "namespace": s.namespace},
		"type":     "Opaque", "stringData": map[string]any{"repository-password": settings.RecoveryKey},
	}
	if err := s.upsert(ctx, "/api/v1/namespaces/"+url.PathEscape(s.namespace)+"/secrets", repositorySecret, repository); err != nil {
		return fmt.Errorf("save Velero repository credential: %w", err)
	}
	proxy := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": proxySecret, "namespace": s.namespace},
		"type":     "Opaque", "stringData": map[string]any{
			"HTTP_PROXY": settings.Storage.ProxyURL, "HTTPS_PROXY": settings.Storage.ProxyURL,
		},
	}
	if err := s.upsert(ctx, "/api/v1/namespaces/"+url.PathEscape(s.namespace)+"/secrets", proxySecret, proxy); err != nil {
		return fmt.Errorf("save Velero proxy configuration: %w", err)
	}
	bsl := map[string]any{
		"apiVersion": "velero.io/v1", "kind": "BackupStorageLocation",
		"metadata": map[string]any{"name": "default", "namespace": s.namespace},
		"spec": map[string]any{
			"provider": "aws", "default": true,
			"objectStorage": map[string]any{"bucket": settings.Storage.Bucket, "prefix": settings.Storage.Prefix},
			"credential":    map[string]any{"name": configurationSecret, "key": "cloud"},
			"config":        backupStorageConfig(settings.Storage),
		},
	}
	if settings.Storage.CustomCAPEM != "" {
		bsl["spec"].(map[string]any)["caCert"] = base64.StdEncoding.EncodeToString([]byte(settings.Storage.CustomCAPEM))
	}
	collection := "/apis/velero.io/v1/namespaces/" + url.PathEscape(s.namespace) + "/backupstoragelocations"
	if err := s.upsert(ctx, collection, "default", bsl); err != nil {
		return fmt.Errorf("configure Velero backup storage: %w", err)
	}
	if err := s.rolloutVelero(ctx, settings.Storage.ProxyURL); err != nil {
		return err
	}
	// Publish the settings Secret last. ConfigurationStatus and scheduled
	// operations cannot observe a successful setup until every Velero
	// dependency above has been accepted and the proxy rollout is ready.
	if err := s.upsert(ctx, "/api/v1/namespaces/"+url.PathEscape(s.namespace)+"/secrets", configurationSecret, configuration); err != nil {
		return fmt.Errorf("save disaster recovery credentials: %w", err)
	}
	return nil
}

func (s *Store) upsert(ctx context.Context, collection, name string, object map[string]any) error {
	var current struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	_, err := s.client.Do(ctx, http.MethodGet, collection+"/"+url.PathEscape(name), nil, &current)
	if err == nil {
		object["metadata"].(map[string]any)["resourceVersion"] = current.Metadata.ResourceVersion
		_, err = s.client.Do(ctx, http.MethodPut, collection+"/"+url.PathEscape(name), object, nil)
		return err
	}
	var apiError *kubernetes.APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusNotFound {
		return err
	}
	_, err = s.client.Do(ctx, http.MethodPost, collection, object, nil)
	return err
}

func (s *Store) secretPath(name string) string {
	return "/api/v1/namespaces/" + url.PathEscape(s.namespace) + "/secrets/" + url.PathEscape(name)
}

func isNotFound(err error) bool {
	var apiError *kubernetes.APIError
	return errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotFound
}

func backupStorageConfig(storage recovery.StorageConfiguration) map[string]any {
	config := map[string]any{"region": storage.Region, "s3ForcePathStyle": fmt.Sprint(storage.ForcePathStyle)}
	if storage.Endpoint != "" {
		config["s3Url"] = storage.Endpoint
	}
	return config
}

func (s *Store) rolloutVelero(ctx context.Context, proxyURL string) error {
	path := "/apis/apps/v1/namespaces/" + url.PathEscape(s.namespace) + "/deployments/velero"
	var deployment map[string]any
	if _, err := s.client.Do(ctx, http.MethodGet, path, nil, &deployment); err != nil {
		return fmt.Errorf("read Velero deployment: %w", err)
	}
	spec, ok := deployment["spec"].(map[string]any)
	if !ok {
		return fmt.Errorf("Velero deployment has no spec")
	}
	template, ok := spec["template"].(map[string]any)
	if !ok {
		return fmt.Errorf("Velero deployment has no pod template")
	}
	metadata, _ := template["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
		template["metadata"] = metadata
	}
	annotations, _ := metadata["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
		metadata["annotations"] = annotations
	}
	digest := sha256.Sum256([]byte(proxyURL))
	wanted := base64.RawURLEncoding.EncodeToString(digest[:])
	if annotations["dr.embeddedcluster.replicated.com/proxy-hash"] != wanted {
		annotations["dr.embeddedcluster.replicated.com/proxy-hash"] = wanted
		if _, err := s.client.Do(ctx, http.MethodPut, path, deployment, &deployment); err != nil {
			return fmt.Errorf("restart Velero for storage proxy configuration: %w", err)
		}
	}
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		if deploymentReady(deployment) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for Velero rollout: %w", ctx.Err())
		case <-ticker.C:
			deployment = map[string]any{}
			if _, err := s.client.Do(ctx, http.MethodGet, path, nil, &deployment); err != nil {
				return fmt.Errorf("wait for Velero rollout: %w", err)
			}
		}
	}
}

func deploymentReady(deployment map[string]any) bool {
	metadata, _ := deployment["metadata"].(map[string]any)
	spec, _ := deployment["spec"].(map[string]any)
	status, _ := deployment["status"].(map[string]any)
	generation, _ := metadata["generation"].(float64)
	observed, _ := status["observedGeneration"].(float64)
	replicas, _ := spec["replicas"].(float64)
	updated, _ := status["updatedReplicas"].(float64)
	available, _ := status["availableReplicas"].(float64)
	return generation > 0 && observed >= generation && replicas > 0 && updated >= replicas && available >= replicas
}
