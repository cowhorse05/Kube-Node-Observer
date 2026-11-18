#!/usr/bin/env python3
"""Validate the saved M11 doctor and inspect-pod evidence."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any


DOCTOR_CHECKS = {
    "agent",
    "ebpf",
    "lifecycle-events",
    "cri-topology",
    "cgroup-v2",
    "kubernetes",
    "kata",
    "permissions",
}
DOCTOR_STATUSES = {"pass", "warn", "fail", "skip"}


def _load(path: Path) -> tuple[dict[str, Any] | None, str | None]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        return None, f"cannot read JSON {path}: {error}"
    if not isinstance(value, dict):
        return None, f"{path} must contain a JSON object"
    return value, None


def _nonempty_string(value: Any) -> bool:
    return isinstance(value, str) and bool(value.strip())


def validate_doctor(report: dict[str, Any], strict_warnings: bool = False) -> tuple[list[str], list[str]]:
    errors: list[str] = []
    warnings: list[str] = []
    if report.get("schemaVersion") != "kno.doctor/v1":
        errors.append("doctor schemaVersion must be kno.doctor/v1")
    overall = report.get("overall")
    if overall not in DOCTOR_STATUSES:
        errors.append("doctor overall must be pass, warn, fail, or skip")

    checks = report.get("checks")
    if not isinstance(checks, list):
        return errors + ["doctor checks must be an array"], warnings

    seen: set[str] = set()
    statuses: list[str] = []
    for index, check in enumerate(checks):
        label = f"doctor checks[{index}]"
        if not isinstance(check, dict):
            errors.append(f"{label} must be an object")
            continue
        name = check.get("name")
        status = check.get("status")
        if not _nonempty_string(name):
            errors.append(f"{label}.name must be a non-empty string")
        elif name in seen:
            errors.append(f"doctor check name is duplicated: {name}")
        else:
            seen.add(name)
        if status not in DOCTOR_STATUSES:
            errors.append(f"{label}.status is invalid")
            continue
        statuses.append(status)
        if not _nonempty_string(check.get("summary")):
            errors.append(f"{label}.summary must be a non-empty string")
        for field in ("evidence", "remediation"):
            value = check.get(field)
            if not isinstance(value, list) or not all(_nonempty_string(item) for item in value):
                errors.append(f"{label}.{field} must be an array of non-empty strings")
        if status != "pass" and not check.get("remediation"):
            errors.append(f"{label} must include remediation when status is {status}")
        if status == "fail":
            errors.append(f"doctor check failed: {name}")
        elif status in {"warn", "skip"}:
            warnings.append(f"doctor {name}: {status}")

    missing = sorted(DOCTOR_CHECKS - seen)
    if missing:
        errors.append("doctor is missing checks: " + ", ".join(missing))
    derived = "fail" if "fail" in statuses else "warn" if "warn" in statuses else "pass"
    if overall in DOCTOR_STATUSES and overall != derived:
        errors.append(f"doctor overall {overall!r} does not match derived status {derived!r}")
    if strict_warnings and warnings:
        errors.extend("strict mode rejects " + warning for warning in warnings)
    return errors, warnings


def validate_inspect(
    report: dict[str, Any],
    namespace: str,
    pod_name: str,
    expected_runtime: str,
    require_guest: bool = False,
    strict_warnings: bool = False,
) -> tuple[list[str], list[str]]:
    errors: list[str] = []
    warnings: list[str] = []
    if report.get("schemaVersion") != "kno.inspect-pod/v1":
        errors.append("inspect schemaVersion must be kno.inspect-pod/v1")

    pod = report.get("pod")
    if not isinstance(pod, dict):
        return errors + ["inspect pod must be an object"], warnings
    if pod.get("namespace") != namespace or pod.get("name") != pod_name:
        errors.append(
            f"inspect identity mismatch: expected {namespace}/{pod_name}, "
            f"got {pod.get('namespace')}/{pod.get('name')}"
        )
    if pod.get("runtime") != expected_runtime:
        errors.append(f"inspect runtime must be {expected_runtime}, got {pod.get('runtime')!r}")
    if not _nonempty_string(pod.get("node")):
        errors.append("inspect pod.node must be a non-empty string")

    topology = report.get("topology")
    if not isinstance(topology, dict):
        errors.append("inspect topology must be an object")
        containers: list[Any] = []
        sandboxes: list[Any] = []
    else:
        containers = topology.get("containers")
        sandboxes = topology.get("sandboxes")
        if not isinstance(containers, list):
            errors.append("inspect topology.containers must be an array")
            containers = []
        if not isinstance(sandboxes, list):
            errors.append("inspect topology.sandboxes must be an array")
            sandboxes = []
    if not containers and not sandboxes:
        errors.append("inspect topology must contain a container or sandbox")
    matching_containers = [
        item
        for item in containers
        if isinstance(item, dict)
        and item.get("namespace") == namespace
        and item.get("podName") == pod_name
    ]
    matching_sandboxes = [
        item
        for item in sandboxes
        if isinstance(item, dict)
        and item.get("namespace") == namespace
        and item.get("podName") == pod_name
    ]
    if expected_runtime == "runc" and not matching_containers:
        errors.append("runc inspect must contain a container for the requested Pod")
    if expected_runtime == "kata" and not matching_sandboxes:
        errors.append("Kata inspect must contain a sandbox for the requested Pod")

    host = report.get("host")
    if not isinstance(host, dict):
        errors.append("inspect host evidence is required")
    else:
        if host.get("domain") != "host":
            errors.append("inspect host.domain must be host")
        items = host.get("items")
        if not isinstance(items, list) or not items:
            errors.append("inspect host.items must be a non-empty array")
        else:
            expected_kind = "runc-container" if expected_runtime == "runc" else "kata-sandbox"
            if not any(
                isinstance(item, dict)
                and item.get("kind") == expected_kind
                and item.get("namespace") == namespace
                and item.get("podName") == pod_name
                for item in items
            ):
                errors.append(f"inspect host.items must contain {expected_kind} for the requested Pod")

    guest = report.get("guest")
    if expected_runtime == "runc" and guest is not None:
        errors.append("runc inspect must not contain Guest evidence")
    if guest is not None:
        if not isinstance(guest, dict) or guest.get("domain") != "guest":
            errors.append("inspect guest.domain must be guest")
        elif guest.get("podUid") != pod.get("uid"):
            errors.append("inspect Guest Pod UID must match the inspected Pod UID")
        elif guest.get("namespace") != namespace or guest.get("podName") != pod_name:
            errors.append("inspect Guest namespace and Pod name must match the inspected Pod")
    if expected_runtime == "kata" and require_guest and guest is None:
        errors.append("Kata Guest evidence is required but absent")

    warning_items = report.get("warnings")
    if not isinstance(warning_items, list):
        errors.append("inspect warnings must be an array")
    else:
        for index, warning in enumerate(warning_items):
            if not isinstance(warning, dict) or not _nonempty_string(warning.get("code")) or not _nonempty_string(warning.get("message")):
                errors.append(f"inspect warnings[{index}] must contain non-empty code and message")
                continue
            warnings.append(f"inspect {namespace}/{pod_name}: {warning['code']}")
    if strict_warnings and warnings:
        errors.extend("strict mode rejects " + warning for warning in warnings)
    return errors, warnings


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--doctor", required=True, type=Path)
    parser.add_argument("--doctor-exit", required=True, type=int)
    parser.add_argument("--runc", required=True, type=Path)
    parser.add_argument("--runc-exit", required=True, type=int)
    parser.add_argument("--runc-namespace", required=True)
    parser.add_argument("--runc-pod", required=True)
    parser.add_argument("--kata", type=Path)
    parser.add_argument("--kata-exit", type=int)
    parser.add_argument("--kata-namespace")
    parser.add_argument("--kata-pod")
    parser.add_argument("--require-guest", action="store_true")
    parser.add_argument("--strict-warnings", action="store_true")
    parser.add_argument("--output", required=True, type=Path)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    errors: list[str] = []
    warnings: list[str] = []

    doctor, error = _load(args.doctor)
    if error:
        errors.append(error)
    else:
        doctor_errors, doctor_warnings = validate_doctor(doctor, args.strict_warnings)
        errors.extend(doctor_errors)
        warnings.extend(doctor_warnings)
    if args.doctor_exit != 0:
        errors.append(f"kno doctor exited with {args.doctor_exit}")

    runc, error = _load(args.runc)
    if error:
        errors.append(error)
    else:
        inspect_errors, inspect_warnings = validate_inspect(
            runc, args.runc_namespace, args.runc_pod, "runc", False, args.strict_warnings
        )
        errors.extend(inspect_errors)
        warnings.extend(inspect_warnings)
    if args.runc_exit != 0:
        errors.append(f"runc inspect exited with {args.runc_exit}")

    kata_requested = args.kata is not None
    if kata_requested:
        if args.kata_exit is None or not args.kata_namespace or not args.kata_pod:
            errors.append("Kata evidence requires --kata-exit, --kata-namespace, and --kata-pod")
        else:
            kata, error = _load(args.kata)
            if error:
                errors.append(error)
            else:
                inspect_errors, inspect_warnings = validate_inspect(
                    kata,
                    args.kata_namespace,
                    args.kata_pod,
                    "kata",
                    args.require_guest,
                    args.strict_warnings,
                )
                errors.extend(inspect_errors)
                warnings.extend(inspect_warnings)
            if args.kata_exit != 0:
                errors.append(f"Kata inspect exited with {args.kata_exit}")

    summary = {
        "schemaVersion": "kno.m11-acceptance/v1",
        "accepted": not errors,
        "coverage": "runc+kata" if kata_requested else "runc-only",
        "strictWarnings": args.strict_warnings,
        "requireGuest": args.require_guest,
        "inputs": {
            "doctor": str(args.doctor),
            "runc": f"{args.runc_namespace}/{args.runc_pod}",
            "kata": f"{args.kata_namespace}/{args.kata_pod}" if kata_requested else None,
        },
        "commandExits": {
            "doctor": args.doctor_exit,
            "runcInspect": args.runc_exit,
            "kataInspect": args.kata_exit if kata_requested else None,
        },
        "errors": errors,
        "warnings": warnings,
    }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    return 0 if not errors else 1


if __name__ == "__main__":
    sys.exit(main())
