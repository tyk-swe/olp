//go:build bench

package loadgen

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestConfigValidation(t *testing.T) {
	ok := Config{URL: "http://x:1", Model: "m", Rate: 10, Duration: time.Second}
	if _, err := ok.normalize(); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"no url":             {func(c *Config) { c.URL = "" }, "base URL"},
		"relative url":       {func(c *Config) { c.URL = "gateway:8080" }, "base URL"},
		"ftp url":            {func(c *Config) { c.URL = "ftp://x" }, "base URL"},
		"url with a query":   {func(c *Config) { c.URL = "http://x/?a=1" }, "query"},
		"unknown dialect":    {func(c *Config) { c.Dialect = "cohere" }, "dialect"},
		"no model":           {func(c *Config) { c.Model = "" }, "model"},
		"zero rate":          {func(c *Config) { c.Rate = 0 }, "rate"},
		"negative rate":      {func(c *Config) { c.Rate = -5 }, "rate"},
		"absurd rate":        {func(c *Config) { c.Rate = 1e9 }, "rate"},
		"no duration":        {func(c *Config) { c.Duration = 0 }, "duration"},
		"negative warmup":    {func(c *Config) { c.Warmup = -time.Second }, "warmup"},
		"no request at all":  {func(c *Config) { c.Rate, c.Duration = 0.1, time.Second }, "no request"},
		"share above one":    {func(c *Config) { c.StreamShare = 1.5 }, "stream share"},
		"negative share":     {func(c *Config) { c.StreamShare = -0.1 }, "stream share"},
		"negative prompt":    {func(c *Config) { c.PromptTokens = []int{-1} }, "prompt size"},
		"huge prompt":        {func(c *Config) { c.PromptTokens = []int{1 << 30} }, "prompt size"},
		"negative max":       {func(c *Config) { c.MaxTokens = -1 }, "max tokens"},
		"negative in flight": {func(c *Config) { c.MaxInFlight = -1 }, "max in flight"},
		"negative timeout":   {func(c *Config) { c.Timeout = -1 }, "timeout"},
		"negative slow read": {func(c *Config) { c.SlowRead.BytesPerSecond = -1 }, "slow read"},
		"bad header name":    {func(c *Config) { c.Headers = map[string]string{"bad name": "x"} }, "header"},
	} {
		c := ok
		tc.mutate(&c)
		if _, err := c.normalize(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error mentioning %q", name, err, tc.want)
		}
		if _, err := Run(context.Background(), c); err == nil {
			t.Errorf("%s: Run accepted it", name)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	c, err := Config{URL: "http://x", Model: "m", Rate: 10, Duration: time.Second}.normalize()
	if err != nil {
		t.Fatal(err)
	}
	if c.Dialect != OpenAI || c.MaxTokens != DefaultMaxTokens || c.MaxInFlight != DefaultMaxInFlight || c.MaxBacklog != DefaultMaxInFlight ||
		c.Timeout != DefaultTimeout || c.Drain != DefaultTimeout+time.Second || c.LateAfter != DefaultLateAfter || len(c.PromptTokens) != 1 || c.PromptTokens[0] != 0 {
		t.Fatalf("%+v", c)
	}
	// A negative backlog means none, and a slow reader gets a small buffer.
	c, _ = Config{URL: "http://x", Model: "m", Rate: 10, Duration: time.Second, MaxBacklog: -1, SlowRead: SlowRead{BytesPerSecond: 100}}.normalize()
	if c.MaxBacklog != 0 || c.SlowRead.RcvBuf != DefaultSlowReadRcvBuf {
		t.Fatalf("%+v", c)
	}
	// The schedule.
	if c.warmupRequests() != 0 || c.measuredRequests() != 10 || c.offset(5) != 500*time.Millisecond {
		t.Fatalf("%d %d %v", c.warmupRequests(), c.measuredRequests(), c.offset(5))
	}
	c.Rate, c.Warmup, c.Duration = 3000, 10*time.Second, 60*time.Second
	if c.warmupRequests() != 30_000 || c.measuredRequests() != 180_000 {
		t.Fatalf("%d %d", c.warmupRequests(), c.measuredRequests())
	}
}
