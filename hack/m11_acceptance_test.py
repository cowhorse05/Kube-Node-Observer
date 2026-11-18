import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from contextlib import redirect_stdout


MODULE_PATH = Path(__file__).with_name("m11_acceptance.py")
SPEC = importlib.util.spec_from_file_location("m11_acceptance", MODULE_PATH)
M11 = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(M11)


def doctor_report():
    checks = []
    for name in sorted(M11.DOCTOR_CHECKS):
        checks.append(
            {
                "name": name,
                "status": "pass",
                "summary": f"{name} is ready",
                "evidence": ["fixture=true"],
                "remediation": [],
            }
        )
    return {
        "schemaVersion": "kno.doctor/v1",
        "checkedAt": "2026-11-16T00:00:00Z",
        "endpoint": "http://127.0.0.1:18080",
        "overall": "pass",
        "checks": checks,
    }


def inspect_report(runtime="runc", namespace="default", pod_name="demo"):
    kata = runtime == "kata"
    pod_uid = "pod-uid-1"
    report = {
        "schemaVersion": "kno.inspect-pod/v1",
        "generatedAt": "2026-11-16T00:00:00Z",
        "pod": {
            "namespace": namespace,
            "name": pod_name,
            "uid": pod_uid,
            "node": "minikube",
            "runtime": runtime,
        },
        "topology": {
            "generatedAt": "2026-11-16T00:00:00Z",
            "containers": [
                {"id": "container-1", "namespace": namespace, "podName": pod_name}
            ],
            "sandboxes": [
                {"id": "sandbox-1", "namespace": namespace, "podName": pod_name}
            ] if kata else [],
        },
        "host": {
            "domain": "host",
            "items": [{
                "kind": "kata-sandbox" if kata else "runc-container",
                "namespace": namespace,
                "podName": pod_name,
            }],
        },
        "warnings": [],
    }
    if kata:
        report["guest"] = {
            "domain": "guest",
            "podUid": pod_uid,
            "namespace": namespace,
            "podName": pod_name,
            "processes": [],
        }
    return report


class ValidatorTests(unittest.TestCase):
    def test_valid_doctor_and_runc(self):
        errors, warnings = M11.validate_doctor(doctor_report())
        self.assertEqual(errors, [])
        self.assertEqual(warnings, [])
        errors, warnings = M11.validate_inspect(inspect_report(), "default", "demo", "runc")
        self.assertEqual(errors, [])
        self.assertEqual(warnings, [])

    def test_doctor_rejects_status_mismatch_and_failure(self):
        report = doctor_report()
        report["checks"][0]["status"] = "fail"
        report["checks"][0]["remediation"] = ["repair it"]
        errors, _ = M11.validate_doctor(report)
        self.assertTrue(any("check failed" in error for error in errors))
        self.assertTrue(any("does not match" in error for error in errors))

    def test_runtime_and_guest_boundaries(self):
        report = inspect_report("kata")
        report["guest"]["domain"] = "host"
        errors, _ = M11.validate_inspect(report, "default", "demo", "kata", require_guest=True)
        self.assertTrue(any("guest.domain" in error for error in errors))

        runc = inspect_report()
        runc["pod"]["runtime"] = "kata"
        errors, _ = M11.validate_inspect(runc, "default", "demo", "runc")
        self.assertTrue(any("runtime must be runc" in error for error in errors))

    def test_strict_mode_rejects_warnings(self):
        report = inspect_report()
        report["warnings"] = [{"code": "partial", "message": "sample is partial"}]
        errors, warnings = M11.validate_inspect(
            report, "default", "demo", "runc", strict_warnings=True
        )
        self.assertEqual(len(warnings), 1)
        self.assertTrue(any("strict mode rejects" in error for error in errors))

    def test_main_writes_failed_summary_for_nonzero_command(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            doctor_path = root / "doctor.json"
            inspect_path = root / "inspect.json"
            output_path = root / "summary.json"
            doctor_path.write_text(json.dumps(doctor_report()), encoding="utf-8")
            inspect_path.write_text(json.dumps(inspect_report()), encoding="utf-8")
            with redirect_stdout(io.StringIO()):
                status = M11.main(
                    [
                        "--doctor", str(doctor_path), "--doctor-exit", "1",
                        "--runc", str(inspect_path), "--runc-exit", "0",
                        "--runc-namespace", "default", "--runc-pod", "demo",
                        "--output", str(output_path),
                    ]
                )
            self.assertEqual(status, 1)
            summary = json.loads(output_path.read_text(encoding="utf-8"))
            self.assertFalse(summary["accepted"])
            self.assertIn("kno doctor exited with 1", summary["errors"])


class HarnessTests(unittest.TestCase):
    def test_harness_captures_and_validates_full_evidence(self):
        repository = Path(__file__).resolve().parent.parent
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fake = root / "kno"
            fake.write_text(
                "#!/usr/bin/env python3\n"
                "import json, sys\n"
                f"doctor={doctor_report()!r}\n"
                f"inspect={inspect_report()!r}\n"
                f"kata={inspect_report('kata', 'kata-ns', 'kata-demo')!r}\n"
                "result = doctor if 'doctor' in sys.argv else "
                "kata if 'kata-demo' in sys.argv else inspect\n"
                "json.dump(result, sys.stdout)\n",
                encoding="utf-8",
            )
            fake.chmod(0o755)
            output = root / "evidence"
            environment = os.environ.copy()
            environment.update(
                {
                    "KNO_BIN": str(fake),
                    "M11_RUNC_NAMESPACE": "default",
                    "M11_RUNC_POD": "demo",
                    "M11_KATA_NAMESPACE": "kata-ns",
                    "M11_KATA_POD": "kata-demo",
                    "M11_REQUIRE_GUEST": "true",
                    "M11_OUTPUT_DIR": str(output),
                }
            )
            completed = subprocess.run(
                ["bash", "hack/m11-acceptance.sh"],
                cwd=repository,
                env=environment,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
            self.assertEqual(completed.returncode, 0, completed.stderr)
            summary = json.loads((output / "summary.json").read_text(encoding="utf-8"))
            self.assertTrue(summary["accepted"])
            self.assertEqual(summary["coverage"], "runc+kata")
            self.assertTrue(summary["requireGuest"])
            self.assertTrue((output / "manifest.json").is_file())
            self.assertTrue((output / "inspect-kata.json").is_file())


if __name__ == "__main__":
    unittest.main()
