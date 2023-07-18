// (c) Copyright 2017-2021 Matt Messier

package jumprun

import (
	"fmt"
	"net/url"
	"strconv"
)

type Turn struct {
	Distance int `json:"distance"` // tenths of a mile
	Heading  int `json:"heading"`  // degrees from magnetic north
}

type Jumprun struct {
	TimeStamp           int64   `json:"timestamp"`            // time when set (UTC UnixNano)
	Heading             int     `json:"heading"`              // degrees from magnetic north
	ExitDistance        int     `json:"exit_distance"`        // tenths of a mile
	OffsetHeading       int     `json:"offset_heading"`       // degrees from magnetic north
	OffsetDistance      int     `json:"offset_distance"`      // tenths of a mile
	HookTurns           [4]Turn `json:"hook_turns"`           // list of turns if there's a hook
	Latitude            string  `json:"latitude"`             // latitude of jumprun origin
	Longitude           string  `json:"longitude"`            // longitude of jumprun origin
	MagneticDeclination int     `json:"magnetic_declination"` // magnetic declination at origin
	CameraHeight        int     `json:"camera_height"`        // camera height to use for view
	IsSet               bool    `json:"is_set"`               // true if jumprun is set
	Offsets             [4]int  `json:"offsets"`              // list of parallel runs (offsets are distances)
}

func (j *Jumprun) getIntValue(values url.Values, key string, defaultValue int) (int, error) {
	v := values.Get(key)
	if v == "" {
		return defaultValue, nil
	}
	value, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return defaultValue, fmt.Errorf("cannot parse %s: %w", key, err)
	}
	return int(value), nil
}

func (j *Jumprun) getCoordinate(values url.Values, key string, defaultValue string) (string, error) {
	v := values.Get(key)
	if v == "" {
		return defaultValue, nil
	}
	if _, err := strconv.ParseFloat(v, 64); err != nil {
		return defaultValue, fmt.Errorf("cannot parse %s: %w", key, err)
	}
	return v, nil
}
