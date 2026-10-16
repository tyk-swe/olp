package gateway

import (
	"io"
	"net/http"
)

func ptrInt64(v int64) *int64 { return &v }

func streamChunk(model, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"`+model+`","choices":[{"index":0,"delta":{"content":"`+text+`"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}
}
