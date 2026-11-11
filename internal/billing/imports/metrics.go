package imports

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tyk-swe/olp/internal/metric"
)

var imports = metric.Counter("olp_billing_imports_total", "Billing imports this process ran, by provider and outcome.", "provider", "outcome")

type outcome struct {
	provider string
	result   string
}

type tally struct {
	mu     sync.Mutex
	counts map[outcome]int64
}

func newTally() *tally {
	return &tally{counts: map[outcome]int64{}}
}

func (t *tally) add(provider, result string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[outcome{provider: provider, result: result}]++
}

func (t *tally) samples() []prometheus.Metric {
	t.mu.Lock()
	defer t.mu.Unlock()
	samples := make([]prometheus.Metric, 0, len(t.counts))
	for key, count := range t.counts {
		samples = append(samples, imports.Sample(float64(count), key.provider, key.result))
	}
	return samples
}

func (w *Worker) Metrics() []prometheus.Metric {
	if w == nil {
		return nil
	}
	return w.outcomes.samples()
}
