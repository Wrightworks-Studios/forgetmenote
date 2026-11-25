package workspace

import (
	"regexp"
)

var taskName = regexp.MustCompile(`^t-[a-z0-9]{4,16}$`)
