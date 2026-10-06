package media

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRunwayTasksReadAsOpenAIVideos(t *testing.T) {
	size, seconds := "720x1280", "8"
	call, failure := encodeRunwayCreate(&Request{Op: OpVideoCreate, Prompt: "rain", Size: &size, Seconds: &seconds}, "gen4.5")
	if failure != nil {
		t.Fatal(failure.Message)
	}
	body, _ := call.JSONFrom(nil)
	if call.Path != "text_to_video" || string(body) != `{"duration":8,"model":"gen4.5","promptText":"rain","ratio":"720:1280"}` {
		t.Fatalf("create = %s %s", call.Path, body)
	}
	created, _ := call.DecodeVideo([]byte(`{"id":"6f9b2c1d-1234-4abc-9def-0123456789ab","estimatedCost":50}`))
	if created.Status != "queued" || *created.Seconds != "8" {
		t.Fatalf("created = %+v", created)
	}
	call, failure = encodeRunwayCreate(&Request{Op: OpVideoCreate, Prompt: "rain"}, "gen4.5")
	if failure != nil {
		t.Fatal(failure.Message)
	}
	body, _ = call.JSONFrom(nil)
	if call.Path != "text_to_video" || string(body) != `{"duration":4,"model":"gen4.5","promptText":"rain","ratio":"1280:720"}` {
		t.Fatalf("default create = %s %s", call.Path, body)
	}
	reference := &Part{Handle: "frame", ContentType: "image/png"}
	call, _ = encodeRunwayCreate(&Request{Op: OpVideoCreate, Prompt: "rain", InputRef: reference}, "gen4.5")
	body, _ = call.JSONFrom(func(*Part) ([]byte, *Error) { return []byte("png"), nil })
	var sent map[string]any
	_ = json.Unmarshal(body, &sent)
	if call.Path != "image_to_video" || sent["promptImage"] != "data:image/png;base64,cG5n" || sent["duration"] != float64(4) || sent["ratio"] != "1280:720" {
		t.Fatalf("image to video = %s %s", call.Path, body)
	}
	for status, want := range map[string]string{"PENDING": "queued", "THROTTLED": "queued", "RUNNING": "in_progress", "SUCCEEDED": "completed", "FAILED": "failed", "CANCELLED": "failed"} {
		video, failure := runwayTaskObject([]byte(`{"id":"t","status":"` + status + `","progress":0.25,"createdAt":"2026-10-05T12:00:00Z","failureCode":"SAFETY.INPUT.TEXT"}`))
		if failure != nil || video.Status != want || *video.Progress != 25 || *video.CreatedAt != time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC).Unix() {
			t.Fatalf("%s = %+v %v", status, video, failure)
		}
	}
	if _, failure := encodeRunwayGet(&Request{JobID: "../organization"}, "gen4.5"); failure == nil {
		t.Fatal("a job ID escaped its task path")
	}
	if _, failure := encodeRunwayContent(&Request{JobID: "t", Variant: "thumbnail"}, "gen4.5"); failure == nil {
		t.Fatal("Runway served a thumbnail")
	}
	content, _ := encodeRunwayContent(&Request{JobID: "t"}, "gen4.5")
	if step, failure := content.Next([]byte(`{"id":"t","status":"RUNNING"}`)); step != nil || failure == nil || failure.Status != 409 {
		t.Fatalf("content of a running task: %+v %+v", step, failure)
	}
	if step, _ := content.Next([]byte(`{"id":"t","status":"SUCCEEDED","output":["https://dnznrvs05pmza.cloudfront.net/video.mp4?sig=1"]}`)); step == nil || step.Credentials || step.Asset != "video/" {
		t.Fatalf("content step = %+v", step)
	}
}
