package cache

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/tyk-swe/olp/internal/oif"
)

// Canonical uses the validated source index, never float64 decoding. Numeric
// spelling is preserved, so distinct exact numbers cannot collide.
func Canonical(doc oif.Document) []byte {
	var out bytes.Buffer
	writeValue(&out, doc.Root())
	return out.Bytes()
}
func writeValue(out *bytes.Buffer, v oif.Value) {
	switch v.Kind() {
	case oif.Object:
		members := v.Members()
		sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
		out.WriteByte('{')
		for i, m := range members {
			if i > 0 {
				out.WriteByte(',')
			}
			name, _ := json.Marshal(m.Name)
			out.Write(name)
			out.WriteByte(':')
			writeValue(out, m.Value)
		}
		out.WriteByte('}')
	case oif.Array:
		out.WriteByte('[')
		for i, item := range v.Elements() {
			if i > 0 {
				out.WriteByte(',')
			}
			writeValue(out, item)
		}
		out.WriteByte(']')
	case oif.String:
		s, _ := v.Text()
		b, _ := json.Marshal(s)
		out.Write(b)
	default:
		out.WriteString(v.Raw())
	}
}
