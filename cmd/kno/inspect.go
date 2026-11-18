package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cowhorse05/KubeNode-Observer/internal/agent"
	"github.com/cowhorse05/KubeNode-Observer/internal/cpu"
	eventstore "github.com/cowhorse05/KubeNode-Observer/internal/event"
	"github.com/cowhorse05/KubeNode-Observer/internal/guest"
	"github.com/cowhorse05/KubeNode-Observer/internal/host"
	"github.com/cowhorse05/KubeNode-Observer/internal/topology"
)

const (
	inspectSchemaVersion = "kno.inspect-pod/v1"
	inspectResponseLimit = 16 << 20
)

type inspectWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type inspectPod struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
	Node      string `json:"node"`
	Runtime   string `json:"runtime"`
	Phase     string `json:"phase,omitempty"`
	QoSClass  string `json:"qosClass,omitempty"`
}

type inspectTopology struct {
	GeneratedAt time.Time            `json:"generatedAt"`
	Containers  []topology.Container `json:"containers"`
	Sandboxes   []topology.Sandbox   `json:"sandboxes"`
}

type inspectReport struct {
	SchemaVersion string                `json:"schemaVersion"`
	GeneratedAt   time.Time             `json:"generatedAt"`
	Pod           inspectPod            `json:"pod"`
	Topology      inspectTopology       `json:"topology"`
	Host          *host.Response        `json:"host,omitempty"`
	Processes     *cpu.Response         `json:"hostProcesses,omitempty"`
	Guest         *guest.Response       `json:"guest,omitempty"`
	Events        *eventstore.Response  `json:"events,omitempty"`
	Health        *agent.HealthResponse `json:"health,omitempty"`
	Warnings      []inspectWarning      `json:"warnings"`
}

type inspectOptions struct {
	namespace string
	output    string
	duration  time.Duration
	limit     int
}

func newInspectCommand(endpoint *string) *cobra.Command {
	inspect := &cobra.Command{
		Use:   "inspect",
		Short: "Combine topology, resources, processes and events into one diagnosis view",
	}
	options := inspectOptions{}
	pod := &cobra.Command{
		Use:   "pod <pod>",
		Short: "Inspect one runc or Kata Pod",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if options.duration < 100*time.Millisecond || options.duration > 10*time.Second {
				return fmt.Errorf("duration must be between 100ms and 10s")
			}
			if options.limit < 1 || options.limit > 100 {
				return fmt.Errorf("limit must be between 1 and 100")
			}
			if options.output != "text" && options.output != "json" {
				return fmt.Errorf("output must be text or json")
			}
			ctx, cancel := context.WithTimeout(command.Context(), options.duration+20*time.Second)
			defer cancel()
			report, err := collectPodInspection(ctx, *endpoint, options.namespace, args[0], options.duration, options.limit)
			if err != nil {
				return err
			}
			if options.output == "json" {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(report)
			}
			return writeInspectText(command.OutOrStdout(), report)
		},
	}
	pod.Flags().StringVarP(&options.namespace, "namespace", "n", "default", "Pod namespace")
	pod.Flags().StringVarP(&options.output, "output", "o", "text", "output format: text or json")
	pod.Flags().DurationVar(&options.duration, "duration", time.Second, "Host and optional Guest measurement window")
	pod.Flags().IntVar(&options.limit, "limit", 10, "maximum processes and recent events")
	inspect.AddCommand(pod)
	return inspect
}

func collectPodInspection(ctx context.Context, endpoint, namespace, pod string, duration time.Duration, limit int) (inspectReport, error) {
	query := url.Values{"namespace": {namespace}, "pod": {pod}}
	var topologyResponse topology.Response
	if err := inspectGet(ctx, endpoint, "/api/v1/topology?"+query.Encode(), inspectResponseLimit, &topologyResponse); err != nil {
		return inspectReport{}, fmt.Errorf("read Pod topology: %w", err)
	}
	if topologyResponse.Snapshot == nil {
		return inspectReport{}, fmt.Errorf("Pod topology is unavailable")
	}
	snapshot := topologyResponse.Snapshot
	if len(snapshot.Containers) == 0 && len(snapshot.Sandboxes) == 0 {
		return inspectReport{}, fmt.Errorf("Pod %s/%s was not found in the current node topology", namespace, pod)
	}

	report := inspectReport{
		SchemaVersion: inspectSchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		Pod: inspectPod{
			Namespace: namespace,
			Name:      pod,
			Node:      snapshot.NodeName,
			Runtime:   "runc",
		},
		Topology: inspectTopology{
			GeneratedAt: snapshot.GeneratedAt,
			Containers:  snapshot.Containers,
			Sandboxes:   snapshot.Sandboxes,
		},
		Warnings: []inspectWarning{},
	}
	if inspectIsKata(snapshot) {
		report.Pod.Runtime = "kata"
	}
	if len(snapshot.Sandboxes) > 0 {
		report.Pod.UID = snapshot.Sandboxes[0].PodUID
	}
	if len(snapshot.Containers) > 0 {
		container := snapshot.Containers[0]
		if report.Pod.UID == "" {
			report.Pod.UID = container.PodUID
		}
		report.Pod.Phase = container.PodPhase
		report.Pod.QoSClass = container.QoSClass
	}
	if topologyResponse.Error != "" {
		report.warn("topology_refresh_error", topologyResponse.Error)
	}
	if len(snapshot.Sandboxes) > 1 {
		report.warn("multiple_sandboxes", fmt.Sprintf("current topology returned %d Kata sandboxes; Guest sampling uses the first", len(snapshot.Sandboxes)))
	}
	for _, container := range snapshot.Containers {
		for _, issue := range container.Issues {
			report.warn("container_topology_partial", container.Name+": "+issue)
		}
	}
	for _, sandbox := range snapshot.Sandboxes {
		for _, issue := range sandbox.Issues {
			report.warn("sandbox_topology_partial", shortID(sandbox.ID)+": "+issue)
		}
	}

	hostQuery := url.Values{
		"namespace": {namespace},
		"pod":       {pod},
		"duration":  {duration.String()},
		"limit":     {"100"},
	}
	processQuery := url.Values{
		"namespace": {namespace},
		"pod":       {pod},
		"level":     {"process"},
		"limit":     {strconv.Itoa(limit)},
	}
	eventQuery := url.Values{
		"namespace": {namespace},
		"pod":       {pod},
		"limit":     {strconv.Itoa(limit)},
	}

	var (
		hostResponse    host.Response
		processResponse cpu.Response
		eventResponse   eventstore.Response
		healthResponse  agent.HealthResponse
		guestResponse   guest.Response
		hostErr         error
		processErr      error
		eventErr        error
		healthErr       error
		guestErr        error
	)
	var group sync.WaitGroup
	group.Add(4)
	go func() {
		defer group.Done()
		hostErr = inspectGet(ctx, endpoint, "/api/v1/host/resources?"+hostQuery.Encode(), inspectResponseLimit, &hostResponse)
	}()
	go func() {
		defer group.Done()
		processErr = inspectGet(ctx, endpoint, "/api/v1/cpu?"+processQuery.Encode(), inspectResponseLimit, &processResponse)
	}()
	go func() {
		defer group.Done()
		eventErr = inspectGet(ctx, endpoint, "/api/v1/events?"+eventQuery.Encode(), inspectResponseLimit, &eventResponse)
	}()
	go func() {
		defer group.Done()
		healthErr = inspectGet(ctx, endpoint, "/healthz", inspectResponseLimit, &healthResponse)
	}()
	if report.Pod.Runtime == "kata" && len(snapshot.Sandboxes) > 0 {
		group.Add(1)
		guestQuery := url.Values{
			"sandbox":  {snapshot.Sandboxes[0].ID},
			"duration": {duration.String()},
			"limit":    {strconv.Itoa(limit)},
		}
		go func() {
			defer group.Done()
			guestErr = inspectGet(ctx, endpoint, guest.Path+"?"+guestQuery.Encode(), guest.MaxResponseBytes, &guestResponse)
		}()
	}
	group.Wait()

	if hostErr != nil {
		report.warn("host_resources_unavailable", hostErr.Error())
	} else {
		report.Host = &hostResponse
		if len(hostResponse.Items) == 0 {
			report.warn("host_resources_empty", "no current runc container or Kata sandbox matched the Pod")
		}
		for _, item := range hostResponse.Items {
			target := item.ContainerID
			if target == "" {
				target = item.SandboxID
			}
			missing := make([]string, 0, 3)
			if item.CPU == nil {
				missing = append(missing, "cpu")
			}
			if item.Memory == nil {
				missing = append(missing, "memory")
			}
			if item.IO == nil {
				missing = append(missing, "io")
			}
			if len(missing) > 0 {
				message := fmt.Sprintf("%s %s has no %s result", item.Kind, shortID(target), strings.Join(missing, ", "))
				if item.Kind == "runc-container" && len(missing) == 1 && missing[0] == "io" {
					message += " (runc Host IO is not implemented)"
				}
				report.warn("host_resource_fields_missing", message)
			}
			if item.Partial {
				report.warn("host_resources_partial", fmt.Sprintf("%s %s has incomplete Host data", item.Kind, shortID(target)))
			}
		}
	}
	if processErr != nil {
		report.warn("host_processes_unavailable", processErr.Error())
	} else {
		report.Processes = &processResponse
		if processResponse.Error != "" {
			report.warn("host_process_sample_error", processResponse.Error)
		}
		if processResponse.UnresolvedTasks > 0 {
			report.warn("unresolved_host_tasks", fmt.Sprintf("%d sampled task(s) were not attributed", processResponse.UnresolvedTasks))
		}
	}
	if eventErr != nil {
		report.warn("events_unavailable", eventErr.Error())
	} else {
		report.Events = &eventResponse
	}
	if healthErr != nil {
		report.warn("health_unavailable", healthErr.Error())
	} else {
		report.Health = &healthResponse
		if healthResponse.EventsLost > 0 {
			report.warn("lifecycle_events_lost", fmt.Sprintf("Agent reports %d ring-buffer reservation failure(s) since start", healthResponse.EventsLost))
		}
	}
	if report.Pod.Runtime == "kata" {
		if len(snapshot.Sandboxes) == 0 {
			report.warn("guest_processes_unavailable", "Guest processes are not visible from the Host and the current Kata Sandbox identity is unavailable")
		} else if guestErr != nil {
			report.warn("guest_processes_unavailable", "Guest processes are not visible from the Host and the opt-in Guest relay did not return data: "+guestErr.Error())
		} else {
			report.Guest = &guestResponse
			for _, process := range guestResponse.Processes {
				if process.Partial {
					report.warn("guest_process_partial", fmt.Sprintf("Guest PID %d has incomplete CPU/runqlat data", process.NamespacePID))
				}
			}
		}
	}
	report.sortWarnings()
	return report, nil
}

func inspectIsKata(snapshot *topology.Snapshot) bool {
	if len(snapshot.Sandboxes) > 0 {
		return true
	}
	for _, container := range snapshot.Containers {
		if container.ProcessVisibility != "" && container.ProcessVisibility != "host" {
			return true
		}
		if strings.HasPrefix(container.RuntimeType, "io.containerd.kata.") || strings.HasPrefix(container.RuntimeType, "io.containerd.kata-") || container.RuntimeHandler == "kata" || strings.HasPrefix(container.RuntimeHandler, "kata-") {
			return true
		}
	}
	return false
}

func inspectGet(ctx context.Context, endpoint, path string, limit int64, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+path, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return fmt.Errorf("response exceeds %s", humanBytes(uint64(limit)))
	}
	if response.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(body))
		if len(message) > 512 {
			message = message[:512] + "..."
		}
		return fmt.Errorf("agent returned %s: %s", response.Status, message)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode agent response: %w", err)
	}
	return nil
}

func (r *inspectReport) warn(code, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	for _, warning := range r.Warnings {
		if warning.Code == code && warning.Message == message {
			return
		}
	}
	r.Warnings = append(r.Warnings, inspectWarning{Code: code, Message: message})
}

func (r *inspectReport) sortWarnings() {
	sort.SliceStable(r.Warnings, func(i, j int) bool {
		if r.Warnings[i].Code == r.Warnings[j].Code {
			return r.Warnings[i].Message < r.Warnings[j].Message
		}
		return r.Warnings[i].Code < r.Warnings[j].Code
	})
}

func writeInspectText(writer io.Writer, report inspectReport) error {
	tw := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)

	fmt.Fprintln(tw, "POD")
	fmt.Fprintln(tw, "NAMESPACE\tNAME\tRUNTIME\tNODE\tPHASE\tQOS\tUID")
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", report.Pod.Namespace, report.Pod.Name, report.Pod.Runtime, dash(report.Pod.Node), dash(report.Pod.Phase), dash(report.Pod.QoSClass), dash(shortID(report.Pod.UID)))

	fmt.Fprintln(tw, "\nTOPOLOGY")
	if len(report.Topology.Sandboxes) > 0 {
		fmt.Fprintln(tw, "TYPE\tNAME/ID\tSTATE/HANDLER\tPID\tCGROUP/COMMAND")
		for _, sandbox := range report.Topology.Sandboxes {
			fmt.Fprintf(tw, "sandbox\t%s\t%s\t-\t-\n", shortID(sandbox.ID), dash(sandbox.RuntimeHandler))
			if sandbox.Shim != nil {
				fmt.Fprintf(tw, "shim\t%s\t-\t%d\t%s\n", shortID(sandbox.ID), sandbox.Shim.PID, dash(sandbox.Shim.Command))
			}
			if sandbox.Hypervisor != nil {
				fmt.Fprintf(tw, "vmm\t%s\t-\t%d\t%s\n", shortID(sandbox.ID), sandbox.Hypervisor.PID, dash(sandbox.Hypervisor.Command))
			}
		}
	} else {
		fmt.Fprintln(tw, "TYPE\tNAME/ID\tSTATE/HANDLER\tPID\tCGROUP/COMMAND")
	}
	for _, container := range report.Topology.Containers {
		runtime := container.RuntimeHandler
		if runtime == "" {
			runtime = container.RuntimeType
		}
		fmt.Fprintf(tw, "container\t%s/%s\t%s/%s\t%d\t%s\n", container.Name, shortID(container.ID), dash(container.State), dash(runtime), container.PID, dash(container.CgroupPath))
	}

	fmt.Fprintln(tw, "\nHOST RESOURCES")
	fmt.Fprintln(tw, "KIND\tTARGET\tCPU\tMEMORY\tREAD/WRITE\tSTATUS")
	if report.Host == nil || len(report.Host.Items) == 0 {
		fmt.Fprintln(tw, "-\t-\t-\t-\t-\tunavailable")
	} else {
		for _, item := range report.Host.Items {
			target := item.ContainerID
			if target == "" {
				target = item.SandboxID
			}
			cpuValue := "-"
			if item.CPU != nil {
				cpuValue = fmt.Sprintf("%.2f%%", item.CPU.Percent)
			}
			memory := "-"
			if item.Memory != nil {
				memory = humanBytes(item.Memory.Bytes)
			}
			ioValue := "-"
			if item.IO != nil {
				ioValue = humanBytes(item.IO.ReadBytes) + "/" + humanBytes(item.IO.WriteBytes)
			} else if item.Kind == "runc-container" {
				ioValue = "unsupported"
			}
			status := "complete"
			if item.Partial {
				status = "partial"
			} else if item.CPU == nil || item.Memory == nil || item.IO == nil {
				status = "limited"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", item.Kind, shortID(target), cpuValue, memory, ioValue, status)
		}
	}

	if report.Pod.Runtime == "kata" {
		writeGuestProcesses(tw, report)
		writeVMMThreads(tw, report)
	} else {
		writeHostProcesses(tw, report)
	}

	fmt.Fprintln(tw, "\nRECENT EVENTS")
	fmt.Fprintln(tw, "SEQ\tTYPE\tPID\tCONTAINER\tCOMMAND\tRESULT\tSOURCE")
	if report.Events == nil || len(report.Events.Events) == 0 {
		fmt.Fprintln(tw, "-\t-\t-\t-\t-\t-\t-")
	} else {
		for _, record := range report.Events.Events {
			container := "-"
			if record.Workload != nil {
				container = record.Workload.ContainerName
			}
			result := "-"
			if record.Type == "exit" {
				result = "exit=" + strconv.FormatInt(int64(record.ExitCode), 10)
				if record.ExitSignal != 0 {
					result = "signal=" + strconv.FormatInt(int64(record.ExitSignal), 10)
				}
			}
			fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%s\t%s\t%s\n", record.Sequence, record.Type, record.PID, container, dash(record.Command), result, dash(record.MetadataSource))
		}
	}

	fmt.Fprintln(tw, "\nWARNINGS")
	if len(report.Warnings) == 0 {
		fmt.Fprintln(tw, "none")
	} else {
		for _, warning := range report.Warnings {
			fmt.Fprintf(tw, "[%s]\t%s\n", warning.Code, warning.Message)
		}
	}
	return tw.Flush()
}

func writeHostProcesses(tw *tabwriter.Writer, report inspectReport) {
	fmt.Fprintln(tw, "\nHOT PROCESSES (HOST)")
	fmt.Fprintln(tw, "PID\tCONTAINER\tCOMMAND\tCPU")
	if report.Processes == nil || len(report.Processes.Items) == 0 {
		fmt.Fprintln(tw, "-\t-\t-\t-")
		return
	}
	for _, process := range report.Processes.Items {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%.2f%%\n", process.PID, dash(process.ContainerName), dash(process.Command), process.CPUPercent)
	}
}

func writeGuestProcesses(tw *tabwriter.Writer, report inspectReport) {
	fmt.Fprintln(tw, "\nHOT PROCESSES (GUEST)")
	fmt.Fprintln(tw, "PID\tCOMMAND\tCPU\tRUNQLAT P95\tCOVERAGE\tSTATUS")
	if report.Guest == nil || len(report.Guest.Processes) == 0 {
		fmt.Fprintln(tw, "-\t-\t-\t-\t-\tunavailable")
		return
	}
	for _, process := range report.Guest.Processes {
		cpuValue := "-"
		if process.CPU != nil {
			cpuValue = fmt.Sprintf("%.2f%%", process.CPU.Percent)
		}
		latency := "-"
		if process.RunQLat != nil {
			latency = fmt.Sprintf("%dus", process.RunQLat.P95US)
		}
		status := "complete"
		if process.Partial {
			status = "partial"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d/%d\t%s\n", process.NamespacePID, dash(process.Command), cpuValue, latency, process.RunQLatCoverage.ObservedThreads, process.RunQLatCoverage.TotalThreads, status)
	}
}

func writeVMMThreads(tw *tabwriter.Writer, report inspectReport) {
	fmt.Fprintln(tw, "\nVMM THREADS (HOST)")
	fmt.Fprintln(tw, "TID\tROLE\tCOMMAND\tCPU\tRUNQLAT P95\tSTATUS")
	written := false
	if report.Host != nil {
		for _, item := range report.Host.Items {
			for _, thread := range item.Threads {
				written = true
				cpuValue := "-"
				if thread.CPU != nil {
					cpuValue = fmt.Sprintf("%.2f%%", thread.CPU.Percent)
				}
				latency := "-"
				if thread.RunQLat != nil {
					latency = fmt.Sprintf("%dus", thread.RunQLat.P95US)
				}
				status := "complete"
				if len(thread.Issues) > 0 {
					status = "partial"
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", thread.TID, dash(thread.Role), dash(thread.Command), cpuValue, latency, status)
			}
		}
	}
	if !written {
		fmt.Fprintln(tw, "-\t-\t-\t-\t-\tunavailable")
	}
}

func shortID(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func humanBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%dB", value)
	}
	divisor, exponent := uint64(unit), 0
	for next := value / unit; next >= unit && exponent < 5; next /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f%ciB", float64(value)/float64(divisor), "KMGTPE"[exponent])
}
