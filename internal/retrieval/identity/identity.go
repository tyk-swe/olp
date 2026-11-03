package identity

import "regexp"

const (
	Bedrock = "bedrock_knowledge_base"
	Vertex  = "vertex_rag"
	Azure   = "azure_ai_search"
)

var knowledgeBaseID = regexp.MustCompile(`^[A-Za-z0-9]{10}$`)
var corpusName = regexp.MustCompile(`^projects/[A-Za-z0-9_-]+/locations/[a-z0-9-]+/ragCorpora/[A-Za-z0-9_-]+$`)
var indexName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

func ValidResource(backend, resource string) bool {
	switch backend {
	case Bedrock:
		return knowledgeBaseID.MatchString(resource)
	case Vertex:
		return corpusName.MatchString(resource)
	case Azure:
		return indexName.MatchString(resource)
	}
	return false
}
