package plugin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestNoDefaultResourceOrVolumeLimits(t *testing.T) {
	for _, dind := range []bool{false, true} {
		cfg := testConfig()
		if dind {
			cfg = dindConfig()
		}
		p := podSpec(podName("no-caps"), "no-caps", cfg.Image, cfg)
		for _, c := range p.Spec.Containers {
			if len(c.Resources.Limits) != 0 || len(c.Resources.Requests) != 3 {
				t.Fatal("default resource policy", c.Name, c.Resources)
			}
		}
		for _, v := range p.Spec.Volumes {
			if v.EmptyDir == nil || v.EmptyDir.SizeLimit != nil {
				t.Fatal("default volume cap", v.Name)
			}
		}
		data, err := json.Marshal(p.Spec)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"limits"`) || strings.Contains(string(data), `"sizeLimit"`) {
			t.Fatal("serialized default cap", string(data))
		}
		// Empty and explicit none must produce exactly the same intended template.
		cfg.CPULimit = "none"
		cfg.MemoryLimit = "none"
		cfg.EphemeralStorageLimit = "none"
		cfg.WorkspaceSizeLimit = "none"
		cfg.DinD.CPULimit = "none"
		cfg.DinD.MemoryLimit = "none"
		cfg.DinD.EphemeralStorageLimit = "none"
		cfg.DinD.DataSizeLimit = "none"
		cfg.DinD.SocketSizeLimit = "none"
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		q := podSpec(p.Name, "no-caps", cfg.Image, cfg)
		if !reflect.DeepEqual(p.Spec, q.Spec) || p.Annotations[jobTemplate] != q.Annotations[jobTemplate] || p.Annotations[dindTemplate] != q.Annotations[dindTemplate] {
			t.Fatal("none changes omitted policy/hash")
		}
	}
}

func TestAllResourceAndVolumeLimitsOperatorConfigured(t *testing.T) {
	cfg := dindConfig()
	cfg.CPULimit = "2"
	cfg.MemoryLimit = "2Gi"
	cfg.EphemeralStorageLimit = "4Gi"
	cfg.WorkspaceSizeLimit = "2Gi"
	cfg.DinD.CPULimit = "4"
	cfg.DinD.MemoryLimit = "4Gi"
	cfg.DinD.EphemeralStorageLimit = "8Gi"
	cfg.DinD.DataSizeLimit = "6Gi"
	cfg.DinD.SocketSizeLimit = "2Mi"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	p := podSpec(podName("all-caps"), "all-caps", cfg.Image, cfg)
	for i, want := range []map[core.ResourceName]string{
		{core.ResourceCPU: "2", core.ResourceMemory: "2Gi", core.ResourceEphemeralStorage: "4Gi"},
		{core.ResourceCPU: "4", core.ResourceMemory: "4Gi", core.ResourceEphemeralStorage: "8Gi"},
	} {
		got := p.Spec.Containers[i].Resources.Limits
		if len(got) != len(want) {
			t.Fatal("limits not applied", got)
		}
		for key, value := range want {
			q, ok := got[key]
			if !ok || q.Cmp(resource.MustParse(value)) != 0 {
				t.Fatal("limit not applied", key, q.String(), value)
			}
		}
	}
	for _, v := range p.Spec.Volumes {
		want := map[string]string{"workspace": "2Gi", "docker-data": "6Gi", "docker-socket": "2Mi"}[v.Name]
		if want == "" || v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.Cmp(resource.MustParse(want)) != 0 {
			t.Fatal("volume cap not applied", v.Name)
		}
	}
}

func TestStorageCapsAreIndependentOptIns(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.WorkspaceSizeLimit = "5Gi" },
		func(c *Config) { c.EphemeralStorageLimit = "2Gi" },
		func(c *Config) { c.DinD.DataSizeLimit = "20Gi" },
		func(c *Config) { c.DinD.SocketSizeLimit = "2Mi" },
		func(c *Config) { c.DinD.EphemeralStorageLimit = "12Gi" },
	} {
		cfg := dindConfig()
		change(&cfg)
		if err := cfg.Validate(); err != nil {
			t.Fatal("single explicit cap incorrectly requires others", err)
		}
		p := podSpec(podName("partial-cap"), "partial-cap", cfg.Image, cfg)
		caps := 0
		for _, c := range p.Spec.Containers {
			caps += len(c.Resources.Limits)
		}
		for _, v := range p.Spec.Volumes {
			if v.EmptyDir.SizeLimit != nil {
				caps++
			}
		}
		if caps != 1 {
			t.Fatal("single explicit cap injected other caps", caps)
		}
	}
}
