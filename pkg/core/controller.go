// (c) Copyright 2017-2021 Matt Messier

package core

import (
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/kelvins/sunrisesunset"
	"github.com/orangematt/manifest-server/pkg/burble"
	"github.com/orangematt/manifest-server/pkg/jumprun"
	"github.com/orangematt/manifest-server/pkg/metar"
	"github.com/orangematt/manifest-server/pkg/settings"
	"github.com/orangematt/manifest-server/pkg/winds"
)

type DataSource uint64

const (
	BurbleDataSource     DataSource = 0 << 1
	JumprunDataSource               = 1 << 1
	METARDataSource                 = 2 << 1
	WindsAloftDataSource            = 3 << 1
	SettingsDataSource              = 4 << 1
)

type Controller struct {
	mutex sync.Mutex

	location         *time.Location
	burbleSource     *burble.Controller
	jumprun          *jumprun.Controller
	metarSource      *metar.Controller
	windsAloftSource *winds.Controller

	settings  *settings.Settings
	listeners []chan DataSource
	done      chan struct{}
	wg        sync.WaitGroup
}

func NewController(settings *settings.Settings) (*Controller, error) {
	c := Controller{
		settings: settings,
		done:     make(chan struct{}),
	}

	loc, err := settings.Location()
	if err != nil {
		return nil, fmt.Errorf("Invalid timezone: %w", err)
	}
	c.location = loc

	c.burbleSource = burble.NewController(c.settings)
	c.launchDataSource(
		func() time.Time { return time.Now().Add(10 * time.Second) },
		"Burble",
		c.burbleSource.Refresh,
		func() { c.WakeListeners(BurbleDataSource) })

	if c.settings.METAREnabled() {
		c.metarSource = metar.NewController(c.settings.METARStation())
		c.launchDataSource(
			func() time.Time { return time.Now().Add(5 * time.Minute) },
			"METAR",
			c.metarSource.Refresh,
			func() { c.WakeListeners(METARDataSource) })
	}

	if c.settings.WindsEnabled() {
		c.windsAloftSource = winds.NewController(c.settings)
		c.launchDataSource(
			func() time.Time { return time.Now().Add(15 * time.Minute) },
			"Winds Aloft",
			c.windsAloftSource.Refresh,
			func() { c.WakeListeners(WindsAloftDataSource) })
	}

	if c.settings.JumprunEnabled() {
		c.jumprun = jumprun.NewController(c.settings,
			func() { c.WakeListeners(JumprunDataSource) })
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.runAtSunriseSunset()
	}()

	return &c, nil
}

func (c *Controller) Done() <-chan struct{} {
	return c.done
}

func (c *Controller) Close() {
	close(c.done)
	c.wg.Wait()
}

func (c *Controller) Settings() *settings.Settings {
	return c.settings
}

func (c *Controller) Location() *time.Location {
	return c.location
}

func (c *Controller) BurbleSource() *burble.Controller {
	return c.burbleSource
}

func (c *Controller) Jumprun() *jumprun.Controller {
	return c.jumprun
}

func (c *Controller) METARSource() *metar.Controller {
	return c.metarSource
}

func (c *Controller) WindsAloftSource() *winds.Controller {
	return c.windsAloftSource
}

func (c *Controller) CurrentTime() time.Time {
	return time.Now().In(c.Location())
}

func (c *Controller) SeparationDelay(speed int) int {
	msec := (1852.0 * float64(speed)) / 3600.0
	ftsec := msec / 0.3048
	return int(math.Ceil(1000.0 / ftsec))
}

func (c *Controller) launchDataSource(
	nextRefresh func() time.Time,
	sourceName string,
	refresh func() error,
	update func(),
) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			fmt.Fprintf(os.Stderr, "refreshing %s\n", sourceName)
			if err := refresh(); err != nil {
				fmt.Fprintf(os.Stderr, "Error refreshing %s: %v\n", sourceName, err)
			} else {
				update()
			}

			nextTime := nextRefresh()
			refreshPeriod := time.Until(nextTime)
			t := time.NewTicker(refreshPeriod)

			select {
			case <-c.Done():
				t.Stop()
				return
			case <-t.C:
				t.Stop()
				break
			}
		}
	}()
}

func (c *Controller) Coordinates() (latitude float64, longitude float64, err error) {
	if c.Jumprun() != nil {
		j := c.Jumprun().Jumprun()
		if j.IsSet && j.Latitude != "" && j.Longitude != "" {
			latitude, err = strconv.ParseFloat(j.Latitude, 64)
			if err == nil {
				longitude, err = strconv.ParseFloat(j.Longitude, 64)
				if err == nil {
					return
				}
			}
		}
	}
	if c.WindsAloftSource() != nil {
		settings := c.Settings()
		latitude, err = strconv.ParseFloat(settings.WindsLatitude(), 64)
		if err == nil {
			longitude, err = strconv.ParseFloat(settings.WindsLongitude(), 64)
			if err == nil {
				return
			}
		}
	}
	var ok bool
	if latitude, longitude, ok = c.METARSource().Location(); ok {
		return latitude, longitude, nil
	}
	err = errors.New("location is unknown")
	return
}

func (c *Controller) SunriseAndSunsetTimes() (sunrise time.Time, sunset time.Time, err error) {
	dzTimeNow := c.CurrentTime()
	_, utcOffset := dzTimeNow.Zone()

	var latitude, longitude float64
	latitude, longitude, err = c.Coordinates()
	if err != nil {
		return
	}

	sunrise, sunset, err = sunrisesunset.GetSunriseSunset(
		latitude, longitude, float64(utcOffset)/3600.0, dzTimeNow)
	if err != nil {
		return
	}

	year, month, day := dzTimeNow.Date()
	sunrise = time.Date(year, month, day, sunrise.Hour(), sunrise.Minute(), sunrise.Second(), 0, dzTimeNow.Location())
	sunset = time.Date(year, month, day, sunset.Hour(), sunset.Minute(), sunset.Second(), 0, dzTimeNow.Location())

	return
}

func (c *Controller) AddListener(l chan DataSource) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.listeners = append(c.listeners, l)
}

func (c *Controller) WakeListeners(source DataSource) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	for _, l := range c.listeners {
		l <- source
	}
}

func (c *Controller) sunrise() {
	fmt.Fprintf(os.Stderr, "Running sunrise events\n")
	// Clear the active jumprun at sunrise
	if c.Jumprun() != nil {
		if sunrise, _, err := c.SunriseAndSunsetTimes(); err == nil {
			dzTimeNow := c.CurrentTime()
			activeJumprunTime := time.Unix(c.jumprun.Jumprun().TimeStamp, 0).In(c.Location())
			if activeJumprunTime.Before(sunrise) && dzTimeNow.After(sunrise) {
				c.Jumprun().Reset()
				if err = c.Jumprun().Write(); err != nil {
					fmt.Fprintf(os.Stderr, "cannot save jumprun state: %v\n", err)
				}
			}
		}
	}
}

func (c *Controller) sunset() {
	// Currently nothing to do at sunset
	fmt.Fprintf(os.Stderr, "Running sunset events\n")
}

func (c *Controller) runAtSunriseSunset() {
	lastSunrise := []int{0, 0, 0}
	lastSunset := []int{0, 0, 0}
	t := time.NewTicker(20 * time.Second)
	for {
		sunrise, sunset, err := c.SunriseAndSunsetTimes()
		if err != nil {
			fmt.Fprintf(os.Stderr, "SunriseAndSunsetTimes ERROR: %v\n", err)
			return
		}

		now := c.CurrentTime()
		if now.Equal(sunset) || now.After(sunset) {
			y, m, d := sunset.Date()
			thisSunset := []int{y, int(m), d}
			if !reflect.DeepEqual(lastSunset, thisSunset) {
				c.sunset()
				lastSunset = thisSunset
			}
		}
		if now.Equal(sunrise) || now.After(sunrise) {
			y, m, d := sunrise.Date()
			thisSunrise := []int{y, int(m), d}
			if !reflect.DeepEqual(lastSunrise, thisSunrise) {
				c.sunrise()
				lastSunrise = thisSunrise
			}
		}

		select {
		case <-c.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}