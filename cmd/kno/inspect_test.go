package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cowhorse05/Kube-Node-Observer/internal/topology"
)

func TestInspectPodTextCombinesRuncData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("namespace") != "demo" && r.URL.Path != "/healthz" {
			t.Errorf("namespace query = %q for %s", r.URL.Query().Get("namespace"), r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/v1/topology":
			fmt.Fprint(w, `{"snapshot":{"generatedAt":"2026-11-13T01:00:00Z","nodeName":"node-a","containers":[{"id":"aaaaaaaaaaaaaaaa","sandboxId":"bbbbbbbbbbbbbbbb","name":"app","image":"demo:v1","state":"CONTAINER_RUNNING","runtimeType":"io.containerd.runc.v2","processVisibility":"host","namespace":"demo","podName":"web","podUid":"pod-uid","podPhase":"Running","qosClass":"Burstable","pid":42,"cgroupPath":"/demo","cgroupId":7,"processes":[]}],"sandboxes":[]}}`)
		case "/api/v1/host/resources":
			if r.URL.Query().Get("duration") != "100ms" {
				t.Errorf("duration = %q", r.URL.Query().Get("duration"))
			}
			fmt.Fprint(w, `{"domain":"host","roleSource":"qemu-thread-name","startedAt":"2026-11-13T01:00:00Z","durationSeconds":0.1,"totalItems":1,"items":[{"kind":"runc-container","namespace":"demo","podName":"web","podUid":"pod-uid","sandboxId":"bbbb","containerId":"aaaaaaaaaaaaaaaa","cpu":{"accounting":"cgroup","runtimeNs":25000000,"percent":25,"windowSeconds":0.1},"memory":{"bytes":1048576,"source":"cgroup-memory.current"},"io":null,"threads":[],"vcpuRunqlat":null,"partial":false}]}`)
		case "/api/v1/cpu":
			fmt.Fprint(w, `{"accounting":"process","sampledAt":"2026-11-13T01:00:00Z","windowSeconds":1,"level":"process","items":[{"scope":"process","namespace":"demo","podName":"web","podUid":"pod-uid","containerName":"app","containerId":"aaaaaaaaaaaaaaaa","cgroupId":7,"pid":43,"tgid":43,"command":"worker","runtimeNs":500000000,"cpuPercent":50}],"unresolvedTasks":0}`)
		case "/api/v1/events":
			fmt.Fprint(w, `{"events":[{"sequence":9,"type":"exec","timestampNs":1,"cgroupId":7,"pid":43,"tgid":43,"ppid":1,"comm":"worker","metadataSource":"current","workload":{"namespace":"demo","podName":"web","podUid":"pod-uid","containerName":"app","containerId":"aaaaaaaaaaaaaaaa","image":"demo:v1"}}]}`)
		case "/healthz":
			fmt.Fprint(w, `{"status":"ok","ebpfLoaded":true,"eventsSeen":9,"eventsLost":0,"uptime":"1m","component":"kno-agent"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	endpoint := server.URL
	command := newInspectCommand(&endpoint)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"pod", "web", "-n", "demo", "--duration", "100ms"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"RUNTIME", "runc", "node-a", "runc-container", "25.00%", "1.0MiB", "unsupported", "limited", "HOT PROCESSES (HOST)", "worker", "RECENT EVENTS", "WARNINGS", "host_resource_fields_missing"} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("output missing %q:\n%s", expected, output.String())
		}
	}
}

func TestInspectPodJSONReportsKataGuestBoundary(t *testing.T) {
	sandboxID := strings.Repeat("c", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/topology":
			fmt.Fprintf(w, `{"snapshot":{"generatedAt":"2026-11-13T01:00:00Z","nodeName":"node-a","containers":[{"id":"aaaaaaaaaaaaaaaa","sandboxId":%q,"name":"app","image":"demo:v1","state":"CONTAINER_RUNNING","runtimeHandler":"kata-qemu","processVisibility":"guest-unavailable","namespace":"demo","podName":"vm","podUid":"pod-uid","podPhase":"Running","pid":0,"processes":[]}],"sandboxes":[{"id":%q,"namespace":"demo","podName":"vm","podUid":"pod-uid","runtimeHandler":"kata-qemu","hypervisor":{"pid":101,"startTimeTicks":10,"command":"qemu-system-x86_64","cgroupPath":"/vm","cgroupId":8}}]}}`, sandboxID, sandboxID)
		case "/api/v1/host/resources":
			fmt.Fprintf(w, `{"domain":"host","roleSource":"qemu-thread-name","startedAt":"2026-11-13T01:00:00Z","durationSeconds":0.1,"totalItems":1,"items":[{"kind":"kata-sandbox","namespace":"demo","podName":"vm","podUid":"pod-uid","sandboxId":%q,"pid":101,"cpu":null,"memory":null,"io":null,"threads":[],"vcpuRunqlat":null,"partial":true,"issues":["CPU unavailable"]}]}`, sandboxID)
		case "/api/v1/cpu":
			fmt.Fprint(w, `{"accounting":"process","sampledAt":"2026-11-13T01:00:00Z","windowSeconds":1,"level":"process","items":[],"unresolvedTasks":0}`)
		case "/api/v1/events":
			fmt.Fprint(w, `{"events":[]}`)
		case "/healthz":
			fmt.Fprint(w, `{"status":"ok","ebpfLoaded":true,"eventsSeen":9,"eventsLost":2,"uptime":"1m","component":"kno-agent"}`)
		case "/api/v1/guest/resources":
			if r.URL.Query().Get("sandbox") != sandboxID {
				t.Errorf("sandbox = %q", r.URL.Query().Get("sandbox"))
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"code":"guest_unavailable","message":"Guest relay disabled"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	endpoint := server.URL
	command := newInspectCommand(&endpoint)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"pod", "vm", "-n", "demo", "--duration", "100ms", "-o", "json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var report inspectReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != inspectSchemaVersion || report.Pod.Runtime != "kata" || report.Guest != nil {
		t.Fatalf("unexpected report: %#v", report)
	}
	warnings := make(map[string]bool)
	for _, warning := range report.Warnings {
		warnings[warning.Code] = true
	}
	for _, code := range []string{"guest_processes_unavailable", "host_resources_partial", "lifecycle_events_lost"} {
		if !warnings[code] {
			t.Errorf("warning %q missing: %#v", code, report.Warnings)
		}
	}
}

func TestInspectPodRejectsMissingPodAndInvalidOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"snapshot":{"generatedAt":"2026-11-13T01:00:00Z","nodeName":"node-a","containers":[],"sandboxes":[]}}`)
	}))
	defer server.Close()
	endpoint := server.URL

	command := newInspectCommand(&endpoint)
	command.SetArgs([]string{"pod", "missing"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("missing Pod error = %v", err)
	}
	command = newInspectCommand(&endpoint)
	command.SetArgs([]string{"pod", "demo", "-o", "yaml"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "text or json") {
		t.Fatalf("invalid output error = %v", err)
	}
}

func TestInspectIsKataWithoutResolvedSandbox(t *testing.T) {
	for _, container := range []topology.Container{
		{ProcessVisibility: "guest-unavailable"},
		{RuntimeType: "io.containerd.kata.v2"},
		{RuntimeHandler: "kata-qemu"},
	} {
		if !inspectIsKata(&topology.Snapshot{Containers: []topology.Container{container}}) {
			t.Fatalf("Kata container was classified as runc: %#v", container)
		}
	}
	if inspectIsKata(&topology.Snapshot{Containers: []topology.Container{{ProcessVisibility: "host", RuntimeType: "io.containerd.runc.v2", RuntimeHandler: "runc"}}}) {
		t.Fatal("runc container was classified as Kata")
	}
}
