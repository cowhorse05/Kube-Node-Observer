# Proposal: Guest-side eBPF observability probe for Kata Containers

> Status: draft for upstream discussion. This is a contribution proposal, not a
> product claim. Everything below that says "validated" was validated on a
> single-node Minikube + Kata 4.3.1 (Go/QEMU) environment; nothing here has run
> on a production cluster.

## 1. The problem

When a Kata pod is slow, host-side tools can only see the VMM (QEMU/Cloud
Hypervisor/Firecracker) and the host-side shim. They cannot answer the two
questions an operator actually asks:

1. Which *guest* process/container is consuming CPU inside the microVM?
2. Is the guest task **runnable-but-not-running** (guest-internal scheduling
   contention), or is the guest vCPU itself starved by **host-side** vCPU
   contention?

Today those are answered by three disconnected, hand-rolled pieces, or not at
all. Host metrics (cAdvisor, node-exporter, Pixie, Tetragon) stop at the VMM
boundary; `kata-monitor` exposes guest-OS/agent-level aggregates but not
process-level CPU or scheduling latency; and nothing correlates the two sides
without conflating host and guest accounting.

This is a real gap for the Kata project: every other container runtime gives you
`top`-style process-level attribution through cgroups, but a microVM hides it.

## 2. What exists today

- **`kata-monitor`** (`src/runtime/cmd/kata-monitor`) aggregates metrics from
  the kata-agent over its gRPC channel (sandbox-level CPU/memory, agent
  status). It is the correct integration point for *sandbox-level* data, and it
  is deliberately not a per-process profiler.
- **The kata-agent** runs in the guest and already reports guest OS metrics. It
  does not load eBPF and does not expose per-process scheduling data.
- **Host-side eBPF tools** (Pixie, Tetragon, Pyroscope, bcc `runqlat`) are
  host-kernel only. For a Kata pod they see the VMM and nothing inside it.
- **`kata-runtime exec`** can reach into the guest, but only for a single
  command, not for continuous per-process accounting.

So the gap is precisely: **guest-internal, per-process CPU and run-queue-latency
with a verifiable link back to a Kubernetes Pod/Sandbox identity — without
reimplementing a whole monitoring stack.**

## 3. The proposal

Run a small privileged **guest probe** (a sidecar sharing the pod's PID
namespace) inside the Kata guest. It loads the same CO-RE eBPF programs an
operator already uses on the host — `sched_switch`-based CPU-runtime aggregation
and `sched_wakeup`/`sched_switch`-based run-queue latency — but attached to the
**guest kernel**. It aggregates in-kernel into bounded maps (no per-event
streaming) and serves a short, bounded measurement window over a fixed port on
the pod network. A **node-side relay** validates the pod/sandbox identity and
adds the authoritative `sandboxId`/`namespace`/`podName`, so the guest never
self-reports its own Kubernetes identity.

```
sched_switch / sched_wakeup (guest kernel)
        │  aggregate in-kernel (CPU runtime, log2 latency histogram)
        ▼
Guest probe (privileged sidecar, shares pod PID namespace)
        │  verifies process/thread identity against shared procfs
        ▼  fixed pod IP:port, one concurrent window, bounded response
Node-side relay (validates Pod UID / sandbox ID / VMM generation)
        │  adds authoritative sandbox/namespace/pod identity
        ▼
kno guest resources --sandbox <id>   (or a Prometheus/OTel exporter)
```

Two design principles carry the whole thing:

- **Aggregate in the kernel, expose on demand.** High-frequency scheduling
  events never leave the guest kernel. A query brackets a start/end map
  snapshot and returns deltas. This is what keeps the probe cheap (measured
  agent overhead on the runc equivalent is ~2–3% user CPU, not an event firehose).
- **The guest never self-attributes.** The relay adds `sandboxId`/`podUid` only
  after checking the CRI sandbox, VMM generation and Pod IP, and each request
  carries a fresh nonce. This is *correlation*, not cryptographic proof — a
  malicious guest can still lie — but it prevents accidental mis-attribution,
  which is the thing that actually bites in practice (a stale PID or a reused
  Pod name).

## 4. Key design decisions and rationale

| Decision | Why |
| --- | --- |
| Sidecar sharing the **pod PID namespace**, not a whole-VM probe | `shareProcessNamespace` lets the probe see the pod's processes via procfs; BPF attaches to guest-global scheduling events but results are filtered to threads provable in that procfs view. This is also the only change a pod author has to make — no host configuration. |
| **Pod-network HTTP relay**, not VSOCK | Works today on Kata 4.x without guest/host VSOCK plumbing or a new device model. A bounded, nonce-checked loopback relay is the smallest correct thing. VSOCK is the obvious next step for security, listed as future work. |
| **Reuse existing CO-RE eBPF programs**, not new ones | The host and guest run the *same* `sched_switch`/`sched_wakeup` programs; only the attach kernel differs. That is the whole portability story, and it is what makes host-vs-guest numbers comparable in shape. |
| **Require the official debug guest kernel** (`vmlinux-debug`) | The stock guest kernel lacks `CONFIG_DEBUG_INFO_BTF` / `CONFIG_BPF_EVENTS`. The proposal does **not** ask to enable eBPF in the default guest image (attack surface + image size); it targets the debug kernel / a custom build, and documents that `uname -r` alone is not proof of capability. |
| **Explicitly label `partial` / `null` / coverage** | CPU requires a complete, stable thread set; runqlat may cover a subset and reports `runqlatCoverage`. Missing data is never rendered as zero. This matters for a monitoring tool more than raw accuracy. |

## 5. Non-goals and boundaries (read this before "why not X")

- **Not a full VM monitor, not a replacement for kata-monitor.** It answers
  "which process, and where does the wait happen". It does not replace agent
  metrics, tracing, or security enforcement.
- **Not a security boundary.** A malicious guest can forge its own measurements.
  The relay prevents *accidental* mis-attribution, not compromise.
- **Not clock-synced (yet).** Host and guest have no shared timeline. A
  four-timestamp offset bracketing experiment concluded it cannot currently
  produce a usable cross-layer synchronization gate, so this proposal does
  **not** claim host/guest time alignment or causal attribution. It provides
  *two coherent views of the same sandbox*, which is already enough to separate
  "guest-internal contention" from "host vCPU contention" at the level of
  direction-of-effect.
- **Not a production security profile.** The relay is HTTP without TLS/auth,
  the probe is privileged inside the guest, and the whole thing trusts the
  guest's other workloads. Multi-tenant hardening is out of scope for this RFC.

## 6. Current status and honest validation boundary

- Environment: single-node Minikube, Kata **4.3.1 Go/QEMU** (the deprecated Go
  runtime, chosen only because its shim/monitor chain is easy to verify), guest
  kernel **6.18.56 debug build**, containerd 2.2.1.
- Validated: guest CPU/runtime attribution and runqlat windows for a Kata pod;
  host-side VMM/vCPU thread view for the same sandbox; positive/negative identity
  checks (relay rejects wrong Pod UID, replayed nonce, pre-filled sandbox id);
  delete/cleanup; no OOM/restart of the probe.
- **Not validated:** runtime-rs, Cloud Hypervisor, Firecracker, Dragonball,
  VSOCK, multi-VM capacity, and anything on a real multi-node cluster. The
  cluster-side validation of this very project has been blocked on image
  delivery to a shared cluster, which is an infrastructure problem, not a design
  problem.

## 7. Open questions for the Kata maintainers

1. Is there appetite for a **guest-side observability probe** as a first-class
   Kata component, or should this stay an external sidecar pattern documented in
   the "how-to" tree? We are prepared for either framing.
2. The current design uses **pod-network HTTP + nonce** for the relay because it
   works on 4.x. Does the project prefer we hold the RFC until it moves to
   **VSOCK + a proper kata-agent channel**, which is where it should end up for
   security? If so, we'd like guidance on the agent-side extension surface.
3. The probe requires **`CONFIG_DEBUG_INFO_BTF` + `CONFIG_BPF_EVENTS`** in the
   guest kernel. Is a documented "observability guest image" (debug kernel +
   probe binary, like today's `vmlinux-debug` container) an acceptable first
   step, or does the project want to gate on a smaller, eBPF-enabled *default*
   guest kernel decision first?
4. Should the aggregated output adopt an **existing wire format** (OpenTelemetry
   metrics, Prometheus text, or kata-monitor's existing gRPC) rather than the
   current ad-hoc JSON? We suspect yes, and would target kata-monitor's metrics
   schema if that's the natural home.
5. Where does the **host↔guest correlation** belong long term — in kata-monitor,
   or in the node-side agent that already knows the CRI sandbox→VMM mapping?
   (The latter is outside Kata; we want to avoid expanding Kata's scope into
   Kubernetes-specific identity if the community prefers a runtime-only API.)

## 8. Alternatives considered

- **Extend kata-agent with eBPF.** Most architecturally pure, but heavier to
  review (agent is a security-critical, every-sandbox component) and forces the
  eBPF feature decision into the default guest image. The sidecar keeps the
  blast radius per-pod and opt-in.
- **Host-only attribution (Pixie/Tetragon/runqlat on the host).** Cannot see the
  guest kernel; sees only the VMM. Fails problem statement item 1 entirely.
- **kata-monitor only.** Already correct for sandbox-level metrics; does not do
  per-process or per-thread scheduling latency.
- **VSOCK from day one.** Better security posture, but blocked on the current
  guest/agent VSOCK surface and adds device-model complexity before the
  observability value is proven.

## 9. Proposed next steps

1. Settle the framing (sidecar pattern vs. first-class component) and the wire
   format with maintainers.
2. Prototype the VSOCK + kata-agent path behind the existing HTTP relay, so the
   security question (open question 2) has a concrete answer.
3. Get the probe validated on a real multi-node cluster — which requires an
   image-delivery story this project does not yet have — before asking anyone to
   run it.

## 10. The ask

We are offering the guest-side eBPF probe design and its (single-node) validation
evidence as a contribution. We want feedback on the two things that determine
whether it is useful to Kata: **the security boundary (HTTP nonce relay vs.
VSOCK/agent)** and **whether guest-internal per-process observability is a gap
the project wants closed at all.** If it is, we will shape the code and the
guest-image story to whatever the project decides.
