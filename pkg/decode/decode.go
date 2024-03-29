// (c) Copyright 2017-2021 Matt Messier

package decode

import (
	"strconv"
)

// Bool decodes a JSON encoded bool
func Bool(s string, i interface{}) bool {
	switch v := i.(type) {
	case bool:
		return v
	case string:
		if x, err := strconv.ParseBool(v); err == nil {
			//fmt.Printf("decode.Bool(%q: %#v %T)\n", s, v, v)
			return x
		}
		return false
	case int64:
		return v != 0
	case float64:
		return v != 0.0
	default:
		//fmt.Printf("decode.Bool(%q: %#v %T)\n", s, v, v)
		return false
	}
}

// Int decodes a JSON encoded signed integer
func Int(s string, i interface{}) int64 {
	switch v := i.(type) {
	case bool:
		if v {
			return 1
		}
		return 0
	case string:
		if x, err := strconv.ParseInt(v, 0, 64); err == nil {
			//fmt.Printf("decode.Int(%q: %#v %T)\n", s, v, v)
			return x
		}
		return 0
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		//fmt.Printf("decode.Int(%q: %#v %T)\n", s, v, v)
		return 0
	}
}
