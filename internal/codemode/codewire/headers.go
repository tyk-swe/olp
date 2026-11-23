package codewire

import (
	"net/http"
	"strings"
)

// ForwardHeaders excludes authentication, OLP controls and hop-by-hop fields.
func ForwardHeaders(source http.Header, request bool) http.Header {
	result := source.Clone()
	removeConnectionFields(result, source)
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		result.Del(name)
	}
	for name := range result {
		if strings.HasPrefix(strings.ToLower(name), "x-olp-") {
			result.Del(name)
		}
	}
	if request {
		for _, name := range []string{"Authorization", "Chatgpt-Account-Id", "Cookie", "X-Api-Key", "X-Goog-Api-Key"} {
			result.Del(name)
		}
	}
	return result
}

// ForwardTrailers also excludes fields nominated by the original message's
// Connection headers, which are absent from its trailer map.
func ForwardTrailers(source, headers http.Header, request bool) http.Header {
	result := ForwardHeaders(source, request)
	removeConnectionFields(result, headers)
	return result
}

func removeConnectionFields(result, headers http.Header) {
	for _, connection := range headers.Values("Connection") {
		for _, name := range strings.Split(connection, ",") {
			result.Del(strings.TrimSpace(name))
		}
	}
}
