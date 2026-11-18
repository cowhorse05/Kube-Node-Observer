package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cowhorse05/KubeNode-Observer/internal/agent"
)

func TestDoctorHealthyNode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			fmt.Fprint(w, `{"status":"ok","ebpfLoaded":true,"eventsSeen":12,"eventsLost":0,"uptime":"1m","component":"kno-agent"}`)
		case "/api/v1/topology":
			fmt.Fprintf(w, `{"snapshot":{"generatedAt":%q,"nodeName":"node-a","nodeUid":"node-uid","containers":[{"id":"container-id","sandboxId":"sandbox-id","name":"app","state":"CONTAINER_RUNNING","runtimeType":"io.containerd.runc.v2","runtimeHandler":"runc","processVisibility":"host","namespace":"default","podName":"app","podUid":"pod-uid","pid":100,"cgroupPath":"/kubepods/app","cgroupId":42,"processes":[]}],"sandboxes":[]}}`, time.Now().UTC().Format(time.RFC3339Nano))
		case "/metrics":
			fmt.Fprint(w, healthyDoctorMetrics())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	endpoint := server.URL
	command := newDoctorCommand(&endpoint)
	var output bytes.Buffer
	var stderr bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&stderr)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"KNO DOCTOR", "PASS", "agent", "ebpf", "cgroup-v2", "cri-topology", "Kubernetes informer", "kata", "SKIP", "REMEDIATION"} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("output missing %q:\n%s", expected, output.String())
		}
	}
}

func TestDoctorJSONFailsClosedWithActionableChecks(t *testing.T) {
	sandboxID := strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			fmt.Fprint(w, `{"status":"ok","ebpfLoaded":true,"eventsSeen":12,"eventsLost":2,"uptime":"1m","component":"kno-agent"}`)
		case "/api/v1/topology":
			fmt.Fprintf(w, `{"snapshot":{"generatedAt":%q,"nodeName":"node-a","containers":[{"id":"container-id","sandboxId":"sandbox-id","name":"app","state":"CONTAINER_RUNNING","runtimeType":"io.containerd.runc.v2","runtimeHandler":"runc","processVisibility":"host","namespace":"default","podName":"app","podUid":"pod-uid","pid":100,"processes":[],"issues":["read cgroup path: permission denied"]}],"sandboxes":[{"id":%q,"namespace":"default","podName":"vm","podUid":"vm-uid","runtimeHandler":"kata-qemu","issues":["expected one VMM descendant of verified shim, found 0"]}]}}`, time.Now().UTC().Format(time.RFC3339Nano), sandboxID)
		case "/metrics":
			fmt.Fprint(w, `kno_agent_bpf_map_stats_ready{source="exec"} 0
kno_agent_bpf_map_stats_refresh_error{source="exec"} 1
kno_agent_bpf_map_stats_ready{source="cpu"} 1
kno_agent_bpf_map_stats_refresh_error{source="cpu"} 0
kno_agent_bpf_map_stats_ready{source="runqlat"} 1
kno_agent_bpf_map_stats_refresh_error{source="runqlat"} 0
kno_agent_metrics_collection_errors 2
`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	endpoint := server.URL
	command := newDoctorCommand(&endpoint)
	var output bytes.Buffer
	var stderr bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&stderr)
	command.SetArgs([]string{"-o", "json"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "failing checks") {
		t.Fatalf("error = %v", err)
	}
	var report doctorReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != doctorSchemaVersion || report.Overall != doctorFail {
		t.Fatalf("unexpected report: %#v", report)
	}
	statuses := map[string]doctorStatus{}
	for _, check := range report.Checks {
		statuses[check.Name] = check.Status
		if check.Status != doctorPass && len(check.Remediation) == 0 {
			t.Errorf("check %s has no remediation", check.Name)
		}
	}
	for name, want := range map[string]doctorStatus{
		"ebpf":             doctorWarn,
		"lifecycle-events": doctorWarn,
		"cgroup-v2":        doctorFail,
		"kubernetes":       doctorWarn,
		"kata":             doctorFail,
		"permissions":      doctorFail,
	} {
		if statuses[name] != want {
			t.Errorf("%s status = %s, want %s", name, statuses[name], want)
		}
	}
}

func TestDoctorAgentFailureStillPrintsReport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	endpoint := server.URL
	command := newDoctorCommand(&endpoint)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "failing checks") || !strings.Contains(output.String(), "KNO DOCTOR") || !strings.Contains(output.String(), "FAIL") {
		t.Fatalf("error=%v output=%s", err, output.String())
	}
}

func TestDoctorRejectsInvalidFlags(t *testing.T) {
	endpoint := "http://127.0.0.1:1"
	for _, args := range [][]string{{"-o", "yaml"}, {"--timeout", "500ms"}, {"extra"}} {
		command := newDoctorCommand(&endpoint)
		command.SetArgs(args)
		if err := command.Execute(); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

func TestParseDoctorMetrics(t *testing.T) {
	metrics := parseDoctorMetrics(healthyDoctorMetrics())
	if metrics.mapReady["exec"] != 1 || metrics.mapRefreshError["runqlat"] != 0 || !metrics.hasCollection || metrics.collectionError != 0 {
		t.Fatalf("unexpected metrics: %#v", metrics)
	}
	check := doctorEBPFCheck(agentHealthLoaded(), nil, parseDoctorMetrics(`kno_agent_bpf_map_stats_ready{source="exec"} 1`), nil)
	if check.Status != doctorWarn {
		t.Fatalf("incomplete metrics status = %s, want warn", check.Status)
	}
}

func agentHealthLoaded() agent.HealthResponse {
	return agent.HealthResponse{Status: "ok", Component: "kno-agent", EBPFLoaded: true}
}

func healthyDoctorMetrics() string {
	return `# HELP ignored ignored
kno_agent_bpf_map_stats_ready{source="exec"} 1
kno_agent_bpf_map_stats_refresh_error{source="exec"} 0
kno_agent_bpf_map_stats_ready{source="cpu"} 1
kno_agent_bpf_map_stats_refresh_error{source="cpu"} 0
kno_agent_bpf_map_stats_ready{source="runqlat"} 1
kno_agent_bpf_map_stats_refresh_error{source="runqlat"} 0
kno_agent_metrics_collection_errors 0
`
}
