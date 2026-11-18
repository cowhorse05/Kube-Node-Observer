## What changed

<!-- Describe the user-visible behavior and why it belongs in KNO. -->

## Runtime boundaries

<!-- Explain runc/Kata and Host/VMM/Guest impact. Use N/A when not applicable. -->

## Validation

<!-- List exact commands and live-cluster coverage. Do not imply live validation when only unit tests ran. -->

## Checklist

- [ ] Tests cover the changed behavior.
- [ ] Existing ecosystem alternatives were reused or the missing runtime-boundary capability is documented.
- [ ] Missing or partial data is not represented as zero.
- [ ] Documentation and Helm values are updated when user-facing behavior changes.
- [ ] Generated experiment output is not committed under `log/`.
