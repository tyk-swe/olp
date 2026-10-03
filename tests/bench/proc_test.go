//go:build bench

package bench_test

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// What a scenario learns about a process it started comes from /proc, so the
// gateway under test is measured as the release binary it is, with nothing
// linked into it.

// procStatus is the part of /proc/<pid>/status a scenario reads.
type procStatus struct {
	// RSSKB and PeakRSSKB are VmRSS and VmHWM in kibibytes.
	RSSKB, PeakRSSKB int64
	Threads          int64
	// CPUsAllowed is Cpus_allowed_list: the CPUs the process may run on, which
	// is the pin when taskset applied one.
	CPUsAllowed string
}

// parseProcStatus reads the text of /proc/<pid>/status.
func parseProcStatus(text string) (procStatus, error) {
	var st procStatus
	var found int
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch name {
		case "VmRSS":
			st.RSSKB, found = kibibytes(value), found+1
		case "VmHWM":
			st.PeakRSSKB, found = kibibytes(value), found+1
		case "Threads":
			st.Threads, _ = strconv.ParseInt(value, 10, 64)
			found++
		case "Cpus_allowed_list":
			st.CPUsAllowed = value
			found++
		}
	}
	if found < 4 {
		return st, fmt.Errorf("the status text lacks VmRSS, VmHWM, Threads or Cpus_allowed_list")
	}
	return st, nil
}

// kibibytes reads "123456 kB".
func kibibytes(value string) int64 {
	n, _ := strconv.ParseInt(strings.Fields(value)[0], 10, 64)
	return n
}

func readProcStatus(pid int) (procStatus, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return procStatus{}, err
	}
	return parseProcStatus(string(data))
}

// clockTicks is USER_HZ, the unit /proc reports CPU time in. It is fixed at
// 100 on Linux's user-space ABI on every architecture.
const clockTicks = 100

// parseCPUSeconds reads utime and stime from the text of /proc/<pid>/stat.
// The command name is in parentheses and may hold spaces, so fields are
// counted from the last parenthesis.
func parseCPUSeconds(text string) (float64, error) {
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return 0, fmt.Errorf("no command name in the stat text")
	}
	fields := strings.Fields(text[end+1:])
	// After the name come state (field 3), then fields 4 onwards, so utime and
	// stime, fields 14 and 15, sit at offsets 11 and 12.
	if len(fields) < 13 {
		return 0, fmt.Errorf("the stat text has %d fields after the name", len(fields))
	}
	utime, err1 := strconv.ParseInt(fields[11], 10, 64)
	stime, err2 := strconv.ParseInt(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("utime and stime are not numbers")
	}
	return float64(utime+stime) / clockTicks, nil
}

func readCPUSeconds(pid int) (float64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	return parseCPUSeconds(string(data))
}

// selfCPUSeconds is the test process's own CPU time, which is the load
// generator's, since the generator runs in it.
func selfCPUSeconds() float64 {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return 0
	}
	return float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
}

// resetPeakRSS makes VmHWM start over from the current RSS, so the peak read
// afterwards belongs to the measured run rather than to start-up. It reports
// whether the kernel allowed it.
func resetPeakRSS(pid int) bool {
	return os.WriteFile(fmt.Sprintf("/proc/%d/clear_refs", pid), []byte("5"), 0) == nil
}

// processGone reports whether pid no longer names a live process.
func processGone(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	end := strings.LastIndexByte(string(data), ')')
	return end >= 0 && strings.HasPrefix(strings.TrimSpace(string(data[end+1:])), "Z")
}

// loadAverage is the first field of /proc/loadavg, recorded with a result so a
// run on a busy machine is recognisable as one.
func loadAverage() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.Fields(string(data) + " ")[0], 64)
	return v
}

// launcher writes the script that starts a binary in the environment the
// benchmark wants and returns its path. The script raises the descriptor limit
// to the hard limit, which Go lowers again for the children it starts, and
// pins the process with taskset when asked. It ends with exec, so the process
// keeps the script's pid: the one the test signals and reads in /proc.
//
// It also starts a watcher that kills the process once the test process that
// started it is gone. A test that times out is killed without running its
// cleanups, and a gateway or a mock left running would go on using the CPUs the
// next run is to measure.
func launcher(t testing.TB, name, binary, cpus string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("ulimit -n \"$(ulimit -Hn)\" 2>/dev/null || ulimit -n 1048576 2>/dev/null || true\n")
	b.WriteString("parent=$PPID self=$$\n")
	b.WriteString("( while kill -0 \"$parent\" 2>/dev/null; do kill -0 \"$self\" 2>/dev/null || exit 0; sleep 1; done; kill -KILL \"$self\" 2>/dev/null ) >/dev/null 2>&1 &\n")
	if cpus != "" {
		fmt.Fprintf(&b, "exec taskset -c %s %s \"$@\"\n", shellQuote(cpus), shellQuote(binary))
	} else {
		fmt.Fprintf(&b, "exec %s \"$@\"\n", shellQuote(binary))
	}
	path := filepath.Join(t.TempDir(), name+".sh")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// pinSelf pins every thread of the test process, which runs the load
// generator, to cpus, and puts the mask back when the test ends. Threads the
// runtime starts later inherit the mask, and so would the processes the next
// scenario starts, which is why it must not outlive the scenario.
func pinSelf(t testing.TB, cpus string) {
	t.Helper()
	pid := strconv.Itoa(os.Getpid())
	before, err := readProcStatus(os.Getpid())
	if err != nil {
		t.Fatalf("read this process's CPU mask: %v", err)
	}
	if out, err := execCombined("taskset", "-a", "-c", "-p", cpus, pid); err != nil {
		t.Fatalf("pin the load generator to CPUs %s: %v\n%s", cpus, err, out)
	}
	t.Cleanup(func() {
		if out, err := execCombined("taskset", "-a", "-c", "-p", before.CPUsAllowed, pid); err != nil {
			t.Errorf("restore the CPU mask %s: %v\n%s", before.CPUsAllowed, err, out)
		}
	})
}

// metrics is one scrape of the private listener, keyed by series as written,
// labels included: `olp_http_admission_rejections_total{surface="inference"}`.
type metrics map[string]float64

// parseMetrics reads Prometheus text exposition, skipping comments and
// anything that is not a sample.
func parseMetrics(text []byte) metrics {
	out := metrics{}
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		if v, err := strconv.ParseFloat(line[i+1:], 64); err == nil {
			out[line[:i]] = v
		}
	}
	return out
}

// Series the scenarios read.
const (
	seriesInferenceRejections = `olp_http_admission_rejections_total{surface="inference"}`
	seriesInferenceAdmitted   = `olp_http_admitted_requests{surface="inference"}`
	seriesInferenceCapacity   = `olp_http_admission_capacity{surface="inference"}`
	seriesOpenCircuits        = "olp_open_target_circuits"
	seriesEventsPending       = "olp_request_metadata_events_pending"
	seriesEventsDropped       = "olp_request_metadata_events_dropped_total"
	seriesEventsAbandoned     = "olp_request_metadata_events_abandoned_total"
	seriesConsumerPending     = "olp_request_metadata_consumer_pending_events"
	seriesConsumerLag         = "olp_request_metadata_consumer_lag_events"
	seriesStreamRetrying      = "olp_request_metadata_stream_retrying"
	seriesLimiterAvailable    = "olp_distributed_limiter_available"
	seriesFailOpen            = "olp_limits_fail_open_total"
	seriesSnapshotAge         = "olp_observability_metrics_snapshot_age_seconds"
	// The Go runtime's cumulative heap allocations, objects and bytes.
	seriesAllocObjects = "go_memstats_mallocs_total"
	seriesAllocBytes   = "go_memstats_alloc_bytes_total"
)
