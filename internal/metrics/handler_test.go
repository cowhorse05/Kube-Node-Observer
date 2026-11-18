package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cowhorse05/Kube-Node-Observer/internal/agent"
	"github.com/cowhorse05/Kube-Node-Observer/internal/cpu"
	"github.com/cowhorse05/Kube-Node-Observer/internal/probe"
	"github.com/cowhorse05/Kube-Node-Observer/internal/topology"
)

type fakeHealth struct{ stats agent.HealthStats }

func (f fakeHealth) Stats() agent.HealthStats { return f.stats }

type fakeMaps struct{}

type fakeTopology struct {
	snapshot *topology.Snapshot
	err      error
}

func (f fakeTopology) CurrentSnapshot() (*topology.Snapshot, error) { return f.snapshot, f.err }

type fakeCPUUsage struct {
	snapshot *cpu.Snapshot
	err      error
}

func (f fakeCPUUsage) CurrentSnapshot() (*cpu.Snapshot, error) { return f.snapshot, f.err }

func (fakeMaps) MapStats() ([]probe.BPFMapStats, error) {
	return []probe.BPFMapStats{{Name: "cpu_runtime", Type: "LRUHash", Entries: 7, MaxEntries: 4096, Memlock: 65536}}, nil
}

func TestHandler(t *testing.T) {
	procRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(procRoot, "self", "task", "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(procRoot, "self", "task", "2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "self", "task", "1", "schedstat"), []byte("1500000000 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "self", "task", "2", "schedstat"), []byte("1000000000 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "self", "statm"), []byte("100 10 0 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	cache, err := NewMapCache([]probe.MapStatsSource{fakeMaps{}}, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cache.Refresh(context.Background())
	NewHandler(fakeHealth{agent.HealthStats{EventsSeen: 3, EventsLost: 1}}, cache, procRoot).ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, expected := range []string{
		"kno_agent_cpu_seconds_total 2.5",
		"kno_agent_events_seen_total 3",
		"kno_agent_events_lost_total 1",
		"kno_agent_bpf_map_entries{map=\"cpu_runtime\",type=\"LRUHash\"} 7",
		"kno_agent_bpf_map_memlock_bytes{map=\"cpu_runtime\",type=\"LRUHash\"} 65536",
		"kno_agent_metrics_collection_errors 0",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("response does not contain %q:\n%s", expected, body)
		}
	}
}

func TestHandlerRejectsNonGET(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewHandler(fakeHealth{}, nil, t.TempDir()).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandlerExportsRuntimeBoundarySnapshots(t *testing.T) {
	procRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(procRoot, "self", "task", "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "self", "task", "1", "schedstat"), []byte("1000000000 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "self", "statm"), []byte("100 10 0 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	runc := topology.Container{ID: "container-id", Name: "app", Namespace: "demo", PodName: "web", PodUID: "sandbox-uid", KubernetesPodUID: "pod-uid", ProcessVisibility: "host", RuntimeType: "io.containerd.runc.v2", CgroupID: 42}
	partialRunc := topology.Container{ID: "partial", Name: "sidecar", Namespace: "demo", PodName: "web", PodUID: "sandbox-uid", ProcessVisibility: "host", RuntimeType: "io.containerd.runc.v2"}
	topologySnapshot := &topology.Snapshot{
		GeneratedAt: now.Add(-2 * time.Second),
		NodeName:    "node-a",
		Containers:  []topology.Container{runc, partialRunc},
		Sandboxes: []topology.Sandbox{
			{ID: "kata-ready", Hypervisor: &topology.HostProcess{PID: 100}},
			{ID: "kata-partial", Issues: []string{"VMM unavailable"}},
		},
	}
	cpuSnapshot := &cpu.Snapshot{SampledAt: now.Add(-time.Second), WindowSeconds: 1, Containers: []cpu.ProcessUsage{{ContainerID: "container-id", CPUPercent: 125}}}
	handler := NewHandler(fakeHealth{}, nil, procRoot, RuntimeSources{
		Topology: fakeTopology{snapshot: topologySnapshot},
		CPU:      fakeCPUUsage{snapshot: cpuSnapshot},
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	for _, expected := range []string{
		"kno_topology_snapshot_ready 1",
		"kno_runtime_cpu_snapshot_ready 1",
		"kno_runtime_entities{domain=\"container\",node=\"node-a\",runtime=\"runc\",state=\"ready\"} 1",
		"kno_runtime_entities{domain=\"container\",node=\"node-a\",runtime=\"runc\",state=\"partial\"} 1",
		"kno_runtime_entities{domain=\"host_vmm\",node=\"node-a\",runtime=\"kata\",state=\"ready\"} 1",
		"kno_runtime_entities{domain=\"host_vmm\",node=\"node-a\",runtime=\"kata\",state=\"partial\"} 1",
		"kno_runtime_cpu_usage_cores{container=\"app\",domain=\"container\",namespace=\"demo\",node=\"node-a\",pod=\"web\",pod_uid=\"pod-uid\",runtime=\"runc\"} 1.25",
		"kno_agent_metrics_collection_errors 0",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("response does not contain %q:\n%s", expected, body)
		}
	}
}
