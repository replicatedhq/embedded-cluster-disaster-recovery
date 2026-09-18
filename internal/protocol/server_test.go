package protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const serverTestOperationID = "e5bda050-2942-4c69-a485-a92c11b1621a"

type fakeExecutor struct {
	request   OperationRequest
	statePath string
	result    *OperationResult
	download  string
	err       error
}

type fakeSettings struct {
	accepted json.RawMessage
}

func (s *fakeSettings) ConfigurationStatus(context.Context) (any, error) {
	return map[string]any{}, nil
}
func (s *fakeSettings) Configure(context.Context, json.RawMessage) (string, bool, error) {
	return "", false, nil
}
func (s *fakeSettings) AcceptRestoreConfiguration(_ context.Context, configuration json.RawMessage) error {
	s.accepted = append(json.RawMessage(nil), configuration...)
	return nil
}

func (e *fakeExecutor) Execute(_ context.Context, request OperationRequest, statePath string, progress func(Progress)) (*OperationResult, string, error) {
	e.request = request
	e.statePath = statePath
	progress(Progress{Phase: "test", Message: "Testing", Percentage: 50})
	return e.result, e.download, e.err
}
func (e *fakeExecutor) ListRecoveryPoints(context.Context, json.RawMessage) ([]RecoveryPoint, error) {
	return []RecoveryPoint{{ID: serverTestOperationID, CreatedAt: time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)}}, nil
}
func (e *fakeExecutor) BackupSchedule(context.Context) (*BackupScheduleStatus, error) {
	return &BackupScheduleStatus{APIVersion: APIVersion}, nil
}

func TestBackupOperationRoundTrip(t *testing.T) {
	executor := &fakeExecutor{result: &OperationResult{RecoveryPointID: serverTestOperationID}}
	server, err := NewServer(executor, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive := []byte("opaque-ec-state")
	digest := sha256.Sum256(archive)
	request := OperationRequest{
		APIVersion: APIVersion, OperationID: serverTestOperationID, Operation: OperationBackup, Phase: PhaseInCluster,
		StateArchive: &ArtifactDescriptor{Name: "ec-state.tar.gz", Size: int64(len(archive)), SHA256: hex.EncodeToString(digest[:])},
		BackupPolicy: &BackupPolicy{IncludedNamespaces: []string{"app"}, VolumeBackup: "fileSystem"},
	}
	response := performJSON(server.Handler(), http.MethodPost, "/v1/operations", request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", response.Code, response.Body.String())
	}

	upload := httptest.NewRequest(http.MethodPut, "/v1/operations/"+serverTestOperationID+"/artifacts/ec-state", bytes.NewReader(archive))
	upload.ContentLength = int64(len(archive))
	upload.Header.Set("X-EC-Artifact-SHA256", hex.EncodeToString(digest[:]))
	uploadResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(uploadResponse, upload)
	if uploadResponse.Code != http.StatusNoContent {
		t.Fatalf("upload status = %d: %s", uploadResponse.Code, uploadResponse.Body.String())
	}

	run := httptest.NewRequest(http.MethodPost, "/v1/operations/"+serverTestOperationID+"/run", nil)
	runResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(runResponse, run)
	if runResponse.Code != http.StatusAccepted {
		t.Fatalf("run status = %d: %s", runResponse.Code, runResponse.Body.String())
	}
	status := waitForStatus(t, server.Handler(), StateSucceeded)
	if status.Result == nil || status.Result.RecoveryPointID != serverTestOperationID {
		t.Fatalf("unexpected status: %#v", status)
	}
	if executor.statePath == "" {
		t.Fatal("executor did not receive uploaded state archive")
	}
}

func TestBackupUIRendersBrowserProxyPathsAsJavaScriptStrings(t *testing.T) {
	server, err := NewServer(&fakeExecutor{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server.SetSettings(&fakeSettings{})
	request := httptest.NewRequest(http.MethodGet, "/ui/backup", nil)
	request.Header.Set("X-Forwarded-Prefix", "/api/console/disaster-recovery/extension")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("backup UI status = %d: %s", response.Code, response.Body.String())
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`const extensionBase = "/api/console/disaster-recovery/extension";`)) {
		t.Fatalf("backup UI did not render an executable extension base: %s", response.Body.String())
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`const consoleBase = "/api/console/disaster-recovery";`)) {
		t.Fatalf("backup UI did not render an executable console base: %s", response.Body.String())
	}
}

func TestRestoreUIRendersOperationIDAsJavaScriptString(t *testing.T) {
	server, err := NewServer(&fakeExecutor{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := OperationRequest{
		APIVersion: APIVersion, OperationID: serverTestOperationID, Operation: OperationRestore,
		Phase: PhaseBootstrap, RecoveryPointID: serverTestOperationID,
	}
	if response := performJSON(server.Handler(), http.MethodPost, "/v1/operations", request); response.Code != http.StatusCreated {
		t.Fatalf("create restore status = %d: %s", response.Code, response.Body.String())
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/restore?operation="+serverTestOperationID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("restore UI status = %d: %s", response.Code, response.Body.String())
	}
	want := []byte(`const operationID = "` + serverTestOperationID + `";`)
	if !bytes.Contains(response.Body.Bytes(), want) {
		t.Fatalf("restore UI did not render an executable operation ID: %s", response.Body.String())
	}
	if bytes.Contains(response.Body.Bytes(), []byte(`onclick=`)) ||
		!bytes.Contains(response.Body.Bytes(), []byte(`addEventListener('click', loadPoints)`)) {
		t.Fatalf("restore UI uses an inline event handler blocked by its CSP: %s", response.Body.String())
	}
}

func TestCreateIsIdempotentWithoutRetainingPlaintextConfigurationIdentity(t *testing.T) {
	executor := &fakeExecutor{}
	server, err := NewServer(executor, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Hour).UTC()
	request := OperationRequest{
		APIVersion: APIVersion, OperationID: serverTestOperationID, Operation: OperationRestore, Phase: PhaseBootstrap,
		RecoveryPointID: serverTestOperationID, Headless: true,
		Configuration: json.RawMessage(`{"storage":{"bucket":"one"}}`), Deadline: &deadline,
	}
	if response := performJSON(server.Handler(), http.MethodPost, "/v1/operations", request); response.Code != http.StatusCreated {
		t.Fatalf("first create status = %d", response.Code)
	}
	extended := deadline.Add(time.Hour)
	request.Deadline = &extended
	if response := performJSON(server.Handler(), http.MethodPost, "/v1/operations", request); response.Code != http.StatusOK {
		t.Fatalf("idempotent create status = %d", response.Code)
	}
	if got := server.operations[serverTestOperationID].request.Deadline; got == nil || !got.Equal(extended) {
		t.Fatalf("deadline was not extended: %v", got)
	}
	request.Configuration = json.RawMessage(`{"storage":{"bucket":"two"}}`)
	if response := performJSON(server.Handler(), http.MethodPost, "/v1/operations", request); response.Code != http.StatusConflict {
		t.Fatalf("conflicting create status = %d", response.Code)
	}
	if bytes.Contains(server.operations[serverTestOperationID].requestJSON, []byte("bucket")) {
		t.Fatalf("idempotency identity retained configuration: %s", server.operations[serverTestOperationID].requestJSON)
	}
}

func TestInClusterCreateDurablyAcceptsAndForgetsConfiguration(t *testing.T) {
	executor := &fakeExecutor{}
	settings := &fakeSettings{}
	server, err := NewServer(executor, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server.SetSettings(settings)
	configuration := json.RawMessage(`{"storage":{"bucket":"backup"},"recoveryKey":"protected-value"}`)
	request := OperationRequest{
		APIVersion: APIVersion, OperationID: serverTestOperationID, Operation: OperationRestore, Phase: PhaseInCluster,
		RecoveryPointID: serverTestOperationID, Configuration: configuration,
	}
	if response := performJSON(server.Handler(), http.MethodPost, "/v1/operations", request); response.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", response.Code, response.Body.String())
	}
	if !bytes.Equal(settings.accepted, configuration) {
		t.Fatalf("accepted configuration = %q", settings.accepted)
	}
	if len(server.operations[serverTestOperationID].request.Configuration) != 0 || bytes.Contains(server.operations[serverTestOperationID].requestJSON, []byte("protected-value")) {
		t.Fatal("operation retained plaintext restore configuration")
	}
}

func TestRecoveredStateDownload(t *testing.T) {
	directory := t.TempDir()
	download := filepath.Join(directory, "state.tar.gz")
	data := []byte("recovered")
	if err := os.WriteFile(download, data, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	executor := &fakeExecutor{
		result:   &OperationResult{RecoveryPointID: serverTestOperationID, StateArchive: &ArtifactDescriptor{Name: "ec-state-v1.tar.gz", Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}},
		download: download,
	}
	server, err := NewServer(executor, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configuration := json.RawMessage(`{"storage":{"bucket":"backup"}}`)
	request := OperationRequest{
		APIVersion: APIVersion, OperationID: serverTestOperationID, Operation: OperationRestore, Phase: PhaseBootstrap,
		RecoveryPointID: serverTestOperationID, Headless: true, Configuration: configuration,
	}
	if response := performJSON(server.Handler(), http.MethodPost, "/v1/operations", request); response.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", response.Code, response.Body.String())
	}
	runResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(runResponse, httptest.NewRequest(http.MethodPost, "/v1/operations/"+serverTestOperationID+"/run", nil))
	waitForStatus(t, server.Handler(), StateSucceeded)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/operations/"+serverTestOperationID+"/artifacts/ec-state", nil))
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), data) {
		t.Fatalf("download status = %d, body = %q", response.Code, response.Body.Bytes())
	}
	configurationResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(configurationResponse, httptest.NewRequest(http.MethodGet, "/v1/operations/"+serverTestOperationID+"/configuration", nil))
	if configurationResponse.Code != http.StatusOK || !bytes.Equal(configurationResponse.Body.Bytes(), configuration) {
		t.Fatalf("configuration status = %d, body = %q", configurationResponse.Code, configurationResponse.Body.Bytes())
	}
	if configurationResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("configuration cache control = %q", configurationResponse.Header().Get("Cache-Control"))
	}
	if !bytes.Equal(executor.request.Configuration, configuration) {
		t.Fatalf("executor configuration = %q", executor.request.Configuration)
	}
	if len(server.operations[serverTestOperationID].request.Configuration) != 0 {
		t.Fatal("operation record retained plaintext configuration")
	}
}

func TestJSONEndpointsRejectNonJSONAndTrailingInput(t *testing.T) {
	server, err := NewServer(&fakeExecutor{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/operations", bytes.NewBufferString(`{}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("non-JSON status = %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/operations", bytes.NewBufferString(`{} {}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status = %d", response.Code)
	}
}

func performJSON(handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	request := httptest.NewRequest(method, path, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func waitForStatus(t *testing.T, handler http.Handler, state State) OperationStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/operations/"+serverTestOperationID, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status request = %d", response.Code)
		}
		var status OperationStatus
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&status); err != nil {
			t.Fatal(err)
		}
		if status.State == state {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("operation did not reach %s", state)
	return OperationStatus{}
}
