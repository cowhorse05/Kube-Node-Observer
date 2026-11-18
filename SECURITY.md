# Security policy

## Reporting a vulnerability

Please report vulnerabilities through GitHub's private security advisory feature for this repository. Do not open a public issue for an undisclosed vulnerability. Include the affected version, deployment configuration, reproduction steps and expected impact when possible.

## Deployment boundary

KubeNode-Observer is a privileged node agent. Its default deployment uses the host PID and network namespaces and reads host procfs, cgroup v2, tracefs and the containerd CRI socket. Treat access to the Agent HTTP endpoint as node-observability access and restrict it with cluster networking and RBAC appropriate to your environment.

The Guest Probe and relay are disabled by default. Enabling them expands the trust boundary into a Kata Guest and must be an explicit deployment decision. See the Guest Probe documentation before enabling that path.

Only the latest release receives security fixes. The project does not currently claim a hardened multi-tenant production security profile.
