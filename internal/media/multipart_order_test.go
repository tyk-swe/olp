package media

import (
	"slices"
	"testing"
)

func TestFileArraysPreserveNumericAndRepeatedPartOrder(t *testing.T) {
	form := &Form{files: map[string][]Part{
		"image":     {{Filename: "bare-first"}, {Filename: "bare-second"}},
		"image[10]": {{Filename: "ten"}},
		"image[2]":  {{Filename: "two-first"}, {Filename: "two-second"}},
		"image[0]":  {{Filename: "zero"}},
		"image[]":   {{Filename: "array-first"}, {Filename: "array-second"}},
		"mask":      {{Filename: "mask"}},
	}}
	parts, err := form.TakeFilesWithPrefix("image")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, part := range parts {
		names = append(names, part.Filename)
	}
	want := []string{"bare-first", "bare-second", "zero", "two-first", "two-second", "ten", "array-first", "array-second"}
	if !slices.Equal(names, want) {
		t.Fatalf("file order = %v, want %v", names, want)
	}
	if len(form.files) != 1 || len(form.files["mask"]) != 1 {
		t.Fatalf("unexpected remaining files: %v", form.files)
	}
}

func TestFileArraysRejectMalformedIndices(t *testing.T) {
	for _, name := range []string{"image[01]", "image[-1]", "image[abc]", "image[1", "image[1][2]", "image[999999999999999999999999]"} {
		t.Run(name, func(t *testing.T) {
			form := &Form{files: map[string][]Part{name: {{Filename: "invalid"}}}}
			if _, err := form.TakeFilesWithPrefix("image"); err == nil {
				t.Fatal("accepted a malformed file index")
			}
		})
	}
}

func TestExtraFieldsUseDeterministicKeyOrder(t *testing.T) {
	fields := extraFields(nil, map[string]any{"z": "last", "a": []any{"first", "second"}, "middle": 2})
	var values []string
	for _, field := range fields {
		if field.Text == nil {
			t.Fatal("extension field lost its text value")
		}
		values = append(values, field.Name+"="+*field.Text)
	}
	want := []string{"a[]=first", "a[]=second", "middle=2", "z=last"}
	if !slices.Equal(values, want) {
		t.Fatalf("extension field order = %v, want %v", values, want)
	}
}
