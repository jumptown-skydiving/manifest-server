// (c) Copyright 2017-2021 Matt Messier

package metar

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/jumptown-skydiving/manifest-server/pkg/settings"
)

// FahrenheitFromCelsius converts a temperature from Celsius to Fahrenheit.
func FahrenheitFromCelsius(c float64) float64 {
	return ((c * 9.0) / 5.0) + 32.0
}

var cardinalDirections = []string{
	"N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE",
	"S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW",
}

// MPHFromKnots converts a speed from knots to miles per hour.
func MPHFromKnots(kts float64) float64 {
	return kts * 1.151
}

// CardinalDirection returns the cardinal direction expressed as a string
// from a value in degrees.
func CardinalDirection(degrees float64) string {
	for degrees < 0.0 {
		degrees += 360.0
	}
	n := math.Floor(math.Mod(degrees+11.25, 360.0) / 22.5)
	return cardinalDirections[int(n)]
}

var descriptors = map[string]string{
	"MI": "shallow ",
	"PR": "partial ",
	"BC": "patches of ",
	"DR": "low drifting ",
	"BL": "blowing ",
	"SH": "showers ",
	"TS": "thunderstorm ",
	"FZ": "freezing ",
}

var conditions = map[string]string{
	"RA": "rain",
	"DZ": "drizzle",
	"SN": "snow",
	"SG": "snow grains",
	"IC": "ice crystals",
	"PL": "ice pellets",
	"GR": "hail",
	"GS": "small hail and/or snow pellets",
	"FG": "fog",
	"VA": "volcanic ash",
	"BR": "mist",
	"HZ": "haze",
	"DU": "widespread dust",
	"FU": "smoke",
	"SA": "sand",
	"PY": "spray",
	"SQ": "squall",
	"PO": "dust or sand whirls",
	"DS": "dust storm",
	"SS": "sandstorm",
	"FC": "funnel cloud",
	"UP": "unknown precipitation",
}

func parseWeatherCondition(parts []string) (int, string) {
	var results []string

	i := 0
	for i < len(parts) {
		var intensity, suffix string

		bit := parts[i]
		switch {
		case strings.HasPrefix(bit, "-"):
			intensity = "light "
			i++
			bit = parts[i]
		case strings.HasPrefix(bit, "+"):
			intensity = "heavy "
			i++
			bit = parts[i]
		case bit == "VC":
			suffix = " in the vicinity"
			i++
			bit = parts[i]
		}

		descriptor, ok := descriptors[bit]
		if ok {
			i++
			if i >= len(parts) {
				results = append(results,
					intensity+descriptor+suffix)
				break
			}
			bit = parts[i]
		}

		condition, ok := conditions[bit]
		if !ok {
			break
		}

		i++
		results = append(results, intensity+descriptor+condition+suffix)
	}

	if len(results) == 0 {
		return i, "clear"
	}
	return i, strings.Join(results, ", ")
}

type Winds struct {
	Speed        int
	Direction    int
	Gusting      int
	Variable     bool
	VariableLow  int
	VariableHigh int
}

func (w *Winds) parse(in []string) int {
	w.Variable = strings.HasPrefix(in[0], "VRB")
	if !w.Variable {
		w.Direction, _ = strconv.Atoi(in[0])
	}
	if kt := strings.Index(in[0], "KT"); kt >= 0 {
		in[0] = in[0][:kt]
	}
	if g := strings.Index(in[0], "G"); g >= 0 {
		w.Gusting, _ = strconv.Atoi(in[0][g+1:])
		in[0] = in[0][:g]
	}
	w.Speed, _ = strconv.Atoi(in[0])
	if len(in) > 1 {
		if v := strings.Index(in[1], "V"); v > 0 {
			w.VariableLow, _ = strconv.Atoi(in[1][:v])
			w.VariableHigh, _ = strconv.Atoi(in[1][v+1:])
			return 2
		}
	}
	return 1
}

type Controller struct {
	settings *settings.Settings

	lock           sync.Mutex
	windConditions Winds
	skyCover       string
	wxCondition    string
	temperature    float64
}

func NewController(settings *settings.Settings) *Controller {
	return &Controller{
		settings: settings,
	}
}

const metarURL = "https://aviationweather.gov/api/data/metar?format=raw&hours=2"

// Refresh retrieves and parses weather data.
func (c *Controller) Refresh() (bool, error) {
	url := fmt.Sprintf("%s&ids=%s", metarURL, c.settings.METARStation())
	resp, err := http.Get(url)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}

	// There should be at least 1 line. Any less is invalid data.
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 1 {
		return false, errors.New("No data returned")
	}

	for _, line := range lines {
		var ok bool
		if ok, err = c.parseLine(line); ok {
			return true, nil
		}
	}

	return false, errors.New("No usable data returned")
}

func (c *Controller) parseLine(line string) (bool, error) {
	fields := strings.Split(strings.TrimSpace(line), " ")
	if fields[0] != "SPECI" && fields[0] != "METAR" {
		return false, fmt.Errorf("Unrecognized record type %s", fields[0])
	}
	if fields[1] != c.settings.METARStation() {
		return false, fmt.Errorf("Report for incorrect station %s", fields[1])
	}
	// fields[2] observation time
	// fields[3] AUTO or maybe something else if not automatic (COR for corrected)
	//
	// Several things can consume multiple fields, so we'll keep an
	// index to skip over them as needed
	idx := 4
	if idx >= len(fields) {
		return false, fmt.Errorf("unexpected end of input (start)")
	}

	var windConditions Winds
	idx += windConditions.parse(fields[idx:])
	if idx >= len(fields) {
		return false, fmt.Errorf("unexpected end of input (winds)")
	}

	// visibility
	for ; idx < len(fields); idx += 1 {
		if strings.HasSuffix(fields[idx], "SM") {
			continue
		}
		break
	}
	if idx >= len(fields) {
		return false, fmt.Errorf("unexpected end of input (visibility)")
	}

	// runway visual range
	for ; idx < len(fields); idx += 1 {
		if strings.HasPrefix(fields[idx], "R") {
			continue
		}
		break
	}
	if idx >= len(fields) {
		return false, fmt.Errorf("unexpected end of input (runway visual range)")
	}

	n, wxCondition := parseWeatherCondition(fields[idx:])
	idx += n
	if idx >= len(fields) {
		return false, fmt.Errorf("unexpected end of input (weather conditions)")
	}

	n, skyCover := parseSkyCover(fields[idx:])
	idx += n
	if idx >= len(fields) {
		return false, fmt.Errorf("unexpected end of input (sky cover)")
	}

	var temperatureInt int
	var temperatureFloat float64
	if slash := strings.Index(fields[idx], "/"); slash > 0 {
		fields[idx] = fields[idx][:slash]
		neg := strings.HasPrefix(fields[idx], "M")
		if neg {
			fields[idx] = fields[idx][1:]
		}
		temperatureInt, _ = strconv.Atoi(fields[idx])
		if neg {
			temperatureInt = -temperatureInt
		}
		temperatureFloat = float64(temperatureInt)
	}
	idx += 1
	if idx >= len(fields) {
		return false, fmt.Errorf("unexpected end of input (temperature)")
	}

	// altimeter
	for ; idx < len(fields); idx += 1 {
		if !strings.HasPrefix(fields[idx], "A") {
			break
		}
	}

	// Parse remarks
	// All we're interested in is temperature that's better than
	// fields[8] because it gives 10ths
	for ; idx < len(fields); idx += 1 {
		if fields[idx] == "RMK" {
			for idx += 1; idx < len(fields); idx += 1 {
				if fields[idx][0] == 'T' {
					// TODO parse out this temperature
				}
			}
			break
		}
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	changed := false
	if c.skyCover != skyCover {
		c.skyCover = skyCover
		changed = true
	}
	if c.wxCondition != wxCondition {
		c.wxCondition = wxCondition
		changed = true
	}
	if c.windConditions != windConditions {
		c.windConditions = windConditions
		changed = true
	}
	if c.temperature != temperatureFloat {
		c.temperature = temperatureFloat
		changed = true
	}

	return changed, nil
}

func parseSkyCover(in []string) (int, string) {
	var (
		lowClouds, highClouds []string
	)

	idx := 0
loop:
	for ; idx < len(in); idx += 1 {
		if strings.HasPrefix(in[idx], "VV") {
			base, err := strconv.Atoi(in[idx][2:])
			if err == nil {
				base *= 100
				highClouds = append(highClouds, fmt.Sprintf("ceiling at %d", base))
			} else {
				highClouds = append(highClouds, "overcast")
			}
			break loop
		}
		base, err := strconv.Atoi(in[idx][3:])
		if err == nil {
			base *= 100
			switch in[idx][0:2] {
			case "FEW":
				lowClouds = append(lowClouds, fmt.Sprintf("few at %d", base))
			case "SCT":
				lowClouds = append(lowClouds, fmt.Sprintf("scattered at %d", base))
			case "BKN":
				highClouds = append(highClouds, fmt.Sprintf("broken at %d", base))
			case "OVC":
				highClouds = append(highClouds, fmt.Sprintf("overcast deck at %d", base))
				break loop
			case "OVX":
				highClouds = append(highClouds, "overcast")
				break loop
			case "SKC", "CLR":
				highClouds = append(highClouds, "clear")
				break loop
			default:
				break loop
			}
		}
	}

	if len(highClouds) > 0 {
		return idx, strings.Join(highClouds, ", ")
	}
	if len(lowClouds) > 0 {
		return idx, strings.Join(lowClouds, ", ")
	}
	return idx, "clear"
}

// WindSpeedMPH returns the current wind speed in MPH.
func (c *Controller) WindSpeedMPH() float64 {
	c.lock.Lock()
	defer c.lock.Unlock()
	return MPHFromKnots(float64(c.windConditions.Speed))
}

// WindGustSpeedMPH returns current wind gust speed in MPH.
func (c *Controller) WindGustSpeedMPH() float64 {
	c.lock.Lock()
	defer c.lock.Unlock()
	return MPHFromKnots(float64(c.windConditions.Gusting))
}

// WindDirectionDegrees returns the current wind direction in degrees.
func (c *Controller) WindDirectionDegrees() float64 {
	c.lock.Lock()
	defer c.lock.Unlock()
	windDirectionDegrees := c.windConditions.Direction
	// Adjust true north to magnetic north. Magnetic deviance for Orange, MA is -14.7
	// This is a little gross, but use jumprun's magnetic declination setting
	return float64((int(windDirectionDegrees) + c.settings.JumprunMagneticDeclination() + 360) % 360)
}

// WindConditions returns the current wind conditions as a human-readable string.
func (c *Controller) WindConditions() string {
	c.lock.Lock()
	w := c.windConditions
	c.lock.Unlock()

	if w.Variable || w.Speed <= 0 {
		return "light and variable"
	}

	windDirection := CardinalDirection(float64(w.Direction))

	if w.Gusting > 0 {
		return fmt.Sprintf("%d MPH gusting to %d MPH from %d° (%s)",
			int64(w.Speed), int64(w.Gusting),
			int64(w.Direction), windDirection)
	}
	return fmt.Sprintf("%d MPH from %d° (%s)",
		int64(w.Speed), int64(w.Direction), windDirection)
}

// WeatherConditions returns a human-readable description of current weather
// conditions (raining, snowing, clear, etc.)
func (c *Controller) WeatherConditions() string {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.wxCondition == "" {
		return "data error"
	}
	return c.wxCondition
}

// SkyCover returns a human-readable description of the current sky cover.
func (c *Controller) SkyCover() string {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.skyCover == "" {
		return "data error"
	}
	return c.skyCover
}

// TemperatureString returns a human-readable temperature string
func (c *Controller) TemperatureString() string {
	c.lock.Lock()
	defer c.lock.Unlock()

	temp := c.temperature
	return fmt.Sprintf("%d℃ / %d℉",
		int64(temp), int64(FahrenheitFromCelsius(temp)))
}

func (c *Controller) Location() (float64, float64, bool) {
	// METAR data API no longer returns latitude/longitude
	// Use winds data instead
	var (
		err                 error
		latitude, longitude float64
	)
	latitude, err = strconv.ParseFloat(c.settings.WindsLatitude(), 64)
	if err == nil {
		longitude, err = strconv.ParseFloat(c.settings.WindsLongitude(), 64)
	}
	return latitude, longitude, err == nil
}
