package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cowhorse05/KubeNode-Observer/internal/agent"
	"github.com/cowhorse05/KubeNode-Observer/internal/topology"
)

const (
	doctorSchemaVersion = "kno.doctor/v1"
	doctorResponseLimit = 16 << 20
)

type doctorStatus string

const (
	doctorPass doctorStatus = "pass"
	doctorWarn doctorStatus = "warn"
	doctorFail doctorStatus = "fail"
	doctorSkip doctorStatus = "skip"
)

type doctorCheck struct {
	Name        string       `json:"name"`
	Status      doctorStatus `json:"status"`
	Summary     string       `json:"summary"`
	Evidence    []string     `json:"evidence"`
	Remediation []string     `json:"remediation"`
}

type doctorReport struct {
	SchemaVersion string        `json:"schemaVersion"`
	CheckedAt     time.Time     `json:"checkedAt"`
	Endpoint      string        `json:"endpoint"`
	Overall       doctorStatus  `json:"overall"`
	Checks        []doctorCheck `json:"checks"`
}

type doctorMetrics struct {
	mapReady        map[string]float64
	mapRefreshError map[string]float64
	collectionError float64
	hasCollection   bool
}

func newDoctorCommand(endpoint *string) *cobra.Command {
	var output string
	var timeout time.Duration
	command := &cobra.Command{
		Use:   "doctor",
		Short: "Check Agent, eBPF, cgroup, CRI, Kata and node-read permissions",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if timeout < time.Second || timeout > time.Minute {
				return fmt.Errorf("timeout must be between 1s and 1m")
			}
			if output != "text" && output != "json" {
				return fmt.Errorf("output must be text or json")
			}
			ctx, cancel := context.WithTimeout(command.Context(), timeout)
			defer cancel()
			report := collectDoctor(ctx, *endpoint)
			var err error
			if output == "json" {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetIndent("", "  ")
				err = encoder.Encode(report)
			} else {
				err = writeDoctorText(command.OutOrStdout(), report)
			}
			if err != nil {
				return err
			}
			if report.Overall == doctorFail {
				// The report is the diagnostic payload. Keep stdout valid (especially
				// for JSON) and let main print only the concise non-zero result.
				command.Root().SilenceUsage = true
				command.Root().SilenceErrors = true
				return fmt.Errorf("doctor found failing checks")
			}
			return nil
		},
	}
	command.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	command.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "overall diagnostic request timeout (1s..1m)")
	return command
}

func collectDoctor(ctx context.Context, endpoint string) doctorReport {
	report := doctorReport{
		SchemaVersion: doctorSchemaVersion,
		CheckedAt:     time.Now().UTC(),
		Endpoint:      strings.TrimRight(endpoint, "/"),
		Checks:        []doctorCheck{},
	}

	var (
		healthResponse   agent.HealthResponse
		topologyResponse topology.Response
		metricsBody      string
		healthErr        error
		topologyErr      error
		metricsErr       error
	)
	var group sync.WaitGroup
	group.Add(3)
	go func() {
		defer group.Done()
		healthErr = inspectGet(ctx, endpoint, "/healthz", doctorResponseLimit, &healthResponse)
	}()
	go func() {
		defer group.Done()
		topologyErr = inspectGet(ctx, endpoint, "/api/v1/topology", doctorResponseLimit, &topologyResponse)
	}()
	go func() {
		defer group.Done()
		metricsBody, metricsErr = doctorGetText(ctx, endpoint, "/metrics", doctorResponseLimit)
	}()
	group.Wait()

	metrics := parseDoctorMetrics(metricsBody)
	report.Checks = append(report.Checks,
		doctorAgentCheck(healthResponse, healthErr),
		doctorEBPFCheck(healthResponse, healthErr, metrics, metricsErr),
		doctorLifecycleCheck(healthResponse, healthErr),
		doctorCRICheck(topologyResponse, topologyErr),
		doctorCgroupCheck(topologyResponse, topologyErr),
		doctorKubernetesCheck(topologyResponse, topologyErr),
		doctorKataCheck(topologyResponse, topologyErr),
		doctorPermissionCheck(healthResponse, healthErr, topologyResponse, topologyErr, metrics, metricsErr),
	)
	report.Overall = doctorPass
	for _, check := range report.Checks {
		if check.Status == doctorFail {
			report.Overall = doctorFail
			break
		}
		if check.Status == doctorWarn && report.Overall == doctorPass {
			report.Overall = doctorWarn
		}
	}
	return report
}

func doctorAgentCheck(health agent.HealthResponse, err error) doctorCheck {
	check := doctorCheck{Name: "agent", Evidence: []string{}, Remediation: []string{}}
	if err != nil {
		check.Status, check.Summary = doctorFail, "kno-agent is unreachable or unhealthy"
		check.Evidence = append(check.Evidence, err.Error())
		check.Remediation = append(check.Remediation,
			"verify the kno-agent Pod is Ready and has not restarted",
			"start or repair the port-forward, then pass its URL with --endpoint",
		)
		return check
	}
	check.Evidence = append(check.Evidence, "component="+health.Component, "status="+health.Status, "uptime="+health.Uptime)
	if health.Status != "ok" || health.Component != "kno-agent" {
		check.Status, check.Summary = doctorFail, "health response does not identify a healthy kno-agent"
		check.Remediation = append(check.Remediation, "inspect kno-agent logs and confirm --endpoint targets the Agent service")
		return check
	}
	check.Status, check.Summary = doctorPass, "kno-agent health endpoint is ready"
	return check
}

func doctorEBPFCheck(health agent.HealthResponse, healthErr error, metrics doctorMetrics, metricsErr error) doctorCheck {
	check := doctorCheck{Name: "ebpf", Evidence: []string{}, Remediation: []string{}}
	if healthErr != nil {
		check.Status, check.Summary = doctorFail, "eBPF state cannot be verified without Agent health"
		check.Remediation = append(check.Remediation, "restore kno-agent connectivity first")
		return check
	}
	check.Evidence = append(check.Evidence, "ebpfLoaded="+strconv.FormatBool(health.EBPFLoaded))
	if !health.EBPFLoaded {
		check.Status, check.Summary = doctorFail, "Agent reports that eBPF probes are not loaded"
		check.Remediation = append(check.Remediation,
			"confirm the node kernel exposes BTF and tracefs",
			"confirm the DaemonSet retains its required privileged/BPF permissions and host mounts",
		)
		return check
	}
	if metricsErr != nil {
		check.Status, check.Summary = doctorWarn, "probes are loaded but BPF map health metrics are unavailable"
		check.Evidence = append(check.Evidence, metricsErr.Error())
		check.Remediation = append(check.Remediation, "query /metrics and inspect kno-agent logs for metrics collection failures")
		return check
	}
	var unhealthy []string
	for _, source := range []string{"exec", "cpu", "runqlat"} {
		ready, present := metrics.mapReady[source]
		refreshError, refreshPresent := metrics.mapRefreshError[source]
		check.Evidence = append(check.Evidence, fmt.Sprintf("%s.ready=%g %s.refreshError=%g", source, ready, source, refreshError))
		if !present || !refreshPresent || ready != 1 || refreshError != 0 {
			unhealthy = append(unhealthy, source)
		}
	}
	if len(unhealthy) > 0 {
		check.Status, check.Summary = doctorWarn, "one or more BPF map censuses are not ready or recently failed: "+strings.Join(unhealthy, ", ")
		check.Remediation = append(check.Remediation,
			"wait at least one map-stats interval and rerun doctor",
			"if the warning persists, inspect map capacity/memlock metrics and Agent logs",
		)
		return check
	}
	check.Status, check.Summary = doctorPass, "exec, CPU and runqlat probes are loaded and their map censuses are ready"
	return check
}

func doctorLifecycleCheck(health agent.HealthResponse, err error) doctorCheck {
	check := doctorCheck{Name: "lifecycle-events", Evidence: []string{}, Remediation: []string{}}
	if err != nil {
		check.Status, check.Summary = doctorSkip, "lifecycle loss counters are unavailable"
		check.Remediation = append(check.Remediation, "restore Agent health and rerun doctor")
		return check
	}
	check.Evidence = append(check.Evidence, fmt.Sprintf("eventsSeen=%d eventsLost=%d", health.EventsSeen, health.EventsLost))
	if health.EventsLost > 0 {
		check.Status, check.Summary = doctorWarn, "the lifecycle ring buffer has reported reservation failures"
		check.Remediation = append(check.Remediation,
			"inspect workload exec/exit churn and Agent CPU throttling",
			"treat recent lifecycle history as incomplete; do not infer that CPU or runqlat data was also lost",
		)
		return check
	}
	check.Status, check.Summary = doctorPass, "no lifecycle ring-buffer reservation failure is reported"
	return check
}

func doctorCRICheck(response topology.Response, err error) doctorCheck {
	check := doctorCheck{Name: "cri-topology", Evidence: []string{}, Remediation: []string{}}
	if err != nil {
		check.Status, check.Summary = doctorFail, "current CRI topology is unavailable"
		check.Evidence = append(check.Evidence, err.Error())
		check.Remediation = append(check.Remediation,
			"confirm the containerd CRI socket is mounted and --runtime-endpoint is correct",
			"inspect kno-agent logs for ListPodSandbox/ListContainers errors",
		)
		return check
	}
	if response.Snapshot == nil {
		check.Status, check.Summary = doctorFail, "topology response has no current snapshot"
		check.Remediation = append(check.Remediation, "wait for the initial topology refresh and inspect Agent logs")
		return check
	}
	check.Evidence = append(check.Evidence,
		"generatedAt="+response.Snapshot.GeneratedAt.UTC().Format(time.RFC3339),
		fmt.Sprintf("containers=%d sandboxes=%d", len(response.Snapshot.Containers), len(response.Snapshot.Sandboxes)),
	)
	if response.Error != "" {
		check.Status, check.Summary = doctorFail, "the last topology refresh failed"
		check.Evidence = append(check.Evidence, response.Error)
		check.Remediation = append(check.Remediation, "repair CRI connectivity or runtime permissions, then wait for a successful refresh")
		return check
	}
	if response.Snapshot.GeneratedAt.IsZero() || time.Since(response.Snapshot.GeneratedAt) > 30*time.Second {
		check.Status, check.Summary = doctorFail, "topology snapshot is missing or stale"
		check.Remediation = append(check.Remediation, "inspect the Agent refresh loop and container runtime responsiveness")
		return check
	}
	check.Status, check.Summary = doctorPass, "CRI topology refresh is current"
	return check
}

func doctorCgroupCheck(response topology.Response, err error) doctorCheck {
	check := doctorCheck{Name: "cgroup-v2", Evidence: []string{}, Remediation: []string{}}
	if err != nil || response.Snapshot == nil || response.Error != "" {
		check.Status, check.Summary = doctorSkip, "cgroup attribution cannot be checked without current topology"
		check.Remediation = append(check.Remediation, "repair the CRI topology check first")
		return check
	}
	var targets, missing int
	for _, container := range response.Snapshot.Containers {
		if container.ProcessVisibility != "host" {
			continue
		}
		targets++
		if container.CgroupPath == "" || container.CgroupID == 0 {
			missing++
			check.Evidence = append(check.Evidence, fmt.Sprintf("%s/%s: cgroupPath=%q cgroupId=%d", container.Namespace, container.PodName, container.CgroupPath, container.CgroupID))
		}
	}
	if targets == 0 {
		check.Status, check.Summary = doctorWarn, "no Host-visible runc container is available to verify cgroup v2 attribution"
		check.Remediation = append(check.Remediation, "run a small runc Pod on this node and rerun doctor")
		return check
	}
	check.Evidence = append(check.Evidence, fmt.Sprintf("hostVisibleContainers=%d missingAttribution=%d", targets, missing))
	if missing > 0 {
		check.Status, check.Summary = doctorFail, "one or more runc containers lack a cgroup v2 path or inode ID"
		check.Remediation = append(check.Remediation,
			"confirm the node uses cgroup v2",
			"confirm /sys/fs/cgroup is mounted at the Agent --cgroup-root and /host/proc matches the node PID namespace",
		)
		return check
	}
	check.Status, check.Summary = doctorPass, "Host-visible containers have cgroup v2 paths and inode IDs"
	return check
}

func doctorKubernetesCheck(response topology.Response, err error) doctorCheck {
	check := doctorCheck{Name: "kubernetes", Evidence: []string{}, Remediation: []string{}}
	if err != nil || response.Snapshot == nil {
		check.Status, check.Summary = doctorSkip, "Kubernetes informer identity cannot be checked without topology"
		check.Remediation = append(check.Remediation, "repair topology connectivity first")
		return check
	}
	check.Evidence = append(check.Evidence, "nodeName="+response.Snapshot.NodeName, "nodeUid="+response.Snapshot.NodeUID)
	if response.Snapshot.NodeName == "" || response.Snapshot.NodeUID == "" {
		check.Status, check.Summary = doctorWarn, "node identity is incomplete; the Kubernetes informer may be unavailable or unsynchronized"
		check.Remediation = append(check.Remediation,
			"confirm NODE_NAME matches the DaemonSet node and the service account can get/list/watch Nodes and Pods",
			"inspect Agent startup logs for informer synchronization failures",
		)
		return check
	}
	check.Status, check.Summary = doctorPass, "Kubernetes informer resolved the current node identity"
	return check
}

func doctorKataCheck(response topology.Response, err error) doctorCheck {
	check := doctorCheck{Name: "kata", Evidence: []string{}, Remediation: []string{}}
	if err != nil || response.Snapshot == nil || response.Error != "" {
		check.Status, check.Summary = doctorSkip, "Kata identity cannot be checked without current topology"
		check.Remediation = append(check.Remediation, "repair the CRI topology check first")
		return check
	}
	if len(response.Snapshot.Sandboxes) == 0 {
		check.Status, check.Summary = doctorSkip, "no current Kata sandbox is available for runtime validation"
		check.Remediation = append(check.Remediation, "create a small Pod with the intended Kata RuntimeClass and rerun doctor")
		return check
	}
	invalid := 0
	for _, sandbox := range response.Snapshot.Sandboxes {
		valid := sandbox.RuntimeHandler != "" && sandbox.Shim != nil && sandbox.Hypervisor != nil && len(sandbox.Issues) == 0
		check.Evidence = append(check.Evidence, fmt.Sprintf("%s handler=%s shim=%t vmm=%t issues=%d", shortID(sandbox.ID), sandbox.RuntimeHandler, sandbox.Shim != nil, sandbox.Hypervisor != nil, len(sandbox.Issues)))
		if !valid {
			invalid++
		}
	}
	if invalid > 0 {
		check.Status, check.Summary = doctorFail, "one or more current Kata sandboxes lack a verified shim/VMM identity"
		check.Remediation = append(check.Remediation,
			"confirm the RuntimeClass handler matches the installed Kata runtime",
			"confirm the Agent uses hostPID and can read node procfs/cgroups; inspect sandbox issues in kno topology",
		)
		return check
	}
	check.Status, check.Summary = doctorPass, "current Kata sandboxes have verified shim and VMM identities"
	return check
}

func doctorPermissionCheck(health agent.HealthResponse, healthErr error, response topology.Response, topologyErr error, metrics doctorMetrics, metricsErr error) doctorCheck {
	check := doctorCheck{Name: "permissions", Evidence: []string{}, Remediation: []string{}}
	var permissionIssues []string
	if topologyErr == nil {
		for _, issue := range topologyIssues(response) {
			lower := strings.ToLower(issue)
			if strings.Contains(lower, "permission denied") || strings.Contains(lower, "operation not permitted") {
				permissionIssues = append(permissionIssues, issue)
			}
		}
	}
	if len(permissionIssues) > 0 {
		check.Status, check.Summary = doctorFail, "topology contains explicit node-read permission failures"
		check.Evidence = append(check.Evidence, permissionIssues...)
		check.Remediation = append(check.Remediation,
			"restore the documented privileged DaemonSet security context, hostPID and read-only host procfs/cgroup mounts",
			"check Pod Security admission and runtime policies for denied BPF/procfs access",
		)
		return check
	}
	if metricsErr == nil && metrics.hasCollection {
		check.Evidence = append(check.Evidence, fmt.Sprintf("metricsCollectionErrors=%g", metrics.collectionError))
		if metrics.collectionError > 0 {
			check.Status, check.Summary = doctorWarn, "Agent metrics report one or more current collection failures"
			check.Remediation = append(check.Remediation, "inspect /metrics map source readiness and Agent logs; collection failure is not automatically a permission failure")
			return check
		}
	}
	if metricsErr == nil && !metrics.hasCollection {
		check.Status, check.Summary = doctorWarn, "permission health is only partially verified because the metrics collection status is missing"
		check.Remediation = append(check.Remediation, "confirm --endpoint targets a compatible kno-agent and inspect its /metrics response")
		return check
	}
	if healthErr != nil || topologyErr != nil || metricsErr != nil {
		check.Status, check.Summary = doctorWarn, "permission health is only partially verified because one or more evidence endpoints failed"
		check.Remediation = append(check.Remediation, "repair Agent, topology and metrics endpoints, then rerun doctor")
		return check
	}
	if !health.EBPFLoaded {
		check.Status, check.Summary = doctorWarn, "node-read permissions cannot be considered healthy while probes are unloaded"
		check.Remediation = append(check.Remediation, "repair the eBPF check first")
		return check
	}
	check.Status, check.Summary = doctorPass, "no explicit permission failure is present in current probe, topology or metrics evidence"
	return check
}

func topologyIssues(response topology.Response) []string {
	issues := []string{}
	if response.Error != "" {
		issues = append(issues, response.Error)
	}
	if response.Snapshot == nil {
		return issues
	}
	for _, container := range response.Snapshot.Containers {
		issues = append(issues, container.Issues...)
	}
	for _, sandbox := range response.Snapshot.Sandboxes {
		issues = append(issues, sandbox.Issues...)
	}
	return issues
}

func doctorGetText(ctx context.Context, endpoint, path string, limit int64) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+path, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(body)) > limit {
		return "", fmt.Errorf("response exceeds %s", humanBytes(uint64(limit)))
	}
	if response.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(body))
		if len(message) > 512 {
			message = message[:512] + "..."
		}
		return "", fmt.Errorf("agent returned %s: %s", response.Status, message)
	}
	return string(body), nil
}

func parseDoctorMetrics(body string) doctorMetrics {
	result := doctorMetrics{mapReady: map[string]float64{}, mapRefreshError: map[string]float64{}}
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		name := fields[0]
		switch {
		case strings.HasPrefix(name, "kno_agent_bpf_map_stats_ready{"):
			if source := doctorMetricLabel(name, "source"); source != "" {
				result.mapReady[source] = value
			}
		case strings.HasPrefix(name, "kno_agent_bpf_map_stats_refresh_error{"):
			if source := doctorMetricLabel(name, "source"); source != "" {
				result.mapRefreshError[source] = value
			}
		case name == "kno_agent_metrics_collection_errors":
			result.collectionError, result.hasCollection = value, true
		}
	}
	return result
}

func doctorMetricLabel(sample, label string) string {
	needle := label + "=\""
	start := strings.Index(sample, needle)
	if start < 0 {
		return ""
	}
	start += len(needle)
	end := strings.Index(sample[start:], "\"")
	if end < 0 {
		return ""
	}
	return sample[start : start+end]
}

func writeDoctorText(writer io.Writer, report doctorReport) error {
	tw := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "KNO DOCTOR\t%s\n", strings.ToUpper(string(report.Overall)))
	fmt.Fprintf(tw, "ENDPOINT\t%s\n", report.Endpoint)
	fmt.Fprintf(tw, "CHECKED AT\t%s\n\n", report.CheckedAt.Format(time.RFC3339))
	fmt.Fprintln(tw, "CHECK\tSTATUS\tSUMMARY")
	for _, check := range report.Checks {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", check.Name, strings.ToUpper(string(check.Status)), check.Summary)
	}
	fmt.Fprintln(tw, "\nREMEDIATION")
	wrote := false
	for _, check := range report.Checks {
		if len(check.Remediation) == 0 {
			continue
		}
		wrote = true
		for index, action := range check.Remediation {
			prefix := ""
			if index == 0 {
				prefix = "[" + check.Name + "]"
			}
			fmt.Fprintf(tw, "%s\t%s\n", prefix, action)
		}
	}
	if !wrote {
		fmt.Fprintln(tw, "none")
	}
	return tw.Flush()
}
