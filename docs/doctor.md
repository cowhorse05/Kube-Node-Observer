# 节点自检

`kno doctor` 是 M11 的只读节点观测链路检查。它使用已经存在的 Agent 接口，不加载新 eBPF、不创建工作负载、不修改权限，也不自动“修复”节点：

```bash
./bin/kno doctor
./bin/kno doctor --endpoint http://127.0.0.1:18080
./bin/kno doctor -o json
```

默认总超时 10 秒，可用 `--timeout` 调整为 `1s..1m`。

## 检查项

doctor 并行读取 `/healthz`、`/api/v1/topology` 和 `/metrics`，再从同一批响应生成以下检查：

| 检查 | 当前证据 | 不声称的内容 |
| --- | --- | --- |
| `agent` | health 状态、组件名、uptime | DaemonSet 永不重启 |
| `ebpf` | `ebpfLoaded` 与 exec/CPU/runqlat map census ready/refresh-error | 所有事件或 map key 永不淘汰 |
| `lifecycle-events` | 启动以来 `eventsSeen/eventsLost` | CPU/runqlat 的完整性 |
| `cri-topology` | 最近拓扑刷新、快照年龄、Container/Sandbox 数量 | CRI 未来持续可用 |
| `cgroup-v2` | 当前 Host-visible runc container 的 cgroup path 与 inode ID | 没有 runc target 时仍能证明归属链 |
| `kubernetes` | Node name/UID 是否由 informer 补全 | 所有 RBAC 操作均可用 |
| `kata` | 当前 Sandbox handler、verified shim 与 VMM identity | 没有 Sandbox 时证明 Kata/KVM 安装正确 |
| `permissions` | probe 状态、拓扑 issues 中的明确拒绝、metrics collection error | 独立穷举所有 Linux capability/LSM 策略 |

## 状态和退出码

- `PASS`：当前证据满足该项检查。
- `WARN`：链路可用但存在数据质量问题，或证据不完整。
- `FAIL`：核心链路当前不可用或身份/归属矛盾。
- `SKIP`：前置证据不可用，或没有当前目标可验证，例如没有 Kata Sandbox。

overall 取最严重的检查状态。`FAIL` 时命令先输出完整报告，再返回非零；只有 `PASS/WARN/SKIP` 时返回 0。自动化若要求零 warning，应解析 JSON 检查具体状态，而不是把 `SKIP` 当作通过。

## 修复建议

文本输出在 `REMEDIATION` 区域按检查项列出下一步，JSON 在每个 check 的 `remediation` 数组中返回相同建议。典型路径包括：

- Agent 不可达：检查 Pod Ready/restart 和 port-forward，再核对 `--endpoint`。
- eBPF 未加载：检查 BTF、tracefs、privileged/BPF 权限和 Host mounts。
- map census 未就绪：等待至少一个 map-stats interval；持续失败时检查 memlock、容量和 Agent 日志。
- CRI/topology 失败：核对 containerd socket mount、`--runtime-endpoint` 和 runtime RPC 日志。
- cgroup 归属缺失：核对 cgroup v2、`--cgroup-root`、Host procfs 与 PID namespace。
- Kubernetes identity 不完整：核对 `NODE_NAME`、Node/Pod get-list-watch RBAC 和 informer sync 日志。
- Kata identity 失败：核对 RuntimeClass handler、hostPID、procfs/cgroup 可见性，并查看 `kno topology` 的 sandbox issues。
- 明确 permission denied：恢复文档中的 DaemonSet security context/mounts，并检查 Pod Security、LSM 和 runtime policy。

建议是有界排障入口，不执行命令、不更改节点，也不把单次 PASS 外推为生产环境长期保证。

## JSON 契约

`-o json` 的 `schemaVersion` 为 `kno.doctor/v1`。顶层包含 `checkedAt`、`endpoint`、`overall` 和有序 `checks`。每个 check 始终包含：

- `name`：稳定检查标识；
- `status`：`pass/warn/fail/skip`；
- `summary`：面向人的当前结论；
- `evidence`：本次判断实际使用的有界证据；
- `remediation`：需要处理或补证据时的下一步。

输出不包含长期历史；需要保留证据时应保存本次 JSON、对应 Agent logs 和 `/metrics` scrape。
