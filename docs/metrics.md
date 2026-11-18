# Metrics and ecosystem integration

KubeNode-Observer exposes Prometheus text format at `/metrics` through the official Go Prometheus client. A scrape only reads immutable background snapshots; it never starts a BPF map scan, CRI refresh or Host measurement window.

## Runtime metrics

| Metric | Labels | Meaning |
| --- | --- | --- |
| `kno_topology_snapshot_ready` | none | `1` when the Kubernetes/CRI identity snapshot is current and healthy |
| `kno_topology_snapshot_age_seconds` | none | age of that identity snapshot |
| `kno_runtime_entities` | `node`, `runtime`, `domain`, `state` | low-cardinality count of runc containers and Kata Host VMM boundaries; `state` is `ready` or `partial` |
| `kno_runtime_cpu_snapshot_ready` | none | `1` when the background eBPF CPU snapshot is available |
| `kno_runtime_cpu_snapshot_age_seconds` | none | age of the background CPU snapshot |
| `kno_runtime_cpu_usage_cores` | `node`, `namespace`, `pod`, `pod_uid`, `container`, `runtime`, `domain` | recent runc container CPU usage; `1` means one logical CPU |

The CPU family intentionally has no process, command or container-ID label. Its active-series budget is bounded by the number of currently observed runc containers. An idle or unattributed container may have no series for a window; absence is not rewritten as zero.

Kata VMM CPU is not exported from the background runc accounting snapshot. Use `kno inspect pod` or `/api/v1/host/resources` for a bounded VMM measurement. KNO will not label Host VMM usage as Guest usage or manufacture a continuous series from an incompatible measurement.

The existing `kno_agent_*` families continue to report Agent cost, lifecycle loss and cached BPF map health. Prometheus serialization and content negotiation are delegated to `prometheus/client_golang` rather than maintained in KNO.

## Prometheus Operator and Grafana

With the Prometheus Operator and a dashboard sidecar already installed:

```bash
helm upgrade --install kno ./charts/kubenode-observer \
  --namespace kno-system --create-namespace \
  --set serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true \
  --set grafanaDashboard.enabled=true
```

The Chart creates standard `ServiceMonitor` and `PrometheusRule` resources plus a dashboard ConfigMap labeled `grafana_dashboard=1`. KNO does not install Prometheus, Grafana or an Operator.

Useful PromQL:

```promql
# runtime boundary inventory
sum by (runtime, domain, state) (kno_runtime_entities)

# busiest runc Pods
topk(10, sum by (namespace, pod) (kno_runtime_cpu_usage_cores))

# any incomplete topology source
min(kno_topology_snapshot_ready)
```

## OpenTelemetry Collector

Use the Collector's Prometheus receiver to translate, batch, retry and export KNO metrics. The example at [`deploy/otel-collector/kubenode-observer.yaml`](../deploy/otel-collector/kubenode-observer.yaml) discovers the Agent Pods through Kubernetes service discovery and sends metrics to an existing OTLP endpoint.

The Collector needs Kubernetes Pod discovery RBAC and the contrib distribution containing the Prometheus receiver. Set `OTLP_ENDPOINT` in the Collector deployment. KNO deliberately does not implement a second native OTLP exporter.

## Alert runbooks

### Lifecycle events lost

Inspect `kno_agent_events_lost_total`, Agent logs and ring-buffer pressure. Process history during the affected interval is incomplete; current topology and CPU snapshots may still be valid.

### Topology unavailable

Run `kno doctor`. Check Kubernetes informer permissions, the mounted containerd socket, CRI v1 availability and cgroup v2 identity. Do not interpret missing runtime entities as zero workloads.

### CPU snapshot unavailable

Run `kno doctor`, inspect Agent logs and verify the CPU eBPF probe/map state. The runtime CPU family is omitted while attribution is unavailable.

### BPF map statistics unavailable

Check `kno_agent_bpf_map_stats_refresh_error` and `kno_agent_bpf_map_stats_age_seconds` by source. The map census is refreshed in the background; repeated scraping cannot force a refresh.
