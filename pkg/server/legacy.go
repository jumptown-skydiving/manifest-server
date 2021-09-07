// (c) Copyright 2017-2021 Matt Messier

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/orangematt/manifest-server/pkg/burble"
	"github.com/orangematt/manifest-server/pkg/core"
	"github.com/orangematt/manifest-server/pkg/metar"
	"github.com/orangematt/manifest-server/pkg/settings"
)

type Manifest struct {
	Settings    *settings.Settings `json:"settings"`
	JumprunTime string             `json:"jumprun_time,omitempty"`
	WindsTime   string             `json:"winds_time,omitempty"`
	ColumnCount int                `json:"column_count"`
	Temperature string             `json:"temperature"`
	Winds       string             `json:"winds"`
	Clouds      string             `json:"clouds"`
	Weather     string             `json:"weather"`
	Separation  string             `json:"separation"`
	Message     string             `json:"message,omitempty"`
	Loads       []*burble.Load     `json:"loads"`
}

func (s *WebServer) windsAloftString() (string, string) {
	windsAloftSource := s.app.WindsAloftSource()

	color := "#ffffff"
	if windsAloftSource == nil {
		return color, ""
	}

	// We're only interested in 13000 feet
	samples := windsAloftSource.Samples()
	if len(samples) < 14 {
		return color, ""
	}
	sample := samples[13]

	var (
		str, t string
		speed  int
	)
	if sample.LightAndVariable {
		speed = 85
	} else {
		speed = 85 - sample.Speed
	}
	if speed <= 0 {
		color = "#ff0000"
		str = fmt.Sprintf("Winds are %d knots",
			sample.Speed)
	} else {
		str = fmt.Sprintf("Separation is %d seconds",
			s.app.SeparationDelay(speed))
	}

	t = fmt.Sprintf("(%d℃ / %d℉)", sample.Temperature,
		int64(metar.FahrenheitFromCelsius(float64(sample.Temperature))))

	if str != "" && t != "" {
		return color, fmt.Sprintf("%s %s", str, t)
	}
	if str == "" {
		return color, t
	}
	if t == "" {
		return color, str
	}

	return color, ""
}

func (s *WebServer) addToManifest(slots []string, jumper *burble.Jumper) []string {
	var color, prefix string
	if jumper.IsTandem {
		color = "#ffff00" // yellow
	} else if jumper.IsStudent || strings.HasSuffix(jumper.ShortName, " + Gear") {
		color = "#00ff00" // green
		if jumper.ShortName == "3500 H/P" {
			prefix = "Hop & Pop"
		} else if jumper.ShortName == "5500 H/P" {
			prefix = "Hop & Pop"
		}
	} else if strings.HasPrefix(jumper.ShortName, "3-5k") || strings.HasPrefix(jumper.ShortName, "3.5k") {
		color = "#ff00ff" // magenta
		prefix = "Hop & Pop"
	} else {
		color = "#ffffff" // white
	}

	if jumper.IsTandem {
		slots = append(slots, fmt.Sprintf("%s Tandem: %s", color,
			jumper.Name))
	} else if prefix != "" {
		slots = append(slots, fmt.Sprintf("%s %s: %s (%s)", color,
			prefix, jumper.Name, jumper.ShortName))
	} else {
		slots = append(slots, fmt.Sprintf("%s %s (%s)", color,
			jumper.Name, jumper.ShortName))
	}

	for _, member := range jumper.GroupMembers {
		slots = append(slots, fmt.Sprintf("%s \t%s (%s)", color,
			member.Name, member.ShortName))
	}

	return slots
}

func (s *WebServer) messageString() string {
	if message := s.app.Settings().Message(); message != "" {
		return message
	}

	sunrise, sunset, err := s.app.SunriseAndSunsetTimes()
	if err != nil {
		return ""
	}

	dzTimeNow := s.app.CurrentTime()
	if dzTimeNow.Before(sunset) {
		delta := sunset.Sub(dzTimeNow).Minutes()
		switch {
		case delta == 1:
			return "Sunset is in 1 minute"
		case delta == 60:
			return "Sunset is in 1 hour"
		case delta > 1 && delta < 60:
			return fmt.Sprintf("Sunset is in %d minutes", int(delta))
		}
	}
	if dzTimeNow.Before(sunrise) {
		delta := sunrise.Sub(dzTimeNow).Minutes()
		switch {
		case delta == 1:
			return "Sunrise is in 1 minute"
		case delta == 60:
			return "Sunrise is in 1 hour"
		case delta > 1 && delta < 60:
			return fmt.Sprintf("Sunrise is in %d minutes", int(delta))
		}
	}

	return ""
}

func (s *WebServer) updateManifestStaticData() {
	burbleSource := s.app.BurbleSource()
	metarSource := s.app.METARSource()
	settings := s.app.Settings()

	m := Manifest{
		Settings:    settings,
		ColumnCount: burbleSource.ColumnCount(),
		Message:     s.messageString(),
		Loads:       burbleSource.Loads(),
	}
	if t, ok := s.ContentModifyTime("/jumprun.json"); ok {
		m.JumprunTime = t.Format(http.TimeFormat)
	}
	if t, ok := s.ContentModifyTime("/winds"); ok {
		m.WindsTime = t.Format(http.TimeFormat)
	}
	if metarSource != nil {
		m.Temperature = metarSource.TemperatureString()
		m.Winds = metarSource.WindConditions()
		m.Clouds = metarSource.SkyCover()
		m.Weather = metarSource.WeatherConditions()
	}
	if b, err := json.Marshal(m); err == nil {
		s.SetContent("/manifest.json", b, "application/json; charset=utf-8")
	}
	aloftColor, aloftString := s.windsAloftString()
	m.Separation = aloftString

	// There are five lines of information that are shown on the upper
	// right of the display. Each line output is prefixed with a color to
	// use for rendering (of the form "#rrggbb") They are:
	//
	//   1. Time (temperature)
	//   2. Winds
	//   3. Clouds
	//   4. Weather
	//   5. Winds Aloft
	//
	// The next line is the message line that is displayed regardless of
	// whether there are any loads manifesting. There's no interface to set
	// it arbitrarily yet, but it's used to show a sunset/sunrise alert.
	// This line is also prefixed with the color to use to render it.
	//
	//   6. Message (arbitrary, time to sunset/sunrise)
	//
	// The remainder of the lines sent is variable, and all have to do with
	// the manifesting loads.
	//
	//   7. Integer: # of loads manifesting
	//
	// For each load that is manifesting, the following lines are present:
	//
	//   n. ID
	//   n+1. AircraftName
	//   n+2. LoadNumber
	//   n+3. CallMinutes
	//   n+4. SlotsFilled
	//   n+5. SlotsAvailable
	//   n+6. IsTurning
	//   n+7. IsFueling
	//   n+8..n+SlotsFilled+8. #rrggbb Manifest entry

	windsColor := "#ffffff"
	/*
		windSpeed := metarSource.WindSpeedMPH()
		windGusts := metarSource.WindGustSpeedMPH()
			if windSpeed >= 17.0 || windGusts >= 17.0 {
				windsColor = "#ff0000" // red
			} else if windGusts-windSpeed >= 7 {
				windsColor = "#ffff00" // yellow
			}
	*/

	lines := make([]string, 7)
	lines[0] = fmt.Sprintf("#ffffff %s", metarSource.TemperatureString())
	lines[1] = fmt.Sprintf("%s %s", windsColor, metarSource.WindConditions())
	lines[2] = fmt.Sprintf("#ffffff %s", metarSource.SkyCover())
	lines[3] = fmt.Sprintf("#ffffff %s", metarSource.WeatherConditions())
	lines[4] = fmt.Sprintf("%s %s", aloftColor, aloftString)
	lines[5] = fmt.Sprintf("#ffffff %s", s.messageString())

	loads := burbleSource.Loads()
	lines[6] = fmt.Sprintf("%d", len(loads))
	for _, load := range loads {
		var slots []string

		for _, j := range load.Tandems {
			slots = s.addToManifest(slots, j)
		}
		for _, j := range load.Students {
			slots = s.addToManifest(slots, j)
		}
		for _, j := range load.SportJumpers {
			slots = s.addToManifest(slots, j)
		}

		lines = append(lines, fmt.Sprintf("%d", load.ID))
		lines = append(lines, load.AircraftName)
		lines = append(lines, fmt.Sprintf("Load %s", load.LoadNumber))
		if load.IsNoTime {
			lines = append(lines, "")
		} else {
			lines = append(lines, fmt.Sprintf("%d", load.CallMinutes))
		}
		lines = append(lines, fmt.Sprintf("%d", len(slots)))
		if load.CallMinutes <= 5 {
			lines = append(lines, fmt.Sprintf("%d aboard", len(slots)))
		} else {
			slotsStr := "slots"
			if load.SlotsAvailable == 1 {
				slotsStr = "slot"
			}
			lines = append(lines, fmt.Sprintf("%d %s", load.SlotsAvailable, slotsStr))
		}
		if load.IsTurning {
			lines = append(lines, "1")
		} else {
			lines = append(lines, "0")
		}
		if load.IsFueling {
			lines = append(lines, "1")
		} else {
			lines = append(lines, "0")
		}
		lines = append(lines, slots...)
	}

	// Deprecated
	b := []byte(strings.Join(lines, "\n") + "\n")
	now := time.Now()
	s.SetContentFunc("/manifest",
		func(w http.ResponseWriter, req *http.Request) {
			h := w.Header()

			if t := m.JumprunTime; t != "" {
				h.Set("X-Jumprun-Time", t)
			}
			if t := m.WindsTime; t != "" {
				h.Set("X-Winds-Time", t)
			}
			o := settings.Options()
			h.Set("X-Display-Weather", strconv.FormatBool(o.DisplayWeather))
			h.Set("X-Display-Winds", strconv.FormatBool(o.DisplayWinds))
			h.Set("X-Display-Nicknames", strconv.FormatBool(o.DisplayNicknames))
			h.Set("X-Column-Count", strconv.FormatInt(int64(m.ColumnCount), 10))

			h.Set("Content-Type", "text/plain; charset=utf-8")
			http.ServeContent(w, req, "", now, bytes.NewReader(b))
		})
}

func (s *WebServer) updateWindsStaticData() {
	var lines []string
	samples := s.app.WindsAloftSource().Samples()
	for _, sample := range samples {
		line := fmt.Sprintf("%d %d %d %d %v",
			sample.Altitude, sample.Heading, sample.Speed,
			sample.Temperature, sample.LightAndVariable)
		lines = append(lines, line)
	}

	s.SetContent("/winds",
		[]byte(strings.Join(lines, "\n")+"\n"),
		"text/plain; charset=utf-8")

	if b, err := json.Marshal(samples); err == nil {
		s.SetContent("/winds.json", b, "application/json; charset=utf-8")
	}
}

func (s *WebServer) updateJumprunStaticData() {
	jumprun := s.app.Jumprun()
	if jumprun == nil {
		return
	}
	j := jumprun.Jumprun()

	modifyTime := time.Unix(j.TimeStamp, 0)
	s.SetContentWithTime("/jumprun",
		j.LegacyContent(), "text/plain; charset=utf-8", modifyTime)
	if jsonContent, err := json.Marshal(j); err != nil {
		s.SetContentWithTime("/jumprun.json",
			jsonContent, "application/json; charset=utf-8", modifyTime)
	}
}

func (s *WebServer) EnableLegacySupport() {
	// Initial legacy endpoint data
	s.SetContent("/manifest", []byte("\n\n\n\n\n\n0\n"), "text/plain; charset=utf-8")
	if s.app.Settings().WindsEnabled() {
		s.SetContent("/winds", []byte{}, "text/plain; charset=utf-8")
		s.SetContent("/winds.json", []byte("{}"), "application/json; charset=utf-8")
	}

	c := make(chan core.DataSource, 64)
	s.app.AddListener(c)

	// Spawn a goroutine to listen for events from the controller and update
	// the static content that's returned for legacy clients.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-s.app.Done():
				return
			case source := <-c:
			drain:
				for {
					select {
					case s := <-c:
						source |= s
					default:
						break drain
					}
				}
				if source&core.WindsAloftDataSource != 0 {
					fmt.Fprintf(os.Stderr, "Updating winds aloft data\n")
					s.updateWindsStaticData()
				}
				if source&core.JumprunDataSource != 0 {
					fmt.Fprintf(os.Stderr, "Updating jumprun data\n")
					s.updateJumprunStaticData()
				}
				s.updateManifestStaticData()
			}
		}
	}()
}