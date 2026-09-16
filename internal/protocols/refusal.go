package protocols

import "github.com/tyk-swe/olp/internal/protocols/openai"

func (t *streamTranslator) refusalDelta(text string) error {
	if text == "" {
		return nil
	}
	if err := t.start(t.id); err != nil {
		return err
	}
	t.refused = true
	if t.target == openai.FamilyChat {
		return t.chat(Object{"refusal": raw(text)}, nil, nil)
	}
	if t.target != openai.FamilyResponses {
		return t.textDelta(text)
	}
	if err := t.reserve(len(text)); err != nil {
		return err
	}
	t.refusal += text
	if t.refusalBlock == nil {
		index := t.nextBlock
		t.nextBlock++
		t.refusalBlock = &index
		item := Object{"id": raw("ref_" + t.id), "type": raw("message"), "role": raw("assistant"), "status": raw("in_progress"), "content": raw([]any{})}
		if e := t.send("response.output_item.added", Object{"type": raw("response.output_item.added"), "output_index": raw(index), "item": raw(item)}); e != nil {
			return e
		}
		if e := t.send("response.content_part.added", Object{"type": raw("response.content_part.added"), "output_index": raw(index), "item_id": raw("ref_" + t.id), "content_index": raw(0), "part": raw(Object{"type": raw("refusal"), "refusal": raw("")})}); e != nil {
			return e
		}
	}
	return t.send("response.refusal.delta", Object{"type": raw("response.refusal.delta"), "item_id": raw("ref_" + t.id), "output_index": raw(*t.refusalBlock), "content_index": raw(0), "delta": raw(text)})
}
func (t *streamTranslator) finishRefusal(output []Object) error {
	if t.refusalBlock == nil {
		return nil
	}
	index := *t.refusalBlock
	if e := t.send("response.refusal.done", Object{"type": raw("response.refusal.done"), "item_id": raw("ref_" + t.id), "output_index": raw(index), "content_index": raw(0), "refusal": raw(t.refusal)}); e != nil {
		return e
	}
	part := Object{"type": raw("refusal"), "refusal": raw(t.refusal)}
	if e := t.send("response.content_part.done", Object{"type": raw("response.content_part.done"), "item_id": raw("ref_" + t.id), "output_index": raw(index), "content_index": raw(0), "part": raw(part)}); e != nil {
		return e
	}
	output[index] = Object{"id": raw("ref_" + t.id), "type": raw("message"), "role": raw("assistant"), "status": raw("completed"), "content": raw([]Object{part})}
	return nil
}
