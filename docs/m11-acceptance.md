# M11 最终验收

`make m11-acceptance` 是 M11 的只读最终验收入口。它不会创建、删除或修改 Kubernetes 资源；它针对已经可访问的 Agent，保存 `kno doctor -o json`、runc Pod 的 `kno inspect pod -o json`，以及可选的 Kata Pod 结果，再离线校验身份、运行时和 Host/Guest 边界。

本仓库合入的是验收程序和判定契约，不代表某个外部集群已经通过。真实集群结果必须以脚本生成的 `summary.json` 和原始 JSON 为准。

## 运行前提

1. Agent 已部署，端口转发或服务地址可从运行脚本的主机访问；
2. 一个 runc Pod 正在运行；完整 Kata 覆盖还需要一个 Kata Pod；
3. 执行者能在当前源码上运行 Go、Python 3 和 Bash；
4. 若要验收 Guest 进程可见性，Kata Pod 已按 Guest Probe 文档显式启用 relay。

例如先建立端口转发：

```bash
kubectl --context <context> -n kube-system \
  port-forward service/kno-agent 18080:18080
```

在另一个终端运行完整 runc + Kata 验收：

```bash
make m11-acceptance \
  M11_ENDPOINT=http://127.0.0.1:18080 \
  M11_RUNC_NAMESPACE=default \
  M11_RUNC_POD=<runc-pod> \
  M11_KATA_NAMESPACE=default \
  M11_KATA_POD=<kata-pod>
```

仅验证 runc 链路时可省略两个 `M11_KATA_*` 参数。此时 `summary.json` 会明确写入 `"coverage": "runc-only"`，不能用来声称 Kata 已验收。

## Guest 和 warning 门禁

默认门禁接受 `doctor` 的 `WARN/SKIP` 和 `inspect` 的 warning，但把它们逐条写入摘要；任何 `FAIL`、命令非零、JSON 契约错误、Pod 身份不符、runc/Kata 判断错误或 Host/Guest 记账混淆都会使脚本非零退出。

这是有意的：当前 runc Host IO 尚未实现，会产生稳定 warning；没有配置 Guest Probe 的 Kata Pod 也会明确报告 Guest 不可见。需要额外门禁时使用：

```bash
# 要求 Kata Guest 证据存在
make m11-acceptance ... M11_REQUIRE_GUEST=true

# 将任意 doctor WARN/SKIP 或 inspect warning 都视为失败
make m11-acceptance ... M11_STRICT_WARNINGS=true
```

`M11_REQUIRE_GUEST=true` 只在同时提供 Kata Pod 时有意义。严格 warning 模式不适合作为当前默认门禁，因为已公开的 unsupported/partial 边界不是数据伪造，也不应被显示成零值。

## 证据目录

默认输出到被 Git 忽略的 `artifacts/m11-acceptance-<UTC>/`；可用 `M11_OUTPUT_DIR=<path>` 指定一个尚不存在的目录。脚本拒绝覆盖已有目录。目录包含：

| 文件 | 内容 |
| --- | --- |
| `manifest.json` | UTC 时间、源码 commit、工作区是否有修改、Agent endpoint |
| `doctor.json` / `doctor.stderr` | doctor 原始标准输出和错误输出 |
| `inspect-runc.json` / `.stderr` | runc inspect 原始结果 |
| `inspect-kata.json` / `.stderr` | 提供 Kata Pod 时的原始结果 |
| `summary.json` | 有版本的最终判定、覆盖范围、errors 和 warnings |
| `validation.exit` | 离线校验器退出码 |

验收通过必须同时满足：

- `make m11-acceptance` 返回 0；
- `summary.json` 的 `accepted` 为 `true`；
- 项目完整验收时 `coverage` 为 `runc+kata`；
- 如项目要求 Guest 可见性，使用了 `M11_REQUIRE_GUEST=true` 且仍通过；
- warnings 已人工确认属于文档中的支持边界，而不是新的数据质量退化。

建议把最终目录连同集群版本、RuntimeClass 和部署镜像 digest 一并归档。脚本不会采集凭据或 Kubernetes Secret；仍不应在 endpoint URL 中放入用户名、密码或 token。

## 离线复核

原始 JSON 可单独再次校验，不需要连接集群：

```bash
python3 hack/m11_acceptance.py \
  --doctor <dir>/doctor.json --doctor-exit 0 \
  --runc <dir>/inspect-runc.json --runc-exit 0 \
  --runc-namespace default --runc-pod <runc-pod> \
  --kata <dir>/inspect-kata.json --kata-exit 0 \
  --kata-namespace default --kata-pod <kata-pod> \
  --output <dir>/summary-recheck.json
```

校验器只使用 Python 标准库。它验证的是 M11 CLI 证据契约和运行时边界，不替代长期稳定性、性能统计或跨节点生产验收。
