package protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxRequestBytes = 2 << 20

type Executor interface {
	Execute(context.Context, OperationRequest, string, func(Progress)) (*OperationResult, string, error)
	ListRecoveryPoints(context.Context, json.RawMessage) ([]RecoveryPoint, error)
	BackupSchedule(context.Context) (*BackupScheduleStatus, error)
}

type SettingsManager interface {
	ConfigurationStatus(context.Context) (any, error)
	Configure(context.Context, json.RawMessage) (string, bool, error)
	AcceptRestoreConfiguration(context.Context, json.RawMessage) error
}

type operationRecord struct {
	request           OperationRequest
	requestJSON       []byte
	status            OperationStatus
	statePath         string
	downloadPath      string
	configurationPath string
	attemptTimeout    time.Duration
	cancel            context.CancelFunc
}

type Server struct {
	executor Executor
	settings SettingsManager
	tempRoot string
	now      func() time.Time

	mu         sync.RWMutex
	createMu   sync.Mutex
	operations map[string]*operationRecord
}

func (s *Server) SetSettings(settings SettingsManager) {
	s.settings = settings
}

func NewServer(executor Executor, tempRoot string) (*Server, error) {
	if executor == nil {
		return nil, fmt.Errorf("executor is required")
	}
	if tempRoot == "" {
		return nil, fmt.Errorf("temp root is required")
	}
	if err := os.MkdirAll(tempRoot, 0700); err != nil {
		return nil, fmt.Errorf("create operation directory: %w", err)
	}
	if err := os.Chmod(tempRoot, 0700); err != nil {
		return nil, fmt.Errorf("secure operation directory: %w", err)
	}
	return &Server{executor: executor, tempRoot: tempRoot, now: time.Now, operations: map[string]*operationRecord{}}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/operations", s.createOperation)
	mux.HandleFunc("PUT /v1/operations/{id}/artifacts/ec-state", s.uploadState)
	mux.HandleFunc("GET /v1/operations/{id}/artifacts/ec-state", s.downloadState)
	mux.HandleFunc("GET /v1/operations/{id}/configuration", s.downloadConfiguration)
	mux.HandleFunc("POST /v1/operations/{id}/run", s.runOperation)
	mux.HandleFunc("GET /v1/operations/{id}", s.getOperation)
	mux.HandleFunc("GET /v1/operations/{id}/events", s.getEvents)
	mux.HandleFunc("DELETE /v1/operations/{id}", s.cancelOperation)
	mux.HandleFunc("GET /ui/restore", s.restoreUI)
	mux.HandleFunc("GET /ui/backup", s.backupUI)
	mux.HandleFunc("POST /ui/api/operations/{id}/recovery-points", s.listRecoveryPoints)
	mux.HandleFunc("POST /ui/api/operations/{id}/select", s.selectRecoveryPoint)
	mux.HandleFunc("GET /ui/api/configuration", s.getConfiguration)
	mux.HandleFunc("POST /ui/api/configuration", s.saveConfiguration)
	mux.HandleFunc("GET /ui/api/recovery-points", s.listConfiguredRecoveryPoints)
	mux.HandleFunc("GET /v1/schedules/backup", s.getBackupSchedule)
	return mux
}

func (s *Server) getBackupSchedule(writer http.ResponseWriter, request *http.Request) {
	if s.settings == nil {
		writeProblem(writer, http.StatusNotFound, "not_available", "backup schedule is unavailable", false)
		return
	}
	status, err := s.executor.BackupSchedule(request.Context())
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "schedule_unavailable", "backup schedule is unavailable", true)
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

func (s *Server) getConfiguration(writer http.ResponseWriter, request *http.Request) {
	if s.settings == nil {
		writeProblem(writer, http.StatusNotFound, "not_available", "in-cluster configuration is unavailable", false)
		return
	}
	status, err := s.settings.ConfigurationStatus(request.Context())
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "configuration_unavailable", "disaster recovery configuration is unavailable", true)
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

func (s *Server) saveConfiguration(writer http.ResponseWriter, request *http.Request) {
	if s.settings == nil {
		writeProblem(writer, http.StatusNotFound, "not_available", "in-cluster configuration is unavailable", false)
		return
	}
	var configuration json.RawMessage
	if err := decodeJSON(request, &configuration); err != nil {
		writeProblem(writer, http.StatusBadRequest, "invalid_configuration", err.Error(), false)
		return
	}
	recoveryKey, generated, err := s.settings.Configure(request.Context(), configuration)
	if err != nil {
		writeProblem(writer, http.StatusBadGateway, "configuration_failed", err.Error(), true)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{"configured": true, "recoveryKey": recoveryKey, "recoveryKeyGenerated": generated})
}

func (s *Server) listConfiguredRecoveryPoints(writer http.ResponseWriter, request *http.Request) {
	if s.settings == nil {
		writeProblem(writer, http.StatusNotFound, "not_available", "in-cluster configuration is unavailable", false)
		return
	}
	points, err := s.executor.ListRecoveryPoints(request.Context(), nil)
	if err != nil {
		writeProblem(writer, http.StatusBadGateway, "storage_unavailable", err.Error(), true)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"recoveryPoints": points})
}

func (s *Server) createOperation(writer http.ResponseWriter, request *http.Request) {
	// Configuration acceptance may update several Kubernetes resources. Keep
	// creates serialized so a concurrent conflicting request cannot change
	// durable settings before its operation ID conflict is detected.
	s.createMu.Lock()
	defer s.createMu.Unlock()

	var operation OperationRequest
	if err := decodeJSON(request, &operation); err != nil {
		writeProblem(writer, http.StatusBadRequest, "invalid_request", err.Error(), false)
		return
	}
	if err := operation.Validate(); err != nil {
		writeProblem(writer, http.StatusBadRequest, "invalid_request", err.Error(), false)
		return
	}
	attemptTimeout := time.Duration(0)
	if operation.Deadline != nil {
		attemptTimeout = operation.Deadline.Sub(s.now())
		if attemptTimeout <= 0 {
			writeProblem(writer, http.StatusBadRequest, "invalid_request", "operation deadline must be in the future", false)
			return
		}
	}
	encoded, _ := canonicalRequest(operation)

	s.mu.Lock()
	if existing := s.operations[operation.OperationID]; existing != nil {
		if !bytes.Equal(existing.requestJSON, encoded) {
			s.mu.Unlock()
			writeProblem(writer, http.StatusConflict, "operation_conflict", "operation ID already exists with different input", false)
			return
		}
		if operation.Deadline != nil && (existing.request.Deadline == nil || operation.Deadline.After(*existing.request.Deadline)) {
			extended := *operation.Deadline
			existing.request.Deadline = &extended
			existing.attemptTimeout = attemptTimeout
		}
		status := existing.status
		s.mu.Unlock()
		writeJSON(writer, http.StatusOK, status)
		return
	}
	s.mu.Unlock()

	if operation.Operation == OperationRestore && operation.Phase == PhaseInCluster && len(operation.Configuration) > 0 {
		if s.settings == nil {
			writeProblem(writer, http.StatusServiceUnavailable, "configuration_unavailable", "in-cluster restore configuration is unavailable", true)
			return
		}
		if err := s.settings.AcceptRestoreConfiguration(request.Context(), operation.Configuration); err != nil {
			writeProblem(writer, http.StatusBadGateway, "configuration_failed", "restore configuration could not be applied", true)
			return
		}
		operation.Configuration = nil
	}

	s.mu.Lock()
	if existing := s.operations[operation.OperationID]; existing != nil {
		if !bytes.Equal(existing.requestJSON, encoded) {
			s.mu.Unlock()
			writeProblem(writer, http.StatusConflict, "operation_conflict", "operation ID already exists with different input", false)
			return
		}
		if operation.Deadline != nil && (existing.request.Deadline == nil || operation.Deadline.After(*existing.request.Deadline)) {
			extended := *operation.Deadline
			existing.request.Deadline = &extended
			existing.attemptTimeout = attemptTimeout
		}
		status := existing.status
		s.mu.Unlock()
		writeJSON(writer, http.StatusOK, status)
		return
	}
	configurationPath := ""
	if operation.Operation == OperationRestore && operation.Phase == PhaseBootstrap && len(operation.Configuration) > 0 {
		var err error
		configurationPath, err = s.stageConfiguration(operation.OperationID, operation.Configuration)
		if err != nil {
			s.mu.Unlock()
			writeProblem(writer, http.StatusInternalServerError, "configuration_write_failed", "could not stage restore configuration", true)
			return
		}
		operation.Configuration = nil
	}
	status := OperationStatus{APIVersion: APIVersion, OperationID: operation.OperationID, State: StatePending, UpdatedAt: s.now().UTC()}
	if operation.Operation == OperationRestore && operation.Phase == PhaseBootstrap && !operation.Headless {
		status.UI = &UI{Path: "/ui/restore?operation=" + operation.OperationID}
	}
	s.operations[operation.OperationID] = &operationRecord{
		request: operation, requestJSON: encoded, status: status,
		configurationPath: configurationPath, attemptTimeout: attemptTimeout,
	}
	s.mu.Unlock()
	writeJSON(writer, http.StatusCreated, status)
}

func (s *Server) uploadState(writer http.ResponseWriter, request *http.Request) {
	record := s.record(request.PathValue("id"))
	if record == nil || record.request.Operation != OperationBackup || record.request.StateArchive == nil {
		writeProblem(writer, http.StatusNotFound, "operation_not_found", "backup operation not found", false)
		return
	}
	descriptor := *record.request.StateArchive
	if request.ContentLength != descriptor.Size || !strings.EqualFold(request.Header.Get("X-EC-Artifact-SHA256"), descriptor.SHA256) {
		writeProblem(writer, http.StatusBadRequest, "artifact_mismatch", "state archive headers do not match the operation", false)
		return
	}
	directory := filepath.Join(s.tempRoot, record.request.OperationID)
	if err := os.MkdirAll(directory, 0700); err != nil {
		writeProblem(writer, http.StatusInternalServerError, "artifact_write_failed", "could not create artifact directory", true)
		return
	}
	path := filepath.Join(directory, "ec-state.tar.gz")
	temporary, err := os.CreateTemp(directory, "upload-*")
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "artifact_write_failed", "could not stage state archive", true)
		return
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	_ = temporary.Chmod(0600)
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(request.Body, descriptor.Size+1))
	closeErr := temporary.Close()
	if copyErr != nil || closeErr != nil || written != descriptor.Size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), descriptor.SHA256) {
		writeProblem(writer, http.StatusBadRequest, "artifact_mismatch", "state archive size or digest mismatch", false)
		return
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		writeProblem(writer, http.StatusInternalServerError, "artifact_write_failed", "could not commit state archive", true)
		return
	}
	s.mu.Lock()
	if current := s.operations[record.request.OperationID]; current != nil {
		current.statePath = path
	}
	s.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) runOperation(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	s.mu.Lock()
	record := s.operations[id]
	if record == nil {
		s.mu.Unlock()
		writeProblem(writer, http.StatusNotFound, "operation_not_found", "operation not found", false)
		return
	}
	if record.status.State == StateRunning || record.status.State == StateSucceeded {
		status := record.status
		s.mu.Unlock()
		writeJSON(writer, http.StatusOK, status)
		return
	}
	if record.status.State == StateCanceled {
		s.mu.Unlock()
		writeProblem(writer, http.StatusConflict, "operation_canceled", "operation has been canceled", false)
		return
	}
	if record.request.Operation == OperationBackup && record.statePath == "" {
		s.mu.Unlock()
		writeProblem(writer, http.StatusConflict, "state_archive_missing", "EC state archive has not been uploaded", true)
		return
	}
	if record.request.Operation == OperationRestore && record.request.Phase == PhaseBootstrap && (record.request.RecoveryPointID == "" || record.configurationPath == "") {
		status := record.status
		s.mu.Unlock()
		writeJSON(writer, http.StatusOK, status)
		return
	}
	ctx := context.Background()
	if record.attemptTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, record.attemptTimeout)
		record.cancel = cancel
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		record.cancel = cancel
	}
	record.status.State = StateRunning
	record.status.Progress = &Progress{Phase: "starting", Message: "Starting lifecycle operation"}
	record.status.Error = nil
	record.status.UpdatedAt = s.now().UTC()
	status := record.status
	operation := record.request
	if operation.Phase == PhaseBootstrap {
		configuration, err := os.ReadFile(record.configurationPath)
		if err != nil || len(configuration) == 0 || len(configuration) > MaxConfigurationSize {
			record.status.State = StateFailed
			record.status.Error = &Error{Code: "configuration_unavailable", Message: "restore configuration is unavailable", Retryable: true}
			record.status.UpdatedAt = s.now().UTC()
			status = record.status
			s.mu.Unlock()
			writeJSON(writer, http.StatusConflict, status)
			return
		}
		operation.Configuration = append(json.RawMessage(nil), configuration...)
	}
	statePath := record.statePath
	s.mu.Unlock()

	go s.execute(ctx, operation, statePath)
	writeJSON(writer, http.StatusAccepted, status)
}

func (s *Server) execute(ctx context.Context, operation OperationRequest, statePath string) {
	progress := func(update Progress) {
		s.mu.Lock()
		if record := s.operations[operation.OperationID]; record != nil && record.status.State == StateRunning {
			record.status.Progress = &update
			record.status.UpdatedAt = s.now().UTC()
		}
		s.mu.Unlock()
	}
	result, downloadPath, err := s.executor.Execute(ctx, operation, statePath, progress)
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.operations[operation.OperationID]
	if record == nil || record.status.State == StateCanceled {
		return
	}
	record.status.UpdatedAt = s.now().UTC()
	if err != nil {
		record.status.State = StateFailed
		record.status.Error = &Error{Code: "operation_failed", Message: err.Error(), Retryable: !errors.Is(err, context.Canceled)}
		return
	}
	record.status.State = StateSucceeded
	record.status.Result = result
	record.status.Progress = &Progress{Phase: "complete", Message: "Lifecycle operation complete", Percentage: 100}
	record.downloadPath = downloadPath
}

func (s *Server) getOperation(writer http.ResponseWriter, request *http.Request) {
	record := s.record(request.PathValue("id"))
	if record == nil {
		writeProblem(writer, http.StatusNotFound, "operation_not_found", "operation not found", false)
		return
	}
	writeJSON(writer, http.StatusOK, record.status)
}

func (s *Server) getEvents(writer http.ResponseWriter, request *http.Request) {
	record := s.record(request.PathValue("id"))
	if record == nil {
		writeProblem(writer, http.StatusNotFound, "operation_not_found", "operation not found", false)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	data, _ := json.Marshal(record.status)
	fmt.Fprintf(writer, "event: status\ndata: %s\n\n", data)
}

func (s *Server) downloadState(writer http.ResponseWriter, request *http.Request) {
	record := s.record(request.PathValue("id"))
	if record == nil || record.status.State != StateSucceeded || record.downloadPath == "" || record.status.Result == nil || record.status.Result.StateArchive == nil {
		writeProblem(writer, http.StatusConflict, "artifact_unavailable", "recovered EC state archive is not available", true)
		return
	}
	file, err := os.Open(record.downloadPath)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "artifact_read_failed", "could not read recovered EC state", true)
		return
	}
	defer file.Close()
	writer.Header().Set("Content-Type", "application/gzip")
	writer.Header().Set("Content-Length", fmt.Sprint(record.status.Result.StateArchive.Size))
	_, _ = io.Copy(writer, file)
}

func (s *Server) downloadConfiguration(writer http.ResponseWriter, request *http.Request) {
	record := s.record(request.PathValue("id"))
	if record == nil || record.request.Operation != OperationRestore || record.request.Phase != PhaseBootstrap || record.status.State != StateSucceeded || record.configurationPath == "" {
		writeProblem(writer, http.StatusConflict, "configuration_unavailable", "restore configuration is not available", true)
		return
	}
	configuration, err := os.ReadFile(record.configurationPath)
	if err != nil || len(configuration) == 0 || len(configuration) > MaxConfigurationSize {
		writeProblem(writer, http.StatusInternalServerError, "configuration_read_failed", "could not read restore configuration", true)
		return
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(configuration, &object); err != nil || object == nil {
		writeProblem(writer, http.StatusInternalServerError, "configuration_read_failed", "restore configuration is invalid", false)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Content-Length", fmt.Sprint(len(configuration)))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(configuration)
}

func (s *Server) cancelOperation(writer http.ResponseWriter, request *http.Request) {
	s.mu.Lock()
	record := s.operations[request.PathValue("id")]
	if record == nil {
		s.mu.Unlock()
		writeProblem(writer, http.StatusNotFound, "operation_not_found", "operation not found", false)
		return
	}
	if record.cancel != nil {
		record.cancel()
	}
	record.status.State = StateCanceled
	record.status.UpdatedAt = s.now().UTC()
	s.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) listRecoveryPoints(writer http.ResponseWriter, request *http.Request) {
	if s.record(request.PathValue("id")) == nil {
		writeProblem(writer, http.StatusNotFound, "operation_not_found", "operation not found", false)
		return
	}
	configuration, err := readConfiguration(request)
	if err != nil {
		writeProblem(writer, http.StatusBadRequest, "invalid_configuration", err.Error(), false)
		return
	}
	points, err := s.executor.ListRecoveryPoints(request.Context(), configuration)
	if err != nil {
		writeProblem(writer, http.StatusBadGateway, "storage_unavailable", err.Error(), true)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"recoveryPoints": points})
}

func (s *Server) selectRecoveryPoint(writer http.ResponseWriter, request *http.Request) {
	var selection struct {
		RecoveryPointID string          `json:"recoveryPointId"`
		Configuration   json.RawMessage `json:"configuration"`
	}
	if err := decodeJSON(request, &selection); err != nil || selection.RecoveryPointID == "" || len(selection.Configuration) == 0 {
		writeProblem(writer, http.StatusBadRequest, "invalid_selection", "recoveryPointId and configuration are required", false)
		return
	}
	id := request.PathValue("id")
	s.mu.Lock()
	record := s.operations[id]
	if record == nil || record.request.Operation != OperationRestore || record.request.Phase != PhaseBootstrap {
		s.mu.Unlock()
		writeProblem(writer, http.StatusNotFound, "operation_not_found", "bootstrap restore operation not found", false)
		return
	}
	if record.status.State != StatePending {
		s.mu.Unlock()
		writeProblem(writer, http.StatusConflict, "operation_started", "the bootstrap restore selection is already committed", false)
		return
	}
	selected := record.request
	selected.RecoveryPointID = selection.RecoveryPointID
	selected.Configuration = append(json.RawMessage(nil), selection.Configuration...)
	selected.Headless = true
	if err := selected.Validate(); err != nil {
		s.mu.Unlock()
		writeProblem(writer, http.StatusBadRequest, "invalid_selection", err.Error(), false)
		return
	}
	configurationPath, err := s.stageConfiguration(id, selected.Configuration)
	if err != nil {
		s.mu.Unlock()
		writeProblem(writer, http.StatusInternalServerError, "configuration_write_failed", "could not stage restore configuration", true)
		return
	}
	selected.Configuration = nil
	record.request = selected
	record.configurationPath = configurationPath
	s.mu.Unlock()
	s.runOperation(writer, request)
}

func (s *Server) stageConfiguration(operationID string, configuration json.RawMessage) (string, error) {
	directory := filepath.Join(s.tempRoot, operationID)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(directory, "configuration-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(configuration); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	path := filepath.Join(directory, "configuration.json")
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Server) record(id string) *operationRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record := s.operations[id]
	if record == nil {
		return nil
	}
	copy := *record
	return &copy
}

func decodeJSON(request *http.Request, destination any) error {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return fmt.Errorf("Content-Type must be application/json")
	}
	limited := http.MaxBytesReader(nil, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

func canonicalRequest(request OperationRequest) ([]byte, error) {
	copy := request
	// Deadlines control a particular attempt and may be safely extended when EC
	// retries the same stable operation ID.
	copy.Deadline = nil
	if len(copy.Configuration) > 0 {
		digest := sha256.Sum256(copy.Configuration)
		copy.Configuration = json.RawMessage(fmt.Sprintf("%q", hex.EncodeToString(digest[:])))
	}
	return json.Marshal(copy)
}

func readConfiguration(request *http.Request) (json.RawMessage, error) {
	var body struct {
		Configuration json.RawMessage `json:"configuration"`
	}
	if err := decodeJSON(request, &body); err != nil {
		return nil, err
	}
	if len(body.Configuration) == 0 || len(body.Configuration) > MaxConfigurationSize {
		return nil, fmt.Errorf("bounded configuration object is required")
	}
	return body.Configuration, nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeProblem(writer http.ResponseWriter, status int, code, message string, retryable bool) {
	writeJSON(writer, status, Error{Code: code, Message: message, Retryable: retryable})
}
