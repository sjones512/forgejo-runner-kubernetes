package plugin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	pb "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestJobResourcePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, cpuReq, cpuLim, memReq, memLim           string
		wantCPUReq, wantCPULim, wantMemReq, wantMemLim string
	}{
		{"default", "", "", "", "", "100m", "", "128Mi", ""},
		{"CPU request", "500m", "", "", "", "500m", "", "128Mi", ""},
		{"CPU ceiling", "", "2", "", "", "100m", "2", "128Mi", ""},
		{"independent CPU pair", "500m", "4", "", "", "500m", "4", "128Mi", ""},
		{"equal CPU pair", "2", "2", "", "", "2", "2", "128Mi", ""},
		{"memory request without ceiling", "", "", "2Gi", "", "100m", "", "2Gi", ""},
		{"memory pair", "", "", "512Mi", "2Gi", "100m", "", "512Mi", "2Gi"},
		{"equal memory pair", "", "", "2Gi", "2Gi", "100m", "", "2Gi", "2Gi"},
		{"omit both CPU", "none", "none", "", "", "", "", "128Mi", ""},
		{"CPU limit only", "none", "50m", "", "", "", "50m", "128Mi", ""},
		{"memory limit only", "", "", "none", "2Gi", "100m", "", "", "2Gi"},
		{"memory request only", "", "", "2Gi", "none", "100m", "", "2Gi", ""},
		{"all optional omitted", "none", "none", "none", "none", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.CPURequest, cfg.CPULimit, cfg.MemoryRequest, cfg.MemoryLimit = tc.cpuReq, tc.cpuLim, tc.memReq, tc.memLim
			if _, err := New(cfg, fake.NewSimpleClientset(), &rest.Config{}); err != nil {
				t.Fatal("startup", err)
			}
			p := podSpec(podName("resources"), "resources", cfg.Image, cfg)
			r := p.Spec.Containers[0].Resources
			check := func(list core.ResourceList, key core.ResourceName, want string) {
				t.Helper()
				q, present := list[key]
				if want == "" {
					if present {
						t.Fatal("omitted resource emitted", key, q.String())
					}
					return
				}
				if !present || q.Cmp(resource.MustParse(want)) != 0 {
					t.Fatal("resource mismatch", key, q.String(), want)
				}
			}
			check(r.Requests, core.ResourceCPU, tc.wantCPUReq)
			check(r.Limits, core.ResourceCPU, tc.wantCPULim)
			check(r.Requests, core.ResourceMemory, tc.wantMemReq)
			check(r.Limits, core.ResourceMemory, tc.wantMemLim)
			check(r.Requests, core.ResourceEphemeralStorage, "256Mi")
			check(r.Limits, core.ResourceEphemeralStorage, "")
			if p.Spec.Volumes[0].EmptyDir.SizeLimit != nil {
				t.Fatal("default workspace cap")
			}
			// Check actual serialized policy: omission is not a zero/empty CPU limit.
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var serialized struct{ Requests, Limits map[string]string }
			if err := json.Unmarshal(data, &serialized); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{"cpu": tc.wantCPULim, "memory": tc.wantMemLim, "ephemeral-storage": ""} {
				_, present := serialized.Limits[key]
				if present != (want != "") {
					t.Fatal("JSON limit omission", key, string(data))
				}
			}
		})
	}
}

func TestJobResourceInvalidStartup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
		want   string
	}{
		{"CPU bad", func(c *Config) { c.CPURequest = "bogus" }, "JOB_CPU_REQUEST"},
		{"CPU negative", func(c *Config) { c.CPURequest = "-1" }, "JOB_CPU_REQUEST"},
		{"CPU zero", func(c *Config) { c.CPURequest = "0" }, "JOB_CPU_REQUEST"},
		{"CPU limit bad", func(c *Config) { c.CPULimit = "1cpu" }, "JOB_CPU_LIMIT"},
		{"CPU limit zero", func(c *Config) { c.CPULimit = "0" }, "JOB_CPU_LIMIT"},
		{"CPU limit negative", func(c *Config) { c.CPULimit = "-2" }, "JOB_CPU_LIMIT"},
		{"CPU request precision", func(c *Config) { c.CPURequest = "0.5m" }, "JOB_CPU_REQUEST"},
		{"CPU limit precision", func(c *Config) { c.CPULimit = "0.0005" }, "JOB_CPU_LIMIT"},
		{"CPU mismatch", func(c *Config) { c.CPURequest = "2"; c.CPULimit = "1" }, "JOB_CPU_REQUEST"},
		{"default CPU mismatch", func(c *Config) { c.CPULimit = "50m" }, "JOB_CPU_REQUEST"},
		{"memory request bad", func(c *Config) { c.MemoryRequest = "1GiB" }, "JOB_MEMORY_REQUEST"},
		{"memory request zero", func(c *Config) { c.MemoryRequest = "0" }, "JOB_MEMORY_REQUEST"},
		{"memory request negative", func(c *Config) { c.MemoryRequest = "-1Gi" }, "JOB_MEMORY_REQUEST"},
		{"memory limit bad", func(c *Config) { c.MemoryLimit = "2Gii" }, "JOB_MEMORY_LIMIT"},
		{"memory limit zero", func(c *Config) { c.MemoryLimit = "0" }, "JOB_MEMORY_LIMIT"},
		{"memory limit negative", func(c *Config) { c.MemoryLimit = "-1Gi" }, "JOB_MEMORY_LIMIT"},
		{"memory mismatch", func(c *Config) { c.MemoryRequest = "2Gi"; c.MemoryLimit = "1Gi" }, "JOB_MEMORY_REQUEST"},
		{"default memory mismatch", func(c *Config) { c.MemoryLimit = "64Mi" }, "JOB_MEMORY_REQUEST"},
		{"wrong case", func(c *Config) { c.CPULimit = "NONE" }, "JOB_CPU_LIMIT"},
		{"whitespace", func(c *Config) { c.MemoryLimit = "none " }, "JOB_MEMORY_LIMIT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			tc.change(&cfg)
			if _, err := New(cfg, fake.NewSimpleClientset(), &rest.Config{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal("invalid resource accepted or wrong field", err)
			}
		})
	}
}

func TestJobResourceTemplateConflicts(t *testing.T) {
	for _, dind := range []bool{false, true} {
		cfg := testConfig()
		if dind {
			cfg.DinD = dindConfig().DinD
		}
		kube := fake.NewSimpleClientset()
		s, _ := New(cfg, kube, &rest.Config{})
		r := &pb.CreateRequest{Name: "resource-retry"}
		if _, err := s.Create(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		for _, change := range []func(*Config){
			func(c *Config) { c.CPURequest = "500m" }, func(c *Config) { c.CPULimit = "2" },
			func(c *Config) { c.MemoryRequest = "512Mi" }, func(c *Config) { c.MemoryLimit = "2Gi" },
			func(c *Config) { c.CPURequest = "none" }, func(c *Config) { c.EphemeralStorageLimit = "2Gi" },
			func(c *Config) { c.WorkspaceSizeLimit = "1Gi" },
		} {
			other := cfg
			change(&other)
			reload, err := New(other, kube, &rest.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reload.Create(context.Background(), r); status.Code(err) != codes.AlreadyExists {
				t.Fatal("resource change reused existing Pod", err)
			}
		}
		equivalent := cfg
		equivalent.CPULimit = "none"
		equivalent.MemoryLimit = "none"
		equivalent.EphemeralStorageLimit = "none"
		equivalent.WorkspaceSizeLimit = "none"
		reload, _ := New(equivalent, kube, &rest.Config{})
		if _, err := reload.Create(context.Background(), r); err != nil {
			t.Fatal("equivalent omission should hash identically", err)
		}
	}
}

func TestJobAndDinDResourceIndependence(t *testing.T) {
	cfg := dindConfig()
	before := podSpec(podName("independent"), "independent", cfg.Image, cfg)
	cfg.CPURequest = "500m"
	cfg.CPULimit = "3"
	cfg.MemoryRequest = "512Mi"
	cfg.MemoryLimit = "4Gi"
	after := podSpec(before.Name, "independent", cfg.Image, cfg)
	if !reflect.DeepEqual(before.Spec.Containers[1], after.Spec.Containers[1]) || !reflect.DeepEqual(before.Spec.Volumes, after.Spec.Volumes) || !reflect.DeepEqual(before.Spec.Containers[0].SecurityContext, after.Spec.Containers[0].SecurityContext) {
		t.Fatal("job policy altered daemon/storage/security")
	}
	jobResources := after.Spec.Containers[0].Resources
	cfg.DinD.CPURequest = "250m"
	cfg.DinD.CPULimit = "4"
	cfg.DinD.MemoryRequest = "256Mi"
	cfg.DinD.MemoryLimit = "3Gi"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	changed := podSpec(before.Name, "independent", cfg.Image, cfg)
	if !reflect.DeepEqual(jobResources, changed.Spec.Containers[0].Resources) || reflect.DeepEqual(after.Spec.Containers[1].Resources, changed.Spec.Containers[1].Resources) {
		t.Fatal("daemon resource controls leaked into job")
	}
}

func TestJobImagePullPolicyDefaultedByKubernetes(t *testing.T) {
	for _, image := range []string{"node:latest", "node", "node:26-bookworm", identityNodeImage} {
		cfg := testConfig()
		p := podSpec(podName(image), image, image, cfg)
		if p.Spec.Containers[0].ImagePullPolicy != "" {
			t.Fatal("job overrides runtime default", image)
		}
		data, err := json.Marshal(p.Spec.Containers[0])
		if err != nil || strings.Contains(string(data), "imagePullPolicy") {
			t.Fatal("serialized pull override", err)
		}
	}
	// Fixed daemon remains pinned/unchanged.
	cfg := dindConfig()
	p := podSpec(podName("dind-pull"), "dind-pull", cfg.Image, cfg)
	if p.Spec.Containers[1].ImagePullPolicy != core.PullIfNotPresent {
		t.Fatal("daemon pull policy changed")
	}
}

func TestOmittedSchedulingAndNetworkPolicyFields(t *testing.T) {
	cfg := testConfig()
	p := podSpec(podName("defaults"), "defaults", cfg.Image, cfg)
	s := p.Spec
	if len(s.NodeSelector) != 1 || s.NodeSelector["kubernetes.io/arch"] != cfg.Arch {
		t.Fatal("architecture contract")
	}
	if s.Affinity != nil || len(s.Tolerations) != 0 || len(s.TopologySpreadConstraints) != 0 || s.PriorityClassName != "" || s.SchedulerName != "" || s.NodeName != "" || s.DNSPolicy != "" || s.DNSConfig != nil || s.Hostname != "" || s.Subdomain != "" || len(s.HostAliases) != 0 || s.EnableServiceLinks != nil {
		t.Fatal("unnecessary scheduling/network override")
	}
	if s.HostNetwork || s.HostPID || s.HostIPC || s.ShareProcessNamespace != nil || len(s.Containers[0].Ports) != 0 {
		t.Fatal("host/job exposure")
	}
	if s.TerminationGracePeriodSeconds != nil || s.Containers[0].Lifecycle != nil || s.Containers[0].LivenessProbe != nil || s.Containers[0].ReadinessProbe != nil || s.Containers[0].StartupProbe != nil || len(s.InitContainers) != 0 {
		t.Fatal("unnecessary lifecycle override")
	}
	if s.Containers[0].SecurityContext.ReadOnlyRootFilesystem != nil {
		t.Fatal("image filesystem override")
	}
}
