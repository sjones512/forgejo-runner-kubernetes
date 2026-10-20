package main

import (
	"strings"
	"testing"
)

func TestJobCPUAndMemoryEnvironment(t *testing.T) {
	for _, tc := range []struct{ cpuReq, cpuLim, memReq, memLim, want string }{
		{"", "", "", "", ""}, {"500m", "2", "512Mi", "2Gi", ""},
		{"none", "none", "none", "none", ""}, {"none", "1", "none", "2Gi", ""},
		{"bad", "", "", "", "JOB_CPU_REQUEST"}, {"", "0", "", "", "JOB_CPU_LIMIT"},
		{"", "", "1GiB", "", "JOB_MEMORY_REQUEST"}, {"", "", "2Gi", "1Gi", "JOB_MEMORY_REQUEST"},
	} {
		t.Run(tc.cpuReq+tc.cpuLim+tc.memReq+tc.memLim, func(t *testing.T) {
			t.Setenv("JOB_CPU_REQUEST", tc.cpuReq)
			t.Setenv("JOB_CPU_LIMIT", tc.cpuLim)
			t.Setenv("JOB_MEMORY_REQUEST", tc.memReq)
			t.Setenv("JOB_MEMORY_LIMIT", tc.memLim)
			cfg, err := jobConfig()
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatal("invalid startup", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CPURequest != tc.cpuReq || cfg.CPULimit != tc.cpuLim || cfg.MemoryRequest != tc.memReq || cfg.MemoryLimit != tc.memLim {
				t.Fatal("job env not applied")
			}
		})
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
		{"workspace cap only", "5Gi", "", "", ""},
		{"none", "none", "none", "none", ""},
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
