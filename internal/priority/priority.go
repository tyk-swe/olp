package priority

const (
	Critical = iota
	High
	Normal
	Low
	Count
)

type Class struct {
	Name   string
	Weight int
}

func Classes() [Count]Class {
	return [Count]Class{
		Critical: {Name: "critical", Weight: 8},
		High:     {Name: "high", Weight: 4},
		Normal:   {Name: "normal", Weight: 2},
		Low:      {Name: "low", Weight: 1},
	}
}

func Rank(name string) int {
	for rank, class := range Classes() {
		if class.Name == name {
			return rank
		}
	}
	return -1
}
