# Pod 一次诊断

`kno inspect pod` 是 M11 的第一版面向日常使用的 CLI。它把已经存在的节点观测接口组合成一份有边界的 Pod 快照：

```bash
./bin/kno inspect pod <pod> -n <namespace>
./bin/kno inspect pod <pod> -n <namespace> --duration 2s --limit 20
./bin/kno inspect pod <pod> -n <namespace> -o json
```

namespace 默认是 `default`。`--duration` 控制 Host 资源和可选 Guest Probe 的测量窗口，范围是 `100ms..10s`，默认 1 秒。`--limit` 控制热点进程和最近事件的最大条数，范围是 1..100，默认 10。

## 输出内容

命令先读取过滤后的当前拓扑，再并行查询现有接口：

| 区域 | 数据来源 | 语义 |
| --- | --- | --- |
| Pod / Topology | `/api/v1/topology` | Node、Pod UID、Container、cgroup、Kata Sandbox、shim 和 VMM 身份 |
| Host resources | `/api/v1/host/resources` | runc cgroup 或 Kata VMM 的 Host CPU、内存和 IO 窗口 |
| Hot processes | `/api/v1/cpu` 或 `/api/v1/guest/resources` | runc 使用 Host 进程；Kata 仅在 opt-in Guest relay 可用时展示 Guest 进程 |
| VMM threads | Host resource 结果 | Kata VMM/vCPU 线程 CPU 与 runqlat；不是 Guest 进程 |
| Recent events | `/api/v1/events` | 内存环当前保留的已归属 exec/exit 子集 |
| Warnings | 所有响应与 `/healthz` | partial、缺失、未归属任务和生命周期 ring buffer reservation failure |

Host resource 与 Guest resource 窗口并行开始，但 `inspect` 不宣称两层时钟已经对齐，也不从这一次读取推断根因。最近事件是有界内存环中的子集；`eventsLost=0` 只说明 exec/exit ring buffer 没有报告 reservation failure，不代表 CPU/runqlat 明细绝对完整。

## runc 与 Kata 边界

runc Pod 的进程在 Host 可见，`HOT PROCESSES (HOST)` 使用节点 CPU 采样结果。Kata Pod 的 CRI container PID 不是 Guest init PID，命令不会把它当作 Host 进程；它展示独立的 Sandbox/VMM Host 资源，并尝试通过已有的 opt-in Guest relay 读取 `HOT PROCESSES (GUEST)`。

Guest relay 默认关闭或 Pod 未配置 Guest Probe 时，命令仍然返回拓扑、VMM Host 资源和事件，并产生 `guest_processes_unavailable`。这不是零负载或零 Guest CPU，而是数据不可见。

现有 Host resource API 没有实现 runc container IO，因此 runc 表格会显示 `unsupported`/`limited` 并产生 `host_resource_fields_missing`；它不会把缺失值显示成 0。Kata VMM IO 仍是 Host QEMU 进程窗口，不代表 Guest 块设备吞吐。

## JSON 契约

`-o json` 的顶层 `schemaVersion` 当前为 `kno.inspect-pod/v1`。顶层区域包括：

- `pod` 和 `topology`：本次选择及归属链；
- `host`：Host 测量成功时存在；
- `hostProcesses`：最新 Host 进程 CPU 采样成功时存在；
- `guest`：Kata Guest relay 成功时存在；
- `events` 和 `health`：对应接口成功时存在；
- `warnings`：始终是数组，每项包含稳定的 `code` 和面向人的 `message`。

非关键接口失败不会让整条命令失败，而会省略对应区域并添加 warning。拓扑不可用、过滤后没有 Container/Sandbox，或 Pod 不在当前节点快照中时，命令返回非零退出码，因为此时无法安全建立身份边界。
