package main

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// FlexInt is an int that accepts both numeric and string JSON values.
// AI models sometimes send numbers as strings (e.g. "20" instead of 20),
// which causes strict JSON unmarshaling to fail. This type handles both.
type FlexInt int

func (f *FlexInt) UnmarshalJSON(data []byte) error {
	// Try as number first
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*f = FlexInt(n)
		return nil
	}
	// Try as string
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("cannot parse %q as integer: %w", s, err)
		}
		*f = FlexInt(n)
		return nil
	}
	return fmt.Errorf("cannot unmarshal %s as integer", string(data))
}

func (f FlexInt) Int() int {
	return int(f)
}
