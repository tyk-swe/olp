package promotion

import "maps"

type Directory struct {
	ids   map[string]string
	names map[string]string
}

func DirectoryOf(names map[string]string) Directory {
	return Directory{
		ids: maps.Collect(func(yield func(string, string) bool) {
			for id, name := range names {
				if !yield(Fold(name), id) {
					return
				}
			}
		}),
		names: names,
	}
}

func (d Directory) ID(name string) (string, bool) {
	id, ok := d.ids[Fold(name)]
	return id, ok
}

func (d Directory) Name(id string) (string, bool) {
	name, ok := d.names[id]
	return name, ok
}
