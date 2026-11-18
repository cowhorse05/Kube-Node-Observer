package metrics

import (
	"runtime"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cowhorse05/Kube-Node-Observer/internal/cpu"
	"github.com/cowhorse05/Kube-Node-Observer/internal/topology"
)

type TopologySource interface {
	CurrentSnapshot() (*topology.Snapshot, error)
}

type CPUUsageSource interface {
	CurrentSnapshot() (*cpu.Snapshot, error)
}

type RuntimeSources struct {
	Topology TopologySource
	CPU      CPUUsageSource
}

type collector struct {
	health   HealthSource
	maps     *MapCache
	procRoot string
	runtime  RuntimeSources
	desc     map[string]*prometheus.Desc
}

func newCollector(health HealthSource, maps *MapCache, procRoot string, sources RuntimeSources) *collector {
	definitions := []struct {
		name, help string
		labels     []string
	}{
		{"kno_agent_events_seen_total", "Process lifecycle events consumed by the agent.", nil},
		{"kno_agent_events_lost_total", "Process lifecycle events dropped in the kernel ring buffer.", nil},
		{"kno_agent_cpu_seconds_total", "CPU time consumed by the agent process.", nil},
		{"kno_agent_resident_memory_bytes", "Resident memory used by the agent process.", nil},
		{"kno_agent_go_heap_alloc_bytes", "Currently allocated Go heap bytes.", nil},
		{"kno_agent_bpf_map_entries", "Cached non-atomic map key census; ring buffers report zero. Omitted when stale or not ready.", []string{"map", "type"}},
		{"kno_agent_bpf_map_max_entries", "Configured eBPF map entry capacity; ring buffers use bytes.", []string{"map", "type"}},
		{"kno_agent_bpf_map_memlock_bytes", "Kernel-reported locked memory for an eBPF map.", []string{"map", "type"}},
		{"kno_agent_bpf_map_stats_refresh_interval_seconds", "Delay between background map refresh cycles.", nil},
		{"kno_agent_bpf_map_stats_max_age_seconds", "Cached map stats are omitted beyond this age.", nil},
		{"kno_agent_bpf_map_stats_ready", "One if a successful non-stale map sample is available.", []string{"source"}},
		{"kno_agent_bpf_map_stats_age_seconds", "Age since the start of the last successful map refresh; absent before first success.", []string{"source"}},
		{"kno_agent_bpf_map_stats_last_success_timestamp_seconds", "Start timestamp of last successful map refresh, zero before first success.", []string{"source"}},
		{"kno_agent_bpf_map_stats_refresh_duration_seconds", "Wall time of the last map refresh attempt.", []string{"source"}},
		{"kno_agent_bpf_map_stats_refresh_error", "One if the last map refresh attempt failed.", []string{"source"}},
		{"kno_agent_bpf_map_stats_refresh_errors_total", "Failed background map refresh attempts.", []string{"source"}},
		{"kno_agent_metrics_collection_errors", "Process read errors plus unavailable, stale, or failing snapshot sources.", nil},
		{"kno_topology_snapshot_ready", "One if the cached Kubernetes and CRI topology snapshot is current and healthy.", nil},
		{"kno_topology_snapshot_age_seconds", "Age of the current topology snapshot.", nil},
		{"kno_runtime_entities", "Current runtime entities grouped by isolation domain and data-quality state.", []string{"node", "runtime", "domain", "state"}},
		{"kno_runtime_cpu_snapshot_ready", "One if the background CPU snapshot is available and healthy.", nil},
		{"kno_runtime_cpu_snapshot_age_seconds", "Age of the background CPU snapshot.", nil},
		{"kno_runtime_cpu_usage_cores", "Recent runc container CPU usage in logical cores from the background eBPF snapshot.", []string{"node", "namespace", "pod", "pod_uid", "container", "runtime", "domain"}},
	}
	descriptors := make(map[string]*prometheus.Desc, len(definitions))
	for _, definition := range definitions {
		descriptors[definition.name] = prometheus.NewDesc(definition.name, definition.help, definition.labels, nil)
	}
	return &collector{health: health, maps: maps, procRoot: procRoot, runtime: sources, desc: descriptors}
}

func (c *collector) Describe(output chan<- *prometheus.Desc) {
	for _, descriptor := range c.desc {
		output <- descriptor
	}
}

func (c *collector) Collect(output chan<- prometheus.Metric) {
	stats := c.health.Stats()
	c.emit(output, "kno_agent_events_seen_total", prometheus.CounterValue, float64(stats.EventsSeen))
	c.emit(output, "kno_agent_events_lost_total", prometheus.CounterValue, float64(stats.EventsLost))

	collectionErrors := 0
	if cpuNS, err := readProcessCPU(c.procRoot); err == nil {
		c.emit(output, "kno_agent_cpu_seconds_total", prometheus.CounterValue, float64(cpuNS)/1e9)
	} else {
		collectionErrors++
	}
	if rss, err := readRSS(c.procRoot + "/self/statm"); err == nil {
		c.emit(output, "kno_agent_resident_memory_bytes", prometheus.GaugeValue, float64(rss))
	} else {
		collectionErrors++
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	c.emit(output, "kno_agent_go_heap_alloc_bytes", prometheus.GaugeValue, float64(memory.HeapAlloc))

	collectionErrors += c.collectMaps(output)
	collectionErrors += c.collectRuntime(output)
	c.emit(output, "kno_agent_metrics_collection_errors", prometheus.GaugeValue, float64(collectionErrors))
}

func (c *collector) collectMaps(output chan<- prometheus.Metric) int {
	if c.maps == nil {
		return 0
	}
	c.emit(output, "kno_agent_bpf_map_stats_refresh_interval_seconds", prometheus.GaugeValue, c.maps.interval.Seconds())
	c.emit(output, "kno_agent_bpf_map_stats_max_age_seconds", prometheus.GaugeValue, c.maps.ttl.Seconds())
	now := time.Now()
	errors := 0
	for index, sample := range c.maps.snapshot.Load().sources {
		source := sourceName(index)
		ready := !sample.success.IsZero() && now.Sub(sample.success) <= c.maps.ttl
		c.emit(output, "kno_agent_bpf_map_stats_ready", prometheus.GaugeValue, boolFloat(ready), source)
		c.emit(output, "kno_agent_bpf_map_stats_refresh_error", prometheus.GaugeValue, boolFloat(sample.failed), source)
		c.emit(output, "kno_agent_bpf_map_stats_refresh_errors_total", prometheus.CounterValue, float64(sample.errors), source)
		c.emit(output, "kno_agent_bpf_map_stats_refresh_duration_seconds", prometheus.GaugeValue, sample.duration.Seconds(), source)
		lastSuccess := float64(0)
		if !sample.success.IsZero() {
			lastSuccess = float64(sample.success.UnixNano()) / 1e9
			c.emit(output, "kno_agent_bpf_map_stats_age_seconds", prometheus.GaugeValue, now.Sub(sample.success).Seconds(), source)
		}
		c.emit(output, "kno_agent_bpf_map_stats_last_success_timestamp_seconds", prometheus.GaugeValue, lastSuccess, source)
		if !ready || sample.failed {
			errors++
		}
		if !ready {
			continue
		}
		for _, current := range sample.stats {
			c.emit(output, "kno_agent_bpf_map_entries", prometheus.GaugeValue, float64(current.Entries), current.Name, current.Type)
			c.emit(output, "kno_agent_bpf_map_max_entries", prometheus.GaugeValue, float64(current.MaxEntries), current.Name, current.Type)
			c.emit(output, "kno_agent_bpf_map_memlock_bytes", prometheus.GaugeValue, float64(current.Memlock), current.Name, current.Type)
		}
	}
	return errors
}

func (c *collector) collectRuntime(output chan<- prometheus.Metric) int {
	if c.runtime.Topology == nil && c.runtime.CPU == nil {
		return 0
	}
	now := time.Now()
	var snapshot *topology.Snapshot
	errors := 0
	if c.runtime.Topology != nil {
		var err error
		snapshot, err = c.runtime.Topology.CurrentSnapshot()
		if err != nil {
			c.emit(output, "kno_topology_snapshot_ready", prometheus.GaugeValue, 0)
			errors++
		} else {
			c.emit(output, "kno_topology_snapshot_ready", prometheus.GaugeValue, 1)
			c.emit(output, "kno_topology_snapshot_age_seconds", prometheus.GaugeValue, nonNegative(now.Sub(snapshot.GeneratedAt).Seconds()))
			c.collectRuntimeEntities(output, snapshot)
		}
	}
	if c.runtime.CPU != nil {
		cpuSnapshot, err := c.runtime.CPU.CurrentSnapshot()
		if err != nil {
			c.emit(output, "kno_runtime_cpu_snapshot_ready", prometheus.GaugeValue, 0)
			errors++
		} else {
			c.emit(output, "kno_runtime_cpu_snapshot_ready", prometheus.GaugeValue, 1)
			c.emit(output, "kno_runtime_cpu_snapshot_age_seconds", prometheus.GaugeValue, nonNegative(now.Sub(cpuSnapshot.SampledAt).Seconds()))
			if snapshot != nil {
				c.collectCPU(output, snapshot, cpuSnapshot)
			}
		}
	}
	return errors
}

func (c *collector) collectRuntimeEntities(output chan<- prometheus.Metric, snapshot *topology.Snapshot) {
	type key struct{ runtime, domain, state string }
	counts := make(map[key]int)
	for _, container := range snapshot.Containers {
		if !isRunc(container) {
			continue
		}
		state := "ready"
		if container.CgroupID == 0 || len(container.Issues) > 0 {
			state = "partial"
		}
		counts[key{"runc", "container", state}]++
	}
	for _, sandbox := range snapshot.Sandboxes {
		state := "ready"
		if sandbox.Hypervisor == nil || len(sandbox.Issues) > 0 {
			state = "partial"
		}
		counts[key{"kata", "host_vmm", state}]++
	}
	for current, count := range counts {
		c.emit(output, "kno_runtime_entities", prometheus.GaugeValue, float64(count), snapshot.NodeName, current.runtime, current.domain, current.state)
	}
}

func (c *collector) collectCPU(output chan<- prometheus.Metric, topologySnapshot *topology.Snapshot, cpuSnapshot *cpu.Snapshot) {
	containers := make(map[string]topology.Container, len(topologySnapshot.Containers))
	for _, container := range topologySnapshot.Containers {
		if isRunc(container) {
			containers[container.ID] = container
		}
	}
	for _, usage := range cpuSnapshot.Containers {
		container, ok := containers[usage.ContainerID]
		if !ok {
			continue
		}
		podUID := container.KubernetesPodUID
		if podUID == "" {
			podUID = container.PodUID
		}
		c.emit(output, "kno_runtime_cpu_usage_cores", prometheus.GaugeValue, usage.CPUPercent/100,
			topologySnapshot.NodeName, container.Namespace, container.PodName, podUID, container.Name, "runc", "container")
	}
}

func (c *collector) emit(output chan<- prometheus.Metric, name string, valueType prometheus.ValueType, value float64, labels ...string) {
	output <- prometheus.MustNewConstMetric(c.desc[name], valueType, value, labels...)
}

func isRunc(container topology.Container) bool {
	return container.ProcessVisibility == "host" && (container.RuntimeType == "" || strings.HasPrefix(container.RuntimeType, "io.containerd.runc."))
}

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func nonNegative(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}
