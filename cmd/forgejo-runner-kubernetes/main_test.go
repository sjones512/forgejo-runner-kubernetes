package main

import (
	"strings"
	"testing"
)

func TestJobIdentityEnvironment(t *testing.T) {
	pinned := "busybox@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct{ profile, helper, want string }{
		{"", "", ""}, {"fixed", "", ""}, {"image", pinned, ""}, {"image-ci", pinned, ""},
		{"unknown", "", "JOB_SECURITY_PROFILE"}, {"image", "", "JOB_PERMISSIONS_IMAGE"},
		{"image-ci", "busybox:1.37", "JOB_PERMISSIONS_IMAGE"}, {"fixed", pinned, "JOB_PERMISSIONS_IMAGE"},
	} {
		t.Run(tc.profile+tc.want, func(t *testing.T) {
			t.Setenv("JOB_SECURITY_PROFILE", tc.profile)
			t.Setenv("JOB_PERMISSIONS_IMAGE", tc.helper)
			cfg, err := jobConfig()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if err == nil && (cfg.SecurityProfile != tc.profile || cfg.PermissionsImage != tc.helper) {
				t.Fatal("identity environment not applied")
			}
		})
	}
}

func TestJobDinDEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"disabled default", nil, ""},
		{"explicitly disabled", map[string]string{"JOB_DIND_ENABLED": "false"}, ""},
		{"enabled pinned image", map[string]string{"JOB_DIND_ENABLED": "true", "JOB_DIND_IMAGE": "docker@sha256:" + strings.Repeat("a", 64)}, ""},
		{"invalid boolean", map[string]string{"JOB_DIND_ENABLED": "yes"}, "JOB_DIND_ENABLED"},
		{"missing image", map[string]string{"JOB_DIND_ENABLED": "true"}, "JOB_DIND_IMAGE"},
		{"mutable image", map[string]string{"JOB_DIND_ENABLED": "true", "JOB_DIND_IMAGE": "docker:29-dind"}, "JOB_DIND_IMAGE"},
		{"invalid resource while disabled", map[string]string{"JOB_DIND_MEMORY_REQUEST": "0"}, "JOB_DIND_MEMORY_REQUEST"},
		{"reversed data/storage budget", map[string]string{"JOB_DIND_DATA_SIZE_LIMIT": "12Gi"}, "JOB_DIND_DATA_SIZE_LIMIT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{"JOB_DIND_ENABLED", "JOB_DIND_IMAGE", "JOB_DIND_CPU_REQUEST", "JOB_DIND_CPU_LIMIT", "JOB_DIND_MEMORY_REQUEST", "JOB_DIND_MEMORY_LIMIT", "JOB_DIND_EPHEMERAL_STORAGE_REQUEST", "JOB_DIND_EPHEMERAL_STORAGE_LIMIT", "JOB_DIND_DATA_SIZE_LIMIT", "JOB_DIND_STORAGE_DRIVER"} {
				t.Setenv(key, tc.env[key])
			}
			cfg, err := jobConfig()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if err == nil && (cfg.DinD.Enabled != (tc.env["JOB_DIND_ENABLED"] == "true") || cfg.DinD.Image != tc.env["JOB_DIND_IMAGE"]) {
				t.Fatal("environment not applied", cfg.DinD)
			}
		})
	}
	t.Setenv("JOB_DIND_ENABLED", "true")
	t.Setenv("JOB_DIND_IMAGE", "docker@sha256:"+strings.Repeat("a", 64))
	t.Setenv("JOB_DIND_CPU_REQUEST", "500m")
	t.Setenv("JOB_DIND_CPU_LIMIT", "4")
	t.Setenv("JOB_DIND_MEMORY_REQUEST", "1Gi")
	t.Setenv("JOB_DIND_MEMORY_LIMIT", "4Gi")
	t.Setenv("JOB_DIND_EPHEMERAL_STORAGE_REQUEST", "3Gi")
	t.Setenv("JOB_DIND_EPHEMERAL_STORAGE_LIMIT", "25Gi")
	t.Setenv("JOB_DIND_DATA_SIZE_LIMIT", "20Gi")
	t.Setenv("JOB_DIND_STORAGE_DRIVER", "vfs")
	cfg, err := jobConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.DinD
	if d.CPURequest != "500m" || d.CPULimit != "4" || d.MemoryRequest != "1Gi" || d.MemoryLimit != "4Gi" || d.EphemeralStorageRequest != "3Gi" || d.EphemeralStorageLimit != "25Gi" || d.DataSizeLimit != "20Gi" || d.StorageDriver != "vfs" {
		t.Fatal("resource/driver environment not applied", d)
	}
}

func TestJobAppArmorEnvironment(t *testing.T) {
	for _, tc := range []struct {
		value, wantError string
	}{
		{"", ""},
		{"runtime-default", ""},
		{"unconfined", ""},
		{"localhost:ci-jobs", ""},
		{"localhost:", "JOB_APPARMOR_PROFILE"},
		{"RuntimeDefault", "JOB_APPARMOR_PROFILE"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("JOB_APPARMOR_PROFILE", tc.value)
			cfg, err := jobConfig()
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("setting %q: want error containing %q, got %v", tc.value, tc.wantError, err)
			}
			if err == nil && cfg.AppArmorProfile != tc.value {
				t.Fatalf("AppArmor setting not applied: %+v", cfg)
			}
		})
	}
}

func TestJobStorageEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, workspace, request, limit, want string
	}{
		{"defaults", "", "", "", ""},
		{"valid override", "5Gi", "3Gi", "7Gi", ""},
		{"invalid quantity", "5GiB", "", "", "JOB_WORKSPACE_SIZE_LIMIT"},
		{"invalid relationship", "5Gi", "", "2Gi", "JOB_WORKSPACE_SIZE_LIMIT"},
		{"invalid request", "", "3Gi", "2Gi", "JOB_EPHEMERAL_STORAGE_REQUEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("JOB_WORKSPACE_SIZE_LIMIT", tc.workspace)
			t.Setenv("JOB_EPHEMERAL_STORAGE_REQUEST", tc.request)
			t.Setenv("JOB_EPHEMERAL_STORAGE_LIMIT", tc.limit)
			cfg, err := jobConfig()
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("want error containing %q, got %v", tc.want, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.WorkspaceSizeLimit != tc.workspace || cfg.EphemeralStorageRequest != tc.request || cfg.EphemeralStorageLimit != tc.limit {
				t.Fatalf("environment not applied: %+v", cfg)
			}
		})
	}
}
