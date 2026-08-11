package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/auth"
	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/kubernetes"
	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/protocol"
	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/recovery"
	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/settings"
	"github.com/replicatedhq/embedded-cluster-disaster-recovery/internal/workflow"
)

const lifecycleAudience = "embedded-cluster-lifecycle"

func main() {
	if err := run(); err != nil {
		log.Printf("embedded-cluster-dr: %v", err)
		os.Exit(1)
	}
}

func run() error {
	if protocolVersion := os.Getenv("EC_LIFECYCLE_PROTOCOL"); protocolVersion != "" && protocolVersion != protocol.APIVersion {
		return fmt.Errorf("unsupported lifecycle protocol %q", protocolVersion)
	}
	_ = syscall.Umask(0077)

	listener, handler, err := runtimeHandler()
	if err != nil {
		return err
	}
	defer listener.Close()

	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute,
		WriteTimeout: 5 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func runtimeHandler() (net.Listener, http.Handler, error) {
	socketPath := os.Getenv("EC_LIFECYCLE_SOCKET")
	bootstrap := socketPath != ""
	tempRoot := envDefault("DR_TEMP_ROOT", filepath.Join(os.TempDir(), "embedded-cluster-dr"))

	var application workflow.ApplicationBackup
	var reviewer auth.TokenReviewer
	var configurationStore workflow.ConfigurationStore
	if !bootstrap {
		client, err := kubernetes.NewInClusterClient()
		if err != nil {
			return nil, nil, err
		}
		application = kubernetes.NewVelero(client, envDefault("DR_VELERO_NAMESPACE", "embedded-cluster-dr"))
		reviewer = client
		configurationStore, err = settings.NewStore(client, envDefault("POD_NAMESPACE", "embedded-cluster-dr"))
		if err != nil {
			return nil, nil, err
		}
	}
	executor, err := workflow.NewExecutor(func(ctx context.Context, configuration recovery.StorageConfiguration) (workflow.Store, error) {
		return recovery.NewObjectStore(ctx, configuration)
	}, application, tempRoot)
	if err != nil {
		return nil, nil, err
	}
	executor.SetConfigurationStore(configurationStore)
	protocolServer, err := protocol.NewServer(executor, filepath.Join(tempRoot, "operations"))
	if err != nil {
		return nil, nil, err
	}
	if manager, ok := configurationStore.(protocol.SettingsManager); ok {
		protocolServer.SetSettings(manager)
	}
	protected := protocolServer.Handler()

	var listener net.Listener
	if bootstrap {
		if !filepath.IsAbs(socketPath) {
			return nil, nil, fmt.Errorf("EC_LIFECYCLE_SOCKET must be absolute")
		}
		token := os.Getenv("EC_LIFECYCLE_TOKEN")
		if len(token) < 32 || token != strings.TrimSpace(token) {
			return nil, nil, fmt.Errorf("EC_LIFECYCLE_TOKEN is missing or invalid")
		}
		listener, err = net.Listen("unix", socketPath)
		if err != nil {
			return nil, nil, fmt.Errorf("listen on lifecycle socket: %w", err)
		}
		if err := os.Chmod(socketPath, 0600); err != nil {
			listener.Close()
			return nil, nil, fmt.Errorf("secure lifecycle socket: %w", err)
		}
		protected = auth.Bootstrap(token, protected)
	} else {
		address := envDefault("DR_LISTEN_ADDRESS", ":8080")
		listener, err = net.Listen("tcp", address)
		if err != nil {
			return nil, nil, fmt.Errorf("listen on lifecycle service: %w", err)
		}
		namespace := envDefault("POD_NAMESPACE", "embedded-cluster-dr")
		serviceAccount := envDefault("DR_SERVICE_ACCOUNT", "embedded-cluster-dr")
		expectedIdentity := "system:serviceaccount:" + namespace + ":" + serviceAccount
		protected = auth.InCluster(reviewer, lifecycleAudience, expectedIdentity, protected)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })
	mux.Handle("/", protected)
	return listener, mux, nil
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
