package observability

import (
	"fmt"
	"io"
	"runtime/metrics"
)

// allocationSeries are the Go runtime's cumulative heap allocation counters,
// served under the names Prometheus client libraries give them so that a
// dashboard written for any Go service reads them. They are what makes the
// allocations of a whole process per request measurable from outside: two
// scrapes and the number of requests between them.
var allocationSeries = []struct{ source, name, help string }{
	{"/gc/heap/allocs:objects", "go_memstats_mallocs_total", "Total number of heap objects allocated, both live and gone."},
	{"/gc/heap/allocs:bytes", "go_memstats_alloc_bytes_total", "Total number of bytes allocated, even if freed."},
}

// writeAllocationMetrics renders the allocation counters as they stand now. The
// runtime counts an allocation when the span it came from leaves the
// allocating processor's cache, so a counter trails the true figure by the
// objects of the spans still cached, which is small against the difference of
// two scrapes taken under load.
func writeAllocationMetrics(w io.Writer) {
	samples := make([]metrics.Sample, len(allocationSeries))
	for i, series := range allocationSeries {
		samples[i].Name = series.source
	}
	metrics.Read(samples)
	for i, series := range allocationSeries {
		// A runtime that does not know a counter leaves its series out.
		if samples[i].Value.Kind() != metrics.KindUint64 {
			continue
		}
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", series.name, series.help, series.name, series.name, samples[i].Value.Uint64())
	}
}
