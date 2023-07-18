// (c) Copyright 2017-2021 Matt Messier

package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/jumptown-skydiving/manifest-server/pkg/burble"
	"github.com/jumptown-skydiving/manifest-server/pkg/core"
	"github.com/jumptown-skydiving/manifest-server/pkg/db"
	"github.com/jumptown-skydiving/manifest-server/pkg/settings"
	"github.com/orangematt/siwa"

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

func (s *manifestServiceServer) translateJumper(j *burble.Jumper, leader *Jumper, load *burble.Load) *Jumper {
	var (
		color  uint32
		prefix string
	)
	shortName := j.ShortName
	if leader != nil && (j.IsInstructor || j.IsVideographer) {
		color = leader.Color
	} else {
		switch {
		case j.IsTandem:
			color = 0xffff00 // yellow
			if leader == nil {
				prefix = "Tandem"
				shortName = ""
			}
		case j.IsStudent || strings.HasSuffix(j.ShortName, " + Gear"):
			color = 0x00ff00 // green
			if strings.HasSuffix(j.ShortName, " H/P") {
				prefix = "H&P"
			}
		case strings.HasPrefix(j.ShortName, "3-5k") || strings.HasPrefix(j.ShortName, "3.5k"):
			if j.IsPondSwoop {
				color = 0x00ffff // cyan
			} else {
				color = 0xff00ff // magenta
			}
			prefix = "H&P"
		case j.IsPondSwoop:
			color = 0x00ffff // cyan
		default:
			color = 0xffffff // white
		}
	}

	var repr string
	if rigName := j.RigName; rigName != "" {
		shortName = fmt.Sprintf("%s / %s", rigName, shortName)
	}
	if shortName != "" {
		shortName = " (" + shortName + ")"
	}
	if prefix != "" {
		repr = fmt.Sprintf("%s: %s%s", prefix, j.Name, shortName)
	} else {
		repr = fmt.Sprintf("%s%s", j.Name, shortName)
	}
	if j.IsPondSwoop {
		repr = "🏄" + repr
	}
	if j.IsTurning && load.IsTurning {
		repr = "♻️ " + repr
	}
	if leader != nil {
		repr = "\t" + repr
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
		Color:     color,
		Repr:      repr,
		RigName:   j.RigName,
	}
}

func (s *manifestServiceServer) slotFromJumper(j *burble.Jumper, load *burble.Load) *LoadSlot {
	if len(j.GroupMembers) == 0 {
		return &LoadSlot{
			Slot: &LoadSlot_Jumper{
				Jumper: s.translateJumper(j, nil, load),
			},
		}
	}

	g := &JumperGroup{
		Leader: s.translateJumper(j, nil, load),
	}
	for _, member := range j.GroupMembers {
		g.Members = append(g.Members, s.translateJumper(member, g.Leader, load))
	}

	return &LoadSlot{
		Slot: &LoadSlot_Group{
			Group: g,
		},
	}
}

func (s *manifestServiceServer) constructUpdate(source core.DataSource) *ManifestUpdate {
	u := &ManifestUpdate{}

	const sunriseSources = core.PreSunriseDataSource | core.SunriseDataSource
	const sunsetSources = core.PreSunsetDataSource | core.SunsetDataSource
	const optionsSources = core.OptionsDataSource | sunriseSources | sunsetSources
	if source&optionsSources != 0 {
		s.options = s.app.Settings().Options()
		o := s.options
		u.Options = &Options{
			DisplayWeather: o.DisplayWeather,
			DisplayWinds:   o.DisplayWinds,
			Message:        o.Message,
			MessageColor:   0xffffff,
			FuelRequested:  o.FuelRequested,
		}
		if source&sunriseSources != 0 {
			u.Options.Sunrise = s.app.SunriseMessage()
		}
		if source&sunsetSources != 0 {
			u.Options.Sunset = s.app.SunsetMessage()
		}
	}

	const statusSources = core.METARDataSource | core.WindsAloftDataSource
	if source&statusSources != 0 {
		var (
			separationColor  uint32
			separationString string
		)
		if s.app.WindsAloftSource() != nil {
			separationColor, separationString = s.app.SeparationStrings()
		} else {
			separationColor = 0xffffff
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
			WindsColor:       0xffffff,
			Clouds:           clouds,
			CloudsColor:      0xffffff,
			Weather:          weather,
			WeatherColor:     0xffffff,
			Separation:       separationString,
			SeparationColor:  separationColor,
			Temperature:      temperature,
			TemperatureColor: 0xffffff,
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
			if len(j.Offsets) > 0 {
				u.Jumprun.Offsets = make([]int32, len(j.Offsets))
				for x, offset := range j.Offsets {
					u.Jumprun.Offsets[x] = int32(offset)
				}
			}
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

	const loadsSources = core.BurbleDataSource | core.OptionsDataSource
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

			load := &Load{
				Id:                uint64(l.ID),
				AircraftName:      l.AircraftName,
				LoadNumber:        l.LoadNumber,
				CallMinutes:       int32(l.CallMinutes),
				CallMinutesString: callMinutes,
				SlotsAvailable:    int32(l.SlotsAvailable),
				IsFueling:         l.IsFueling,
				IsTurning:         l.IsTurning,
				IsNoTime:          l.IsNoTime,
			}
			for _, j := range l.Tandems {
				load.Slots = append(load.Slots, s.slotFromJumper(j, l))
			}
			for _, j := range l.Students {
				load.Slots = append(load.Slots, s.slotFromJumper(j, l))
			}
			for _, j := range l.SportJumpers {
				load.Slots = append(load.Slots, s.slotFromJumper(j, l))
			}

			var slotsAvailable string
			if l.CallMinutes <= 5 {
				// Burble doesn't give us unique Jumper IDs in
				// the loads even though it surely tracks them
				// internally. So we have to do the next best
				// thing and just count unique names. This
				// should generally work out fine since mostly
				// duplicate names really only come up when
				// there is one coach with multiple hop/pop
				// students
				names := make(map[string]struct{})
				for _, slot := range load.Slots {
					if j := slot.GetJumper(); j != nil {
						names[j.Name] = struct{}{}
					} else if g := slot.GetGroup(); g != nil {
						names[g.Leader.Name] = struct{}{}
						for _, member := range g.GetMembers() {
							names[member.Name] = struct{}{}
						}
					}
				}
				slotsAvailable = fmt.Sprintf("%d aboard", len(names))
			} else if l.SlotsAvailable == 1 {
				slotsAvailable = "1 slot"
			} else {
				slotsAvailable = fmt.Sprintf("%d slots", l.SlotsAvailable)
			}
			load.SlotsAvailableString = slotsAvailable

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
			update := proto.Clone(lastUpdate).(*ManifestUpdate)
			req.updates <- update

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
					update := proto.Clone(u).(*ManifestUpdate)
					client <- update
				}
				// We cannot use proto.Merge here because we
				// attribute meaning to nil on optional fields,
				// but proto.Merge ignores nil when merging in,
				// not clearing the field in the destination.
				// This is what we want at the top-level, but
				// not the lower levels.
				//proto.Merge(lastUpdate, u)
				if u.Status != nil {
					lastUpdate.Status = u.Status
				}
				if u.Options != nil {
					lastUpdate.Options = u.Options
				}
				if u.Jumprun != nil {
					lastUpdate.Jumprun = u.Jumprun
				}
				if u.WindsAloft != nil {
					lastUpdate.WindsAloft = u.WindsAloft
				}
				if u.Loads != nil {
					lastUpdate.Loads = u.Loads
				}
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
	s.addClientChan <- request
	response := <-request.reply
	return response.id
}

func (s *manifestServiceServer) removeClient(id uint64) {
	request := removeClientRequest{
		reply: make(chan removeClientResponse),
		id:    id,
	}
	s.removeClientChan <- request
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

func (s *manifestServiceServer) SignInWithApple(
	ctx context.Context,
	req *SignInWithAppleRequest,
) (*SignInResponse, error) {
	m := s.app.SignInWithAppleManager()
	if m == nil {
		return &SignInResponse{
			ErrorMessage: "Server is not configured to support Sign In With Apple",
		}, nil
	}

	id, err := m.VerifyIdentityToken(ctx, req.IdentityToken, req.Nonce)
	if err != nil {
		return &SignInResponse{
			ErrorMessage: fmt.Sprintf("VerifyIdentityToken: %v", err),
		}, nil
	}

	tx, err := s.app.BeginDatabaseTransaction()
	if err != nil {
		return &SignInResponse{
			ErrorMessage: fmt.Sprintf("BeginDatabaseTransaction: %v", err),
		}, nil
	}

	r, err := m.ValidateAuthCode(ctx, req.Nonce, req.AuthorizationCode, "")
	if err != nil {
		_ = s.app.AbortDatabaseTransaction(tx)
		return &SignInResponse{
			ErrorMessage: fmt.Sprintf("ValidateAuthCode: %v", err),
		}, nil
	}

	user, err := s.app.CreateUser(tx, id.Subject, req.GivenName,
		req.FamilyName, id.Email, id.IsPrivateEmail, id.EmailVerified)
	if err != nil {
		_ = s.app.AbortDatabaseTransaction(tx)
		return &SignInResponse{
			ErrorMessage: fmt.Sprintf("CreateUser: %v", err),
		}, nil
	}

	session, err := s.app.NewSession(tx, user, r.AccessToken,
		r.RefreshToken, r.IdentityToken, req.Nonce, "siwa")
	if err != nil {
		_ = s.app.AbortDatabaseTransaction(tx)
		return &SignInResponse{
			ErrorMessage: fmt.Sprintf("NewSession: %v", err),
		}, nil
	}

	roles, err := s.app.QueryRoles(tx, user)
	if err != nil {
		_ = s.app.AbortDatabaseTransaction(tx)
		return &SignInResponse{
			ErrorMessage: fmt.Sprintf("QueryRoles: %v", err),
		}, nil
	}

	if err = s.app.CommitDatabaseTransaction(tx); err != nil {
		return &SignInResponse{
			ErrorMessage: fmt.Sprintf("CommitDatabaseTransaction: %v", err),
		}, nil
	}

	return &SignInResponse{
		SessionId:         session.ID,
		SessionExpiration: session.ExpireTime.Unix(),
		IsValid:           true,
		Roles:             roles,
	}, nil
}

func (s *manifestServiceServer) SignOut(
	ctx context.Context,
	req *SignOutRequest,
) (*SignOutResponse, error) {
	tx, err := s.app.BeginDatabaseTransaction()
	if err != nil {
		return &SignOutResponse{}, nil
	}

	if err = s.app.DeleteSession(ctx, tx, req.SessionId); err != nil {
		s.app.AbortDatabaseTransaction(tx)
		return &SignOutResponse{}, nil
	}

	if err = s.app.CommitDatabaseTransaction(tx); err != nil {
		return &SignOutResponse{}, nil
	}

	return &SignOutResponse{
		SessionId: req.SessionId,
	}, nil
}

func (s *manifestServiceServer) VerifySessionID(
	ctx context.Context,
	req *VerifySessionRequest,
) (*SignInResponse, error) {
	tx, err := s.app.BeginDatabaseTransaction()
	if err != nil {
		return &SignInResponse{
			ErrorMessage: fmt.Sprintf("BeginDatabaseTransaction: %v", err),
		}, nil
	}

	session, err := s.app.LookupSession(ctx, tx, req.SessionId)
	if err != nil {
		_ = s.app.AbortDatabaseTransaction(tx)
		sessionDeleted := false
		if errors.Is(err, db.ErrInvalidSessionID) {
			sessionDeleted = true
		} else if _, ok := err.(siwa.ErrorResponse); ok {
			sessionDeleted = true
		}
		return &SignInResponse{
			ErrorMessage:   fmt.Sprintf("LookupSession: %v", err),
			SessionDeleted: sessionDeleted,
		}, nil
	}

	user, err := s.app.LookupUser(tx, session.UserID)
	if err != nil {
		_ = s.app.AbortDatabaseTransaction(tx)
		return &SignInResponse{
			ErrorMessage: fmt.Sprintf("LookupUser: %v", err),
		}, nil
	}

	roles, err := s.app.QueryRoles(tx, user)
	if err != nil {
		_ = s.app.AbortDatabaseTransaction(tx)
		return &SignInResponse{
			ErrorMessage: fmt.Sprintf("QueryRoles: %v", err),
		}, nil
	}

	if err = s.app.CommitDatabaseTransaction(tx); err != nil {
		return &SignInResponse{
			ErrorMessage: fmt.Sprintf("CommitDatabaseTransaction: %v", err),
		}, nil
	}

	return &SignInResponse{
		SessionId:         session.ID,
		SessionExpiration: session.ExpireTime.Unix(),
		IsValid:           true,
		Roles:             roles,
	}, nil
}

func (s *manifestServiceServer) ToggleFuelRequested(
	ctx context.Context,
	req *ToggleFuelRequestedRequest,
) (*ToggleFuelRequestedResponse, error) {
	vreq := VerifySessionRequest{
		SessionId: req.SessionId,
	}
	vresp, err := s.VerifySessionID(ctx, &vreq)
	if err != nil {
		return nil, err
	}

	ok := false
	for _, role := range vresp.Roles {
		if role == "admin" || role == "pilot" {
			ok = true
			break
		}
	}
	if !ok {
		return &ToggleFuelRequestedResponse{
			ErrorMessage: "Permission Denied",
		}, nil
	}

	settings := s.app.Settings()
	settings.SetFuelRequested(!settings.FuelRequested())
	if err := settings.Write(); err != nil {
		errorMessage := fmt.Sprintf("Unable to save settings: %v", err)
		fmt.Fprintf(os.Stderr, "%s\n", errorMessage)
		return &ToggleFuelRequestedResponse{
			ErrorMessage: errorMessage,
		}, nil
	} else {
		s.app.WakeListeners(core.OptionsDataSource)
		return &ToggleFuelRequestedResponse{}, nil
	}
}

func (s *manifestServiceServer) RestartServer(
	ctx context.Context,
	req *RestartServerRequest,
) (*RestartServerResponse, error) {
	vreq := VerifySessionRequest{
		SessionId: req.SessionId,
	}
	vresp, err := s.VerifySessionID(ctx, &vreq)
	if err != nil {
		return nil, err
	}

	for _, role := range vresp.Roles {
		if role == "admin" {
			syscall.Kill(os.Getpid(), syscall.SIGTERM)
			return &RestartServerResponse{}, nil
		}
	}

	return &RestartServerResponse{
		ErrorMessage: "Permission Denied",
	}, nil
}
