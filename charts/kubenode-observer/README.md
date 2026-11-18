# KubeNode-Observer Helm chart

This chart installs `kno-agent` as a privileged Linux DaemonSet. The Agent reads host procfs, cgroup v2, tracefs and the containerd CRI socket to build runtime-aware diagnostics.

```bash
helm install kno ./charts/kubenode-observer \
  --namespace kno-system --create-namespace \
  --set image.repository=kubenode-observer/kno-agent \
  --set image.tag=dev
```

The default image is the continuously published `main` build from GHCR. Pin a version tag for reproducible environments once a release is published. Override the repository and tag when using a locally loaded development image.

Enable Prometheus Operator discovery when the `ServiceMonitor` CRD is installed:

```bash
helm upgrade --install kno ./charts/kubenode-observer \
  --namespace kno-system --create-namespace \
  --set serviceMonitor.enabled=true
```

Enable the standard alert rules and Grafana sidecar dashboard when those integrations are installed:

```bash
helm upgrade --install kno ./charts/kubenode-observer \
  --namespace kno-system --create-namespace \
  --set serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true \
  --set grafanaDashboard.enabled=true
```

The Chart does not install Prometheus, Grafana or their Operators. See the [metrics guide](../../docs/metrics.md) for semantics, cardinality and the OpenTelemetry Collector receiver example.

Important values:

| Value | Default | Purpose |
| --- | --- | --- |
| `image.repository` | `ghcr.io/cowhorse05/kubenode-observer` | Agent image repository |
| `image.tag` | `main` | Agent image tag; pin a release in stable environments |
| `agent.runtimeEndpoint` | containerd socket | CRI endpoint inside the Agent |
| `hostPaths.runtimeSocket` | containerd socket | Socket path on the node |
| `serviceMonitor.enabled` | `false` | Render a Prometheus Operator ServiceMonitor |
| `prometheusRule.enabled` | `false` | Render bounded Agent/data-quality alerts |
| `grafanaDashboard.enabled` | `false` | Render a dashboard ConfigMap for a Grafana sidecar |
| `agent.kataMonitorEndpoint` | empty | Opt-in host Kata Monitor endpoint |
| `agent.guestProbePort` | `0` | Opt-in Guest Probe relay; zero disables it |
| `securityContext.privileged` | `true` | Required by the current eBPF deployment model |

Changing `runtimeEndpoint` to a different socket path also changes its container mount path; set `hostPaths.runtimeSocket` to the corresponding node path.
