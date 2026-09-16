package protocol

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBackupUIRendersBasePaths(t *testing.T) {
	tests := []struct {
		name            string
		forwardedPrefix string
		extensionBase   string
		consoleBase     string
	}{
		{
			name:            "admin console proxy",
			forwardedPrefix: "/api/console/disaster-recovery/extension",
			extensionBase:   "/api/console/disaster-recovery/extension",
			consoleBase:     "/api/console/disaster-recovery",
		},
		{
			name:          "no prefix",
			extensionBase: "",
			consoleBase:   "/console/disaster-recovery",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, err := NewServer(&fakeExecutor{}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			server.SetSettings(&fakeSettings{})

			request := httptest.NewRequest(http.MethodGet, "/ui/backup", nil)
			request.Header.Set("X-Forwarded-Prefix", test.forwardedPrefix)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
			body := response.Body.String()
			for _, declaration := range []string{
				`const extensionBase = "` + test.extensionBase + `";`,
				`const consoleBase = "` + test.consoleBase + `";`,
			} {
				if !strings.Contains(body, declaration) {
					t.Errorf("response does not contain %q", declaration)
				}
			}
			for _, doubleEncoded := range []string{
				`"\"` + test.extensionBase + `\""`,
				`"\"` + test.consoleBase + `\""`,
			} {
				if strings.Contains(body, doubleEncoded) {
					t.Errorf("response contains double-encoded value %q", doubleEncoded)
				}
			}
		})
	}
}

func TestRestoreUIRendersOperationID(t *testing.T) {
	server, err := NewServer(&fakeExecutor{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	operation := OperationRequest{
		APIVersion: APIVersion, OperationID: serverTestOperationID,
		Operation: OperationRestore, Phase: PhaseBootstrap,
	}
	if response := performJSON(server.Handler(), http.MethodPost, "/v1/operations", operation); response.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", response.Code, response.Body.String())
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/restore?operation="+serverTestOperationID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	want := `const operationID = "` + serverTestOperationID + `";`
	if !strings.Contains(body, want) {
		t.Fatalf("response does not contain %q", want)
	}
	if doubleEncoded := `"\"` + serverTestOperationID + `\""`; strings.Contains(body, doubleEncoded) {
		t.Fatalf("response contains double-encoded operation ID %q", doubleEncoded)
	}
}

func TestUITemplatesContextuallyEscapeJavaScriptValues(t *testing.T) {
	malicious := `"</script><script>alert('injected')</script>`
	tests := []struct {
		name     string
		render   func(*bytes.Buffer) error
		variable string
	}{
		{
			name: "backup",
			render: func(output *bytes.Buffer) error {
				return backupTemplate.Execute(output, map[string]string{
					"Nonce": "nonce", "ExtensionBase": malicious, "ConsoleBase": malicious,
				})
			},
			variable: "extensionBase",
		},
		{
			name: "restore",
			render: func(output *bytes.Buffer) error {
				return restoreTemplate.Execute(output, map[string]string{"Nonce": "nonce", "OperationID": malicious})
			},
			variable: "operationID",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := test.render(&output); err != nil {
				t.Fatal(err)
			}
			body := output.String()
			if strings.Contains(body, malicious) || strings.Contains(body, `<script>alert('injected')</script>`) {
				t.Fatal("JavaScript value was rendered as executable markup")
			}
			if !strings.Contains(body, `const `+test.variable+` = "`) {
				t.Fatalf("%s was not rendered as a JavaScript string", test.variable)
			}
			if !strings.Contains(body, `\u003c/script\u003e`) {
				t.Fatal("script-closing text was not contextually escaped")
			}
		})
	}
}
