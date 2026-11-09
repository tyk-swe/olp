package provider

import "strconv"

type path string

func (p path) field(name string) string {
	if p == "" {
		return name
	}
	return string(p) + "." + name
}

func (p path) index(i int) path { return path(string(p) + "." + strconv.Itoa(i)) }

func (p path) at(name string, i int) path { return path(p.field(name) + "." + strconv.Itoa(i)) }
