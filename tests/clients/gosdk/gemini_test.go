//go:build integration

package gosdk

// geminiPath is the path of a native Gemini method on the scripted upstream.
func (h harness) geminiPath(method string) string {
	return "/gemini/v1beta/models/" + h.upstreamModels["gemini"] + ":" + method
}
