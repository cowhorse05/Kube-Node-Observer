# Product roadmap

KubeNode-Observer is transitioning from a research repository into an installable runtime-diagnostics component. The product goal is not to replace Prometheus, OpenTelemetry, Parca, Pyroscope, Tetragon or Kata Monitor. KNO should provide the runc/Kata isolation identity and data-quality boundary that those systems can consume.

## Product promise

For a selected Kubernetes Pod, KNO should determine its runtime boundary, show the evidence available in each domain, and answer whether the current signal belongs to a runc container, a Kata Host VMM or a Kata Guest process.

The default operator path remains:

```bash
kno doctor
kno inspect pod <pod> -n <namespace>
```

## Reuse-first gate

KNO only owns the verified gap: correlating Kubernetes identity across runc containers and Kata Host/VMM/Guest boundaries. Before adding a subsystem, contributors must evaluate and integrate the established project that already owns it:

| Need | Reuse |
| --- | --- |
| Metrics storage, queries and alerts | Prometheus |
| Telemetry routing and OTLP export | OpenTelemetry Collector |
| Continuous profiling | Parca, Pyroscope or Grafana Alloy |
| Generic eBPF event observability | Tetragon or Inspektor Gadget |
| Dashboards | Grafana |
| Kata component metrics | Kata Monitor |
| Kubernetes packaging | Helm and Prometheus Operator CRDs |
| Image signing and provenance | Cosign and OCI attestations |

A new KNO component is justified only when these integrations cannot preserve runtime-boundary identity or data-quality semantics. Any proposal must document the upstream alternatives considered and the smallest missing capability. KNO must not fork an upstream project merely to rename, repackage or lightly customize it.

## P0: installable foundation

- Helm deployment with configurable host paths and runtime endpoint;
- optional Prometheus Operator ServiceMonitor;
- repository hygiene and curated evidence reports;
- Apache-2.0 licensing, contribution and security policy;
- CI rendering and Kubernetes API smoke tests;
- signed multi-architecture GHCR images with SBOM and provenance.

## P1: ecosystem metrics

- delivered: bounded runc container CPU and runc/Kata boundary inventory on the existing Prometheus endpoint;
- delivered: Prometheus base units, stable labels and explicit `runtime`/`domain` dimensions;
- delivered: documented cardinality budget, standard alerts and a practical Grafana dashboard;
- next: continuous Kata Host VMM metrics only when they can reuse a bounded background snapshot without changing Host/Guest semantics.

The scrape path must not run expensive BPF map scans. It should expose a background snapshot and retain partial/stale/error state.

## P2: OpenTelemetry and profiler correlation

- delivered: documented OpenTelemetry Collector Prometheus receiver pipeline;
- Kubernetes resource attribute mapping;
- Parca/Pyroscope co-deployment examples;
- CLI-generated profiler queries or deep links based on verified Pod/container identity.

KNO will not serialize aggregate CPU runtime as a fake pprof profile. Function-level profiles remain the responsibility of existing profilers.
KNO will not implement a parallel OTLP exporter: the Collector owns protocol conversion, batching, retries and export.

## P3: release and compatibility

- versioned CLI binaries and OCI Helm chart;
- tested Kubernetes, kernel, containerd and Kata version matrix;
- upgrade notes and compatibility policy;
- runc Kind integration on every pull request;
- separately labeled Kata validation on a capable runner.

## P4: upstream-oriented Guest proposal

- isolate the Guest protocol and threat model;
- document privilege, identity, replay and lifecycle guarantees;
- benchmark the opt-in path independently;
- discuss the design with the Kata Containers community before proposing a large upstream change.

## Non-goals

- another telemetry database or Web UI;
- a generic continuous profiler;
- runtime security policy enforcement;
- an Operator without a real reconciliation use case;
- automatic node remediation;
- claims that missing Guest evidence means zero Guest load.
