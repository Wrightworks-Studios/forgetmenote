package automation

import (
	"strconv"
	"strings"
)

func ValidPointer(p string) bool {
	if p == "" {
		return true
	}
	if !strings.HasPrefix(p, "/") || len(p) > 1000 {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '~' {
			i++
			if i == len(p) || p[i] != '0' && p[i] != '1' {
				return false
			}
		}
	}
	return true
}

func Pointer(value any, pointer string) (any, bool) {
	if !ValidPointer(pointer) {
		return nil, false
	}
	if pointer == "" {
		return value, true
	}
	for _, part := range strings.Split(pointer[1:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = v[part]
			if !ok {
				return nil, false
			}
		case []any:
			i, e := strconv.Atoi(part)
			if e != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			value = v[i]
		default:
			return nil, false
		}
	}
	return value, true
}
