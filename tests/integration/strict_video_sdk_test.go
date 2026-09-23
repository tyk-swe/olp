//go:build integration

package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestStrictVideoPinnedOpenAISDKs(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tests/sdk-smoke/node_modules/openai")); err != nil {
		t.Fatal("pinned OpenAI JavaScript SDK is missing; run pnpm install --frozen-lockfile")
	}
	for _, test := range []struct {
		name, binary string
		args         []string
	}{
		{"javascript", "node", []string{"tests/sdk-smoke/strict-video.mjs"}},
		{"python", "uv", []string{"run", "--project", "tests/sdk-smoke-python", "--frozen", "python", "tests/sdk-smoke-python/strict_video.py"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newAccessHarness(t)
			upstream := newStrictVideoUpstream(t)
			slug, key := publishStrictVideo(t, h, h.owner(), upstream)
			cmd := exec.CommandContext(t.Context(), test.binary, test.args...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "OLP_VIDEO_BASE="+h.HTTP.URL,
				"OLP_VIDEO_ROUTE="+slug, "OLP_VIDEO_KEY="+key)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("pinned %s video SDK: %v\n%s", test.name, err, output)
			}
			creates, gets, contents, deletes, names, values := upstream.snapshot()
			if creates != 1 || gets != 1 || contents != 2 || deletes != 1 || len(names) != 5 || len(values) != 5 {
				t.Fatalf("pinned %s video side effects changed: creates=%d gets=%d contents=%d deletes=%d names=%v\n%s", test.name, creates, gets, contents, deletes, names, output)
			}
			fields := make(map[string]string, len(names))
			for i, name := range names {
				if _, duplicate := fields[name]; duplicate {
					t.Fatalf("pinned %s SDK duplicated multipart field %q", test.name, name)
				}
				fields[name] = string(values[i])
			}
			if fields["prompt"] != "one frame, eight seconds" || fields["model"] != vendorModel || fields["seconds"] != "8" || fields["size"] != "1280x720" || fields["input_reference"] != string([]byte{0x89, 0x50, 0x4e, 0x47, 0, 1, 0xff}) {
				t.Fatalf("pinned %s SDK native multipart source changed: %v", test.name, fields)
			}
		})
	}
}
