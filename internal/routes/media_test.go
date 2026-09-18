package routes

import "testing"

func TestMediaSimulationUsesNativeOperationModes(t *testing.T) {
	for _, op := range []string{"image_generation", "image_edit", "speech", "transcription"} {
		for _, mode := range []string{"unary", "streaming"} {
			if err := validTuple(op, "openai", mode); err != nil {
				t.Errorf("%s/%s rejected: %v", op, mode, err)
			}
		}
	}
	for _, op := range []string{"image_variation", "video_list", "video_get", "video_content", "video_delete"} {
		if err := validTuple(op, "openai", "unary"); err != nil {
			t.Errorf("%s rejected: %v", op, err)
		}
	}
	if err := validTuple("video_create", "openai", "async"); err != nil {
		t.Fatal(err)
	}
	for _, tuple := range [][3]string{{"video_create", "openai", "unary"}, {"video_get", "openai", "streaming"}, {"speech", "anthropic", "unary"}, {"image_variation", "openai", "streaming"}} {
		if err := validTuple(tuple[0], tuple[1], tuple[2]); err == nil {
			t.Errorf("invalid tuple accepted: %v", tuple)
		}
	}
}
