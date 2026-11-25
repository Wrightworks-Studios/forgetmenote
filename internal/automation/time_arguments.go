package automation

import (
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

type TimeArgument struct {
	OffsetSeconds int    `json:"offsetSeconds,omitempty"`
	Format        string `json:"format"`
}

func (c Call) At(now time.Time) (Call, error) {
	b, _ := json.Marshal(c.Arguments)
	args := map[string]any{}
	if len(b) > 0 && string(b) != "null" {
		if err := json.Unmarshal(b, &args); err != nil {
			return c, err
		}
	}
	for name, binding := range c.Time {
		if !identifier.MatchString(name) || binding.OffsetSeconds < -366*86400 || binding.OffsetSeconds > 86400 || binding.OffsetSeconds > 0 {
			return c, errors.New("invalid time binding")
		}
		at := now.UTC().Add(time.Duration(binding.OffsetSeconds) * time.Second)
		switch binding.Format {
		case "rfc3339":
			args[name] = at.Format(time.RFC3339Nano)
		case "date":
			args[name] = at.Format("20060102")
		case "unix":
			args[name] = strconv.FormatInt(at.Unix(), 10)
		case "unixnano":
			args[name] = strconv.FormatInt(at.UnixNano(), 10)
		default:
			return c, errors.New("unsupported time binding format")
		}
	}
	c.Arguments = args
	return c, nil
}
