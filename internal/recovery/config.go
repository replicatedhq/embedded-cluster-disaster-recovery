package recovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Configuration struct {
	Storage     StorageConfiguration `json:"storage"`
	RecoveryKey string               `json:"recoveryKey"`
}

type StorageConfiguration struct {
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix,omitempty"`
	Region          string `json:"region,omitempty"`
	Endpoint        string `json:"endpoint,omitempty"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	ForcePathStyle  bool   `json:"forcePathStyle,omitempty"`
	CustomCAPEM     string `json:"customCaPem,omitempty"`
	ProxyURL        string `json:"proxyUrl,omitempty"`
}

func ParseConfiguration(data json.RawMessage) (Configuration, error) {
	var configuration Configuration
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&configuration); err != nil {
		return Configuration{}, fmt.Errorf("decode lifecycle configuration: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Configuration{}, fmt.Errorf("decode lifecycle configuration: expected one JSON object")
		}
		return Configuration{}, fmt.Errorf("decode lifecycle configuration: %w", err)
	}
	if err := configuration.Validate(); err != nil {
		return Configuration{}, err
	}
	return configuration, nil
}

func ConfigurationFromEnvironment() (Configuration, error) {
	forcePathStyle, err := strconv.ParseBool(envDefault("DR_FORCE_PATH_STYLE", "true"))
	if err != nil {
		return Configuration{}, fmt.Errorf("DR_FORCE_PATH_STYLE must be true or false")
	}
	configuration := Configuration{
		Storage: StorageConfiguration{
			Bucket: os.Getenv("DR_BUCKET"), Prefix: os.Getenv("DR_PREFIX"),
			Region: envDefault("DR_REGION", "auto"), Endpoint: os.Getenv("DR_ENDPOINT"),
			AccessKeyID: os.Getenv("DR_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("DR_SECRET_ACCESS_KEY"),
			ForcePathStyle: forcePathStyle, CustomCAPEM: os.Getenv("DR_CUSTOM_CA_PEM"), ProxyURL: os.Getenv("DR_PROXY_URL"),
		},
		RecoveryKey: os.Getenv("DR_RECOVERY_KEY"),
	}
	if err := configuration.Validate(); err != nil {
		return Configuration{}, fmt.Errorf("in-cluster recovery configuration: %w", err)
	}
	return configuration, nil
}

func (c Configuration) Validate() error {
	if strings.TrimSpace(c.Storage.Bucket) == "" || strings.ContainsAny(c.Storage.Bucket, "\r\n") {
		return fmt.Errorf("storage.bucket is required")
	}
	if c.Storage.AccessKeyID == "" || c.Storage.SecretAccessKey == "" {
		return fmt.Errorf("storage accessKeyId and secretAccessKey are required")
	}
	if strings.ContainsAny(c.Storage.AccessKeyID+c.Storage.SecretAccessKey, "\r\n") {
		return fmt.Errorf("storage credentials must not contain line breaks")
	}
	if len(c.RecoveryKey) < 16 {
		return fmt.Errorf("recoveryKey must contain at least 16 characters")
	}
	if strings.ContainsAny(c.RecoveryKey, "\r\n") {
		return fmt.Errorf("recoveryKey must not contain line breaks")
	}
	if c.Storage.Endpoint != "" {
		parsed, err := url.Parse(c.Storage.Endpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("storage.endpoint must be an HTTPS origin")
		}
	}
	if len(c.Storage.CustomCAPEM) > 256<<10 {
		return fmt.Errorf("storage.customCaPem exceeds 256 KiB")
	}
	if c.Storage.ProxyURL != "" {
		parsed, err := url.Parse(c.Storage.ProxyURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("storage.proxyUrl must be an HTTP or HTTPS origin")
		}
	}
	return nil
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
