// (c) Copyright 2017-2021 Matt Messier

package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/orangematt/manifest-server/pkg/burble"
	"github.com/orangematt/manifest-server/pkg/core"
	"github.com/orangematt/manifest-server/pkg/settings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type addClientResponse struct {
	id uint64
}

type addClientRequest struct {
	reply   chan addClientResponse
	updates chan *ManifestUpdate
}

type removeClientResponse struct{}

type removeClientRequest struct {
	reply chan removeClientResponse
	id    uint64
}

type manifestServiceServer struct {
	UnimplementedManifestServiceServer

	app     *core.Controller
	options settings.Options
	wg      sync.WaitGroup
	cancel  context.CancelFunc

	addClientChan    chan addClientRequest
	removeClientChan chan removeClientRequest
}

func newManifestServiceServer(controller *core.Controller) *manifestServiceServer {
	return &manifestServiceServer{
		app:              controller,
		addClientChan:    make(chan addClientRequest, 16),
		removeClientChan: make(chan removeClientRequest, 16),
	}
}

func (s *manifestServiceServer) translateJumper(j *burble.Jumper, leader *Jumper) *Jumper {
	var color, prefix string
	if leader != nil {
		color = leader.Color
	} else {
		switch {
		case j.IsTandem:
			color = "#ffff00" // yellow
		case j.IsStudent || strings.HasSuffix(j.ShortName, " + Gear"):
			color = "#00ff00" // green
			if strings.HasSuffix(j.ShortName, " H/P") {
				prefix = "Hop & Pop"
			}
		case strings.HasPrefix(j.ShortName, "3-5k") || strings.HasPrefix(j.ShortName, "3.5k"):
			color = "#ff00ff" // magenta
			prefix = "Hop & Pop"
		default:
			color = "#ffffff" // white
		}
	}

	var name, repr string
	if s.options.DisplayNicknames && j.Nickname != "" {
		name = j.Nickname
	} else {
		name = j.Name
	}
	if leader == nil {
		switch {
		case j.IsTandem:
			repr = "Tandem: " + name
		case prefix != "":
			repr = fmt.Sprintf("%s: %s (%s)", prefix, name, j.ShortName)
		default:
			repr = fmt.Sprintf("%s (%s)", name, j.ShortName)
		}
	} else {
		repr = fmt.Sprintf("\t%s (%s)", name, j.ShortName)
	}

	t := JumperType_EXPERIENCED
	if j.IsVideographer {
		t = JumperType_VIDEOGRAPHER
	} else if leader != nil {
		switch leader.Type {
		case JumperType_TANDEM_STUDENT:
			if j.IsInstructor {
				t = JumperType_TANDEM_INSTRUCTOR
			}
		case JumperType_AFF_STUDENT:
			if j.IsInstructor {
				t = JumperType_AFF_INSTRUCTOR
			}
		case JumperType_COACH_STUDENT:
			if j.IsInstructor {
				t = JumperType_COACH
			}
		}
	} else {
		switch {
		case j.IsTandem:
			t = JumperType_TANDEM_STUDENT
		case j.IsStudent:
			// TODO how to distinguish between AFF / Coach?
			t = JumperType_AFF_STUDENT
		}
	}

	return &Jumper{
		Id:        uint64(j.ID),
		Type:      t,
		Name:      j.Name,
		ShortName: j.ShortName,
		Nickname:  j.Nickname,
		Color:     color,
		Repr:      repr,
	}
}

func (s *manifestServiceServer) slotFromJumper(j *burble.Jumper) *LoadSlot {
	if len(j.GroupMembers) == 0 {
		return &LoadSlot{
			Slot: &LoadSlot_Jumper{
				Jumper: s.translateJumper(j, nil),
			},
		}
	}

	g := &JumperGroup{
		Leader: s.translateJumper(j, nil),
	}
	for _, member := range j.GroupMembers {
		g.Members = append(g.Members, s.translateJumper(member, g.Leader))
	}

	return &LoadSlot{
		Slot: &LoadSlot_Group{
			Group: g,
		},
	}
}

func (s *manifestServiceServer) constructUpdate(source core.DataSource) *ManifestUpdate {
	u := &ManifestUpdate{}

	const optionsSources = core.OptionsDataSource
	if source&optionsSources != 0 {
		s.options = s.app.Settings().Options()
		o := s.options
		u.Options = &Options{
			DisplayNicknames: o.DisplayNicknames,
			DisplayWeather:   o.DisplayWeather,
			DisplayWinds:     o.DisplayWinds,
			Message:          o.Message,
			MessageColor:     "#ffffff",
		}
	}

	const statusSources = core.METARDataSource | core.WindsAloftDataSource
	if source&statusSources != 0 {
		var separationColor, separationString string
		if s.app.WindsAloftSource() != nil {
			separationColor, separationString = s.app.SeparationStrings()
		} else {
			separationColor = "#ffffff"
		}

		var winds, clouds, weather, temperature string
		if m := s.app.METARSource(); m != nil {
			winds = m.WindConditions()
			clouds = m.SkyCover()
			weather = m.WeatherConditions()
			temperature = m.TemperatureString()
		}

		u.Status = &Status{
			Winds:            winds,
			WindsColor:       "#ffffff",
			Clouds:           clouds,
			CloudsColor:      "#ffffff",
			Weather:          weather,
			WeatherColor:     "#ffffff",
			Separation:       separationString,
			SeparationColor:  separationColor,
			Temperature:      temperature,
			TemperatureColor: "#ffffff",
		}
	}

	const jumprunSources = core.JumprunDataSource
	if source&jumprunSources != 0 {
		j := s.app.Jumprun().Jumprun()
		u.Jumprun = &Jumprun{
			Origin: &JumprunOrigin{
				Latitude:          j.Latitude,
				Longitude:         j.Longitude,
				MagneticDeviation: int32(j.MagneticDeclination),
				CameraHeight:      int32(j.CameraHeight),
			},
		}
		if j.IsSet {
			p := &JumprunPath{
				Heading:        int32(j.Heading),
				ExitDistance:   int32(j.ExitDistance),
				OffsetHeading:  int32(j.OffsetHeading),
				OffsetDistance: int32(j.OffsetDistance),
			}
			for _, t := range j.HookTurns {
				if t.Distance == 0 && t.Heading == 0 {
					break
				}
				p.Turns = append(p.Turns, &JumprunTurn{
					Distance: int32(t.Distance),
					Heading:  int32(t.Heading),
				})
			}
			u.Jumprun.Path = p
		}
	}

	const windsAloftSources = core.WindsAloftDataSource
	if source&windsAloftSources != 0 {
		w := s.app.WindsAloftSource()
		u.WindsAloft = &WindsAloft{}
		for _, sample := range w.Samples() {
			u.WindsAloft.Samples = append(u.WindsAloft.Samples,
				&WindsAloftSample{
					Altitude:    int32(sample.Altitude),
					Heading:     int32(sample.Heading),
					Speed:       int32(sample.Speed),
					Temperature: int32(sample.Temperature),
					Variable:    sample.LightAndVariable,
				})
		}
	}

	const loadsSources = core.BurbleDataSource
	if source&loadsSources != 0 {
		b := s.app.BurbleSource()
		u.Loads = &Loads{
			ColumnCount: int32(b.ColumnCount()),
		}
		for _, l := range b.Loads() {
			var callMinutes string
			if !l.IsNoTime {
				if l.CallMinutes == 0 {
					callMinutes = "NOW"
				} else {
					callMinutes = strconv.FormatInt(l.CallMinutes, 10)
				}
			}

			var slotsAvailable string
			if l.CallMinutes <= 5 {
				slotsAvailable = fmt.Sprintf("%d aboard", l.SlotsAvailable)
			} else if l.SlotsAvailable == 1 {
				slotsAvailable = "1 slot"
			} else {
				slotsAvailable = fmt.Sprintf("%d slots", l.SlotsAvailable)
			}

			load := &Load{
				Id:                   uint64(l.ID),
				AircraftName:         l.AircraftName,
				LoadNumber:           l.LoadNumber,
				CallMinutes:          int32(l.CallMinutes),
				CallMinutesString:    callMinutes,
				SlotsAvailable:       int32(l.SlotsAvailable),
				SlotsAvailableString: slotsAvailable,
				IsFueling:            l.IsFueling,
				IsTurning:            l.IsTurning,
				IsNoTime:             l.IsNoTime,
			}
			for _, j := range l.Tandems {
				load.Slots = append(load.Slots, s.slotFromJumper(j))
			}
			for _, j := range l.Students {
				load.Slots = append(load.Slots, s.slotFromJumper(j))
			}
			for _, j := range l.SportJumpers {
				load.Slots = append(load.Slots, s.slotFromJumper(j))
			}

			u.Loads.Loads = append(u.Loads.Loads, load)
		}
	}

	return u
}

func (x *ManifestUpdate) diff(y *ManifestUpdate) bool {
	if proto.Equal(x.Status, y.Status) {
		x.Status = nil
	}
	if proto.Equal(x.Options, y.Options) {
		x.Options = nil
	}
	if proto.Equal(x.Jumprun, y.Jumprun) {
		x.Jumprun = nil
	}
	if proto.Equal(x.WindsAloft, y.WindsAloft) {
		x.WindsAloft = nil
	}
	if proto.Equal(x.Loads, y.Loads) {
		x.Loads = nil
	}
	return x.Status != nil || x.Options != nil || x.Jumprun != nil ||
		x.WindsAloft != nil || x.Loads != nil
}

func (s *manifestServiceServer) processUpdates(ctx context.Context) {
	c := make(chan core.DataSource, 128)
	id := s.app.AddListener(c)
	defer func() {
		s.app.RemoveListener(id)
	}()

	clientID := uint64(0)
	clients := make(map[uint64]chan *ManifestUpdate)

	// Create and send the initial baseline ManifestUpdate
	source := core.BurbleDataSource | core.OptionsDataSource
	if s.app.Jumprun() != nil {
		source |= core.JumprunDataSource
	}
	if s.app.METARSource() != nil {
		source |= core.METARDataSource
	}
	if s.app.WindsAloftSource() != nil {
		source |= core.WindsAloftDataSource
	}
	lastUpdate := s.constructUpdate(source)

	for {
		select {
		case <-ctx.Done():
			return

		case req := <-s.addClientChan:
			clientID++
			clients[clientID] = req.updates
			req.reply <- addClientResponse{
				id: clientID,
			}
			req.updates <- lastUpdate

		case req := <-s.removeClientChan:
			delete(clients, req.id)
			req.reply <- removeClientResponse{}

		case source = <-c:
		drain:
			for {
				select {
				case s := <-c:
					source |= s
				default:
					break drain
				}
			}
			if u := s.constructUpdate(source); u.diff(lastUpdate) {
				for _, client := range clients {
					client <- u
				}
				proto.Merge(lastUpdate, u)
			}
		}
	}
}

func (s *manifestServiceServer) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.processUpdates(ctx)
	}()
}

func (s *manifestServiceServer) Stop() {
	s.cancel()
	s.wg.Wait()
}

func (s *manifestServiceServer) addClient(c chan *ManifestUpdate) uint64 {
	request := addClientRequest{
		reply:   make(chan addClientResponse),
		updates: c,
	}
	response := <-request.reply
	return response.id
}

func (s *manifestServiceServer) removeClient(id uint64) {
	request := removeClientRequest{
		reply: make(chan removeClientResponse),
		id:    id,
	}
	<-request.reply
}

func (s *manifestServiceServer) StreamUpdates(
	_ *emptypb.Empty,
	stream ManifestService_StreamUpdatesServer,
) error {
	c := make(chan *ManifestUpdate, 16)
	id := s.addClient(c)
	defer s.removeClient(id)

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-s.app.Done():
			return nil
		case u := <-c:
			if err := stream.Send(u); err != nil {
				return err
			}
		}
	}
}
