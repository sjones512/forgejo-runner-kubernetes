package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	"forgejo-runner-kubernetes/internal/plugin"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func jobConfig() (plugin.Config, error) {
	dindEnabled := os.Getenv("JOB_DIND_ENABLED")
	if dindEnabled != "" && dindEnabled != "false" && dindEnabled != "true" {
		return plugin.Config{}, fmt.Errorf("JOB_DIND_ENABLED: use true or false (default false)")
	}
	cfg := plugin.Config{
		Namespace: env("JOB_NAMESPACE", "forgejo-jobs"), Image: env("JOB_IMAGE", "ubuntu:24.04"), Arch: env("JOB_ARCH", "arm64"),
		StartupTimeout: 3 * time.Minute, CleanupTimeout: 30 * time.Second,
		WorkspaceSizeLimit: os.Getenv("JOB_WORKSPACE_SIZE_LIMIT"), EphemeralStorageRequest: os.Getenv("JOB_EPHEMERAL_STORAGE_REQUEST"), EphemeralStorageLimit: os.Getenv("JOB_EPHEMERAL_STORAGE_LIMIT"),
		CPURequest: os.Getenv("JOB_CPU_REQUEST"), CPULimit: os.Getenv("JOB_CPU_LIMIT"),
		MemoryRequest: os.Getenv("JOB_MEMORY_REQUEST"), MemoryLimit: os.Getenv("JOB_MEMORY_LIMIT"),
		AppArmorProfile: os.Getenv("JOB_APPARMOR_PROFILE"),
		DinD: plugin.DinDConfig{
			Enabled: dindEnabled == "true", Image: os.Getenv("JOB_DIND_IMAGE"),
			CPURequest: os.Getenv("JOB_DIND_CPU_REQUEST"), CPULimit: os.Getenv("JOB_DIND_CPU_LIMIT"),
			MemoryRequest: os.Getenv("JOB_DIND_MEMORY_REQUEST"), MemoryLimit: os.Getenv("JOB_DIND_MEMORY_LIMIT"),
			EphemeralStorageRequest: os.Getenv("JOB_DIND_EPHEMERAL_STORAGE_REQUEST"), EphemeralStorageLimit: os.Getenv("JOB_DIND_EPHEMERAL_STORAGE_LIMIT"),
			DataSizeLimit: os.Getenv("JOB_DIND_DATA_SIZE_LIMIT"), SocketSizeLimit: os.Getenv("JOB_DIND_SOCKET_SIZE_LIMIT"), StorageDriver: os.Getenv("JOB_DIND_STORAGE_DRIVER"),
		},
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("job configuration: %w", err)
	}
	return cfg, nil
}
func run() error {
	cfg, err := jobConfig()
	if err != nil {
		return err
	}
	rc, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster Kubernetes credentials: %w", err)
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	server, err := plugin.New(cfg, client, rc)
	if err != nil {
		return err
	}
	addr := env("PLUGIN_LISTEN_ADDR", "0.0.0.0:50051")
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	grpcServer := plugin.NewGRPCServer()
	pb.RegisterBackendPluginServer(grpcServer, server)
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
		grpcServer.GracefulStop()
	}()
	log.Printf("listening on %s, namespace %s", lis.Addr(), cfg.Namespace)
	return grpcServer.Serve(lis)
}
func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
