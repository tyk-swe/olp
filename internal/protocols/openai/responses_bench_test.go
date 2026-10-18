package openai

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/oif"
)

// BenchmarkResponsesMetadataStream relays 64 native content frames and their
// created/completed lifecycle frames without retaining generated output. It
// uses the public API available at the initial architecture-review baseline,
// so identical instrumentation can compare native validation and accounting.
func BenchmarkResponsesMetadataStream(b *testing.B) {
	const chunks = 64
	var source strings.Builder
	source.WriteString("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp-bench\",\"object\":\"response\",\"created_at\":1800000000,\"model\":\"provider-model\",\"status\":\"in_progress\",\"output\":[],\"usage\":null}}\n\n")
	var answer strings.Builder
	for i := range chunks {
		fmt.Fprintf(&source, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":%d,\"item_id\":\"msg-bench\",\"output_index\":0,\"content_index\":0,\"delta\":\"word %d \"}\n\n", i+1, i)
		fmt.Fprintf(&answer, "word %d ", i)
	}
	fmt.Fprintf(&source, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":%d,\"response\":{\"id\":\"resp-bench\",\"object\":\"response\",\"created_at\":1800000000,\"model\":\"provider-model\",\"status\":\"completed\",\"output\":[{\"id\":\"msg-bench\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"%s\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":17,\"output_tokens\":9,\"total_tokens\":26,\"input_tokens_details\":{\"cached_tokens\":3}}}}\n\n", chunks+1, answer.String())
	encodedWire := []byte(source.String())
	for _, tc := range []struct {
		name    string
		observe bool
	}{{"relay", false}, {"native-observer", true}} {
		b.Run(tc.name, func(b *testing.B) {
			reader := bytes.NewReader(encodedWire)
			var frames, emittedBytes, terminals int
			var observedFrames, nativeModels int
			observe := func(event oif.Event) error {
				observedFrames++
				if value, ok := event.Source().Lookup("/response/model"); ok {
					model, valid := value.Text()
					if !valid || model != "provider-model" {
						return fmt.Errorf("native observer received projected model %q", model)
					}
					nativeModels++
				}
				return nil
			}
			relay := func(emit wire.Emit) (*wire.Completion, error) {
				reader.Reset(encodedWire)
				frames, emittedBytes, observedFrames, nativeModels = 0, 0, 0, 0
				if tc.observe {
					return StreamMetadataEvents(wire.Responses, wire.Responses, reader, 8192, "bench-route", true, emit, observe)
				}
				return StreamMetadata(wire.Responses, reader, 8192, "bench-route", true, emit)
			}
			var projected bytes.Buffer
			completion, err := relay(func(frame []byte) error {
				frames++
				emittedBytes += len(frame)
				if bytes.Contains(frame, []byte(`"type":"response.completed"`)) {
					terminals++
				}
				_, err := projected.Write(frame)
				return err
			})
			if err != nil || completion == nil || completion.Usage == nil {
				b.Fatalf("native stream did not complete with usage: %v, %v", completion, err)
			}
			u := completion.Usage
			if tc.observe && (observedFrames != chunks+2 || nativeModels != 2) {
				b.Fatalf("native observer lost original frames/model: frames=%d models=%d", observedFrames, nativeModels)
			}
			if frames != chunks+2 || terminals != 1 || emittedBytes <= 0 ||
				u.InputTokens != 17 || u.OutputTokens != 9 || u.TotalTokens != 26 ||
				u.CachedInputTokens == nil || *u.CachedInputTokens != 3 || completion.FinishReason != "stop" ||
				!bytes.Contains(projected.Bytes(), []byte(`"status":"in_progress"`)) ||
				!bytes.Contains(projected.Bytes(), []byte(`"model":"bench-route"`)) ||
				!bytes.Contains(projected.Bytes(), []byte(`"delta":"word 63 "`)) ||
				bytes.Contains(projected.Bytes(), []byte(`"model":"provider-model"`)) {
				b.Fatalf("native relay lost frames, binding, terminal evidence or counters: frames=%d terminal=%d bytes=%d completion=%+v", frames, terminals, emittedBytes, completion)
			}

			count := func(frame []byte) error {
				frames++
				emittedBytes += len(frame)
				return nil
			}
			b.SetBytes(int64(len(encodedWire)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := relay(count); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(frames), "frames/op")
			b.ReportMetric(float64(emittedBytes), "wire-B/op")
			b.ReportMetric(float64(observedFrames), "native-frames/op")
		})
	}
}

// Compare the unary retained path's already-owned OIF document with reparsing
// the native bytes. Large output stays in the immutable source; the accounting
// observer owns only status and usage, independent of generated content size.
func BenchmarkResponseUsageObservation(b *testing.B) {
	for _, size := range []int{0, 1 << 20} {
		body := []byte(`{"id":"resp-bench","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"` + strings.Repeat("x", size) + `"}]}],"usage":{"input_tokens":17,"output_tokens":9,"total_tokens":26,"input_tokens_details":{"cached_tokens":3}}}`)
		doc, err := oif.ParseJSON(body, oif.Limits{})
		if err != nil {
			b.Fatal(err)
		}
		for _, ownedDocument := range []bool{false, true} {
			b.Run(fmt.Sprintf("output=%d/document=%t", size, ownedDocument), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					var observed ResponseUsageObservation
					var err error
					if ownedDocument {
						observed, err = ObserveResponseDocumentUsage(doc)
					} else {
						observed, err = ObserveResponseUsage(body)
					}
					if err != nil || !observed.Complete || observed.Usage == nil || observed.Usage.InputTokens != 17 || observed.Usage.OutputTokens != 9 || observed.Usage.CachedInputTokens == nil || *observed.Usage.CachedInputTokens != 3 {
						b.Fatalf("usage observation lost native counters: %+v %v", observed, err)
					}
				}
			})
		}
	}
}
