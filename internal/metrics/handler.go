package metrics

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/cowhorse05/KubeNode-Observer/internal/agent"
	"github.com/cowhorse05/KubeNode-Observer/internal/telemetry"
)

type HealthSource interface {
	Stats() agent.HealthStats
}

type Handler struct {
	Timing  *telemetry.Recorder // Set before serving.
	handler http.Handler
}

func NewHandler(health HealthSource, maps *MapCache, procRoot string, runtimeSources ...RuntimeSources) *Handler {
	sources := RuntimeSources{}
	if len(runtimeSources) > 0 {
		sources = runtimeSources[0]
	}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newCollector(health, maps, procRoot, sources))
	return &Handler{handler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	})}
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer h.Timing.Observe(telemetry.MetricsScrape, time.Now())
	h.handler.ServeHTTP(writer, request)
}

func readUintFile(path string) (uint64, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(content))
	if len(fields) == 0 {
		return 0, fmt.Errorf("%s is empty", path)
	}
	return strconv.ParseUint(fields[0], 10, 64)
}

func readProcessCPU(procRoot string) (uint64, error) {
	paths, err := filepath.Glob(filepath.Join(procRoot, "self", "task", "*", "schedstat"))
	if err != nil {
		return 0, err
	}
	if len(paths) == 0 {
		return 0, fmt.Errorf("no task schedstat files found")
	}
	var total uint64
	for _, path := range paths {
		value, err := readUintFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += value
	}
	return total, nil
}

func readRSS(path string) (uint64, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	scanner.Split(bufio.ScanWords)
	if !scanner.Scan() || !scanner.Scan() {
		return 0, fmt.Errorf("%s does not contain an RSS field", path)
	}
	pages, err := strconv.ParseUint(scanner.Text(), 10, 64)
	if err != nil {
		return 0, err
	}
	return pages * uint64(os.Getpagesize()), nil
}
