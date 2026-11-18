import json
import shutil
import subprocess
from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parent.parent
CHART = ROOT / "charts" / "kubenode-observer"


class EcosystemIntegrationTests(unittest.TestCase):
    def test_otel_collector_example_reuses_standard_components(self):
        config = (
            ROOT / "deploy" / "otel-collector" / "kubenode-observer.yaml"
        ).read_text(encoding="utf-8")

        self.assertIn("prometheus/kubenode-observer:", config)
        self.assertIn("kubernetes_sd_configs:", config)
        self.assertIn("memory_limiter:", config)
        self.assertIn("batch:", config)
        self.assertIn("endpoint: ${env:OTLP_ENDPOINT}", config)
        self.assertIn("otlp/upstream:", config)


@unittest.skipUnless(shutil.which("helm"), "helm is not installed")
class HelmChartTests(unittest.TestCase):
    def helm(self, *arguments, expected=0):
        completed = subprocess.run(
            ["helm", *arguments],
            cwd=ROOT,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        self.assertEqual(completed.returncode, expected, completed.stdout + completed.stderr)
        return completed

    def test_chart_lints_and_renders_runtime_mounts(self):
        self.helm("lint", str(CHART))
        rendered = self.helm(
            "template", "kno", str(CHART), "--namespace", "kno-system"
        ).stdout
        self.assertIn("kind: DaemonSet", rendered)
        self.assertIn("kind: ClusterRole", rendered)
        self.assertIn("hostPID: true", rendered)
        self.assertIn("privileged: true", rendered)
        self.assertIn("--runtime-endpoint=unix:///run/containerd/containerd.sock", rendered)
        self.assertIn("path: /run/containerd/containerd.sock", rendered)
        self.assertNotIn("kind: ServiceMonitor", rendered)
        self.assertNotIn("kind: PrometheusRule", rendered)
        self.assertNotIn("grafana_dashboard", rendered)

    def test_service_monitor_is_opt_in(self):
        rendered = self.helm(
            "template",
            "kno",
            str(CHART),
            "--namespace",
            "observability",
            "--set",
            "serviceMonitor.enabled=true",
        ).stdout
        self.assertIn("kind: ServiceMonitor", rendered)
        self.assertIn("namespace: observability", rendered)
        self.assertIn("port: http", rendered)

    def test_monitoring_integrations_are_opt_in_and_reuse_standard_resources(self):
        rendered = self.helm(
            "template",
            "kno",
            str(CHART),
            "--namespace",
            "observability",
            "--set",
            "serviceMonitor.enabled=true",
            "--set",
            "prometheusRule.enabled=true",
            "--set",
            "grafanaDashboard.enabled=true",
        ).stdout
        self.assertIn("kind: PrometheusRule", rendered)
        self.assertIn("alert: KubeNodeObserverTopologyUnavailable", rendered)
        self.assertIn("grafana_dashboard: \"1\"", rendered)
        self.assertIn("KubeNode-Observer / Runtime boundaries", rendered)

    def test_grafana_dashboard_is_valid_json(self):
        dashboard = CHART / "dashboards" / "kubenode-observer.json"
        content = json.loads(dashboard.read_text(encoding="utf-8"))
        self.assertEqual(content["uid"], "kubenode-observer-runtime")
        self.assertGreaterEqual(len(content["panels"]), 6)

    def test_values_schema_rejects_invalid_pull_policy(self):
        completed = self.helm(
            "template",
            "kno",
            str(CHART),
            "--set",
            "image.pullPolicy=Sometimes",
            expected=1,
        )
        self.assertRegex(completed.stderr, r"(?:image\.pullPolicy|/image/pullPolicy)")


if __name__ == "__main__":
    unittest.main()
