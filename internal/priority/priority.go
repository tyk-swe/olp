package priority

import "slices"

// Names lists admission classes in order of urgency.
var Names = []string{"critical", "high", "normal", "low"}

func Rank(name string) int { return slices.Index(Names, name) }
