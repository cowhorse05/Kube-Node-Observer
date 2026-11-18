# Contributing

KubeNode-Observer welcomes bug reports, documentation improvements and focused code changes.

## Development workflow

The project requires Linux with BTF and cgroup v2 for live eBPF tests. Unit tests, CLI tests and Helm rendering also run in WSL or a standard Linux CI runner.

```bash
go test ./...
python3 -m unittest discover -s hack -p '*_test.py'
make build
make helm-lint
```

Run `gofmt` on changed Go files and `git diff --check` before opening a pull request. Changes to eBPF accounting, runtime identity or Host/Guest boundaries must include regression tests and explain missing/partial-data semantics.

## Pull requests

- Keep each pull request focused and squash local fixup commits.
- Describe the user-visible behavior and the validation performed.
- Do not commit generated experiment directories under `log/`; attach large evidence to a release or pull request instead.
- Reuse the established CNCF ecosystem for storage, transport, profiling, dashboards and generic eBPF events. A new KNO component must document why integration cannot preserve the required runtime-boundary identity.
- Never report missing Guest, Host or runtime data as zero.
- Preserve runc behavior when changing Kata-specific paths.

By contributing, you agree that your contribution is licensed under the Apache License 2.0.
