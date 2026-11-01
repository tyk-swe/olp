package wire

import "testing"

func TestRM6FormatsShareTheirAccountingOperation(t *testing.T) {
	if DocumentAIProcess.Operation() != OCR.Operation() {
		t.Fatalf("Document AI work must be accounted as OCR, got %q next to %q", DocumentAIProcess.Operation(), OCR.Operation())
	}
	if DocumentAIProcess.Operation() == OperationGeneration {
		t.Fatal("Document AI work fell through to generation accounting")
	}
	if NativeDuplex.Operation() != GeminiLive.Operation() || NativeDuplex.Operation() != Realtime.Operation() {
		t.Fatalf("native duplex sessions must be accounted as realtime, got %q next to %q", NativeDuplex.Operation(), GeminiLive.Operation())
	}
	if Passthrough.Operation() != string(Passthrough) {
		t.Fatalf("passthrough traffic must keep its own operation tag, got %q", Passthrough.Operation())
	}
}

func TestWorkflowTranscriptFieldFollowsTheDialectFamily(t *testing.T) {
	for _, format := range []Format{GeminiStream, GeminiCount, GeminiInteractions, GeminiLive, GeminiFile, GeminiBatch} {
		if format.WorkflowTranscriptField() != Gemini.WorkflowTranscriptField() {
			t.Fatalf("%s must read its workflow transcript from the Gemini field, got %q next to %q", format, format.WorkflowTranscriptField(), Gemini.WorkflowTranscriptField())
		}
	}
	if Anthropic.WorkflowTranscriptField() != Chat.WorkflowTranscriptField() {
		t.Fatalf("anthropic and chat transcripts must share a field, got %q next to %q", Anthropic.WorkflowTranscriptField(), Chat.WorkflowTranscriptField())
	}
	fields := map[string]Format{}
	for _, format := range []Format{Chat, Responses, Gemini} {
		field := format.WorkflowTranscriptField()
		if field == "" {
			t.Fatalf("%s has no workflow transcript field", format)
		}
		if other, clash := fields[field]; clash {
			t.Fatalf("%s and %s must not share workflow transcript field %q", format, other, field)
		}
		fields[field] = format
	}
}

func TestRM6FormatsSurfaceAsNativeDialects(t *testing.T) {
	for _, format := range []Format{NativeDuplex, Passthrough} {
		if format.Surface() != MistralFIM.Surface() || format.Surface() != CohereChat.Surface() {
			t.Fatalf("%s must surface with the other native dialects, got %q next to %q", format, format.Surface(), MistralFIM.Surface())
		}
	}
	if DocumentAIProcess.Surface() == NativeDuplex.Surface() && DocumentAIProcess.Operation() == NativeDuplex.Operation() {
		t.Fatal("Document AI and native duplex collapsed into one accounting identity")
	}
}
