package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/protocol"
)

func TestVeleroBackupUsesVendorPolicyAndRequiresCleanCompletion(t *testing.T) {
	client := fakeClient(func(request *http.Request) *http.Response {
		switch request.Method {
		case http.MethodPost:
			var object map[string]any
			if err := json.NewDecoder(request.Body).Decode(&object); err != nil {
				t.Fatal(err)
			}
			spec := object["spec"].(map[string]any)
			if spec["defaultVolumesToFsBackup"] != true || spec["snapshotVolumes"] != false {
				t.Fatalf("unexpected volume policy: %#v", spec)
			}
			return jsonResponse(http.StatusCreated, `{}`)
		case http.MethodGet:
			return jsonResponse(http.StatusOK, `{"status":{"phase":"Completed","errors":0}}`)
		default:
			t.Fatalf("unexpected method %s", request.Method)
		}
		return nil
	})
	velero := NewVelero(client, "dr")
	velero.pollInterval = time.Millisecond
	err := velero.Backup(context.Background(), "backup", protocol.BackupPolicy{
		IncludedNamespaces: []string{"app"}, IncludedClusterResources: []string{"customresourcedefinitions.apiextensions.k8s.io"}, VolumeBackup: "fileSystem",
	}, func(protocol.Progress) {})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVeleroRestoreRejectsPartialFailure(t *testing.T) {
	client := fakeClient(func(request *http.Request) *http.Response {
		if request.Method == http.MethodPost {
			return jsonResponse(http.StatusCreated, `{}`)
		}
		return jsonResponse(http.StatusOK, `{"status":{"phase":"PartiallyFailed","errors":1,"failureReason":"volume failed"}}`)
	})
	velero := NewVelero(client, "dr")
	velero.pollInterval = time.Millisecond
	if err := velero.Restore(context.Background(), "restore", "backup", func(protocol.Progress) {}); err == nil {
		t.Fatal("expected partial restore to fail")
	}
}

func TestReviewTokenRequiresAudienceAndServiceAccount(t *testing.T) {
	client := fakeClient(func(request *http.Request) *http.Response {
		if request.Header.Get("Authorization") != "Bearer reviewer-token" {
			t.Fatalf("unexpected reviewer credential")
		}
		var review map[string]any
		if err := json.NewDecoder(request.Body).Decode(&review); err != nil {
			t.Fatal(err)
		}
		return jsonResponse(http.StatusOK, `{"status":{"authenticated":true,"audiences":["lifecycle"],"user":{"username":"system:serviceaccount:dr:handler"}}}`)
	})
	if err := client.ReviewToken(context.Background(), "caller-token", "lifecycle", "system:serviceaccount:dr:handler"); err != nil {
		t.Fatal(err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func fakeClient(handler func(*http.Request) *http.Response) *Client {
	return &Client{
		baseURL: "https://kubernetes.test", token: "reviewer-token",
		httpClient: &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			return handler(request), nil
		})},
	}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}
