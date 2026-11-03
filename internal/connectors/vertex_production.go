package connectors

import "net/url"

func cloudTestDestination(*url.URL) bool { return false }
