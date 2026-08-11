package kubernetes

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const serviceAccountRoot = "/var/run/secrets/kubernetes.io/serviceaccount"

type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Kubernetes API returned HTTP %d: %s", e.StatusCode, e.Message)
}

func NewInClusterClient() (*Client, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("Kubernetes service environment is unavailable")
	}
	token, err := os.ReadFile(serviceAccountRoot + "/token")
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}
	ca, err := os.ReadFile(serviceAccountRoot + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("parse Kubernetes CA")
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}
	return &Client{
		baseURL:    "https://" + host + ":" + port,
		token:      strings.TrimSpace(string(token)),
		httpClient: &http.Client{Transport: transport, Timeout: 5 * time.Minute},
	}, nil
}

func (c *Client) Do(ctx context.Context, method, path string, body, destination any) (int, error) {
	var encoded io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshal Kubernetes request: %w", err)
		}
		encoded = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, encoded)
	if err != nil {
		return 0, fmt.Errorf("create Kubernetes request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return 0, fmt.Errorf("request Kubernetes API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		return response.StatusCode, &APIError{StatusCode: response.StatusCode, Message: strings.TrimSpace(string(message))}
	}
	if destination != nil && response.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(destination); err != nil {
			return response.StatusCode, fmt.Errorf("decode Kubernetes response: %w", err)
		}
	}
	return response.StatusCode, nil
}

func (c *Client) ReviewToken(ctx context.Context, token, audience, serviceAccount string) error {
	request := map[string]any{
		"apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview",
		"spec": map[string]any{"token": token, "audiences": []string{audience}},
	}
	var response struct {
		Status struct {
			Authenticated bool     `json:"authenticated"`
			Audiences     []string `json:"audiences"`
			User          struct {
				Username string `json:"username"`
			} `json:"user"`
			Error string `json:"error"`
		} `json:"status"`
	}
	if _, err := c.Do(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/tokenreviews", request, &response); err != nil {
		return err
	}
	if !response.Status.Authenticated || response.Status.Error != "" || response.Status.User.Username != serviceAccount {
		return fmt.Errorf("lifecycle caller is not authenticated as the declared service account")
	}
	audienceFound := false
	for _, candidate := range response.Status.Audiences {
		if candidate == audience {
			audienceFound = true
			break
		}
	}
	if !audienceFound {
		return fmt.Errorf("lifecycle credential has the wrong audience")
	}
	return nil
}
