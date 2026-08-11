package recovery

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseConfiguration(t *testing.T) {
	data := json.RawMessage(`{"storage":{"bucket":"backup","region":"auto","endpoint":"https://example.r2.cloudflarestorage.com","accessKeyId":"access","secretAccessKey":"secret","forcePathStyle":true},"recoveryKey":"0123456789abcdef"}`)
	configuration, err := ParseConfiguration(data)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Storage.Bucket != "backup" || configuration.RecoveryKey != "0123456789abcdef" {
		t.Fatalf("unexpected configuration: %#v", configuration)
	}
}

func TestConfigurationRejectsUnsafeEndpoints(t *testing.T) {
	configuration := Configuration{
		Storage:     StorageConfiguration{Bucket: "backup", AccessKeyID: "access", SecretAccessKey: "secret", Endpoint: "http://object-store.example"},
		RecoveryKey: "0123456789abcdef",
	}
	if err := configuration.Validate(); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("expected HTTPS validation error, got %v", err)
	}
}

func TestManifestValidate(t *testing.T) {
	id := "e5bda050-2942-4c69-a485-a92c11b1621a"
	manifest := Manifest{
		FormatVersion: RecoveryFormatVersion, RecoveryPointID: id, CreatedAt: time.Now(),
		VeleroBackupName: "ec-dr-e5bda05029424c69a485a92c11b1621a",
		StateObjectKey:   "recovery-points/" + id + "/ec-state-v1.tar.gz.age",
		Status:           "ready",
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	manifest.StateObjectKey = "../another-object"
	if err := manifest.Validate(); err == nil {
		t.Fatal("expected invalid state object key to be rejected")
	}
}
