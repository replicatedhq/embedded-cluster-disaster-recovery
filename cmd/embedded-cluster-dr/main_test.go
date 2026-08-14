package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeHandlerDoesNotCollideWithBootstrapExecutable(t *testing.T) {
	tempDir, err := os.MkdirTemp("/tmp", "ecdr-bootstrap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("DR_TEMP_ROOT", "")
	t.Setenv("EC_LIFECYCLE_SOCKET", filepath.Join(tempDir, "handler.sock"))
	t.Setenv("EC_LIFECYCLE_TOKEN", "01234567890123456789012345678901")

	// EC extracts the declared executable at this exact path before starting it.
	executable := filepath.Join(tempDir, "embedded-cluster-dr")
	if err := os.WriteFile(executable, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}

	listener, _, err := runtimeHandler()
	if err != nil {
		t.Fatalf("runtimeHandler: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	info, err := os.Stat(filepath.Join(tempDir, defaultTempDirName, "operations"))
	if err != nil {
		t.Fatalf("stat workflow operation directory: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("workflow operation path is not a directory")
	}
}
