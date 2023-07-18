// (c) Copyright 2017-2021 Matt Messier

package jumprun

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jumptown-skydiving/manifest-server/pkg/settings"
)

type UpdateFunc func()

type Controller struct {
	settings      *settings.Settings
	stateFilename string
	update        UpdateFunc

	lock     sync.Mutex
	jumprun  Jumprun
	template *template.Template
}

func NewController(
	settings *settings.Settings,
	update UpdateFunc,
) *Controller {
	c := &Controller{
		settings:      settings,
		stateFilename: settings.JumprunStateFile(),
		update:        update,
	}
	if err := c.restore(); err != nil {
		fmt.Fprintf(os.Stderr, "cannot restore jumprun state: %v\n", err)
		c.jumprun = Jumprun{
			TimeStamp:           time.Now().Unix(),
			Latitude:            settings.JumprunLatitude(),
			Longitude:           settings.JumprunLongitude(),
			MagneticDeclination: settings.JumprunMagneticDeclination(),
			CameraHeight:        settings.JumprunCameraHeight(),
		}
	}
	return c
}

func (c *Controller) Jumprun() Jumprun {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.jumprun
}

func (c *Controller) Reset() {
	c.lock.Lock()
	c.jumprun.TimeStamp = time.Now().Unix()
	c.jumprun.IsSet = false
	c.lock.Unlock()

	c.updateStaticData()
}

func (c *Controller) SetFromURLValues(values url.Values) error {
	var (
		err error
		v   int
	)

	c.lock.Lock()
	latitude := c.jumprun.Latitude
	longitude := c.jumprun.Longitude
	c.lock.Unlock()

	newj := Jumprun{
		TimeStamp: time.Now().Unix(),
		IsSet:     true,
	}
	if v, err = newj.getIntValue(values, "main_heading", 0); err != nil {
		return err
	}
	if v < 0 || v > 359 {
		return fmt.Errorf("main heading out of range: %d", v)
	}
	newj.Heading = v

	if v, err = newj.getIntValue(values, "exit_distance", 0); err != nil {
		return err
	}
	newj.ExitDistance = v

	if v, err = newj.getIntValue(values, "offset_heading", 0); err != nil {
		return err
	}
	newj.OffsetHeading = v

	if v, err = newj.getIntValue(values, "offset_distance", 0); err != nil {
		return err
	}
	newj.OffsetDistance = v

	if v, err = newj.getIntValue(values, "magnetic_declination", 0); err != nil {
		return err
	}
	newj.MagneticDeclination = v

	if v, err = newj.getIntValue(values, "camera_height", 0); err != nil {
		return err
	}
	newj.CameraHeight = v

	if latitude, err = newj.getCoordinate(values, "latitude", latitude); err != nil {
		return err
	}
	newj.Latitude = latitude

	if longitude, err = newj.getCoordinate(values, "longitude", longitude); err != nil {
		return err
	}
	newj.Longitude = longitude

	for i := 0; i < len(newj.HookTurns); i++ {
		var turn Turn
		key := fmt.Sprintf("hook_heading_%d", i)
		value := values.Get(key)
		if value == "" {
			break
		}

		var v64 int64
		if v64, err = strconv.ParseInt(value, 10, 32); err != nil {
			return fmt.Errorf("cannot parse hook heading %d: %v", i, err)
		}
		if v64 < 0 || v64 > 359 {
			return fmt.Errorf("hook heading %d out of range: %d", i, v64)
		}
		turn.Heading = int(v64)

		key = fmt.Sprintf("hook_distance_%d", i)
		if v64, err = strconv.ParseInt(values.Get(key), 10, 32); err != nil {
			return fmt.Errorf("cannot parse hook distance %d: %v", i, err)
		}
		turn.Distance = int(v64)
		newj.HookTurns[i] = turn
	}

	x := 0
	for i := 0; i < len(newj.Offsets); i++ {
		key := fmt.Sprintf("parallel_offset_%d", i)
		value := values.Get(key)
		if value == "" {
			break
		}

		var v64 int64
		if v64, err = strconv.ParseInt(value, 10, 32); err != nil {
			return fmt.Errorf("cannot parse parallel offset %d: %v", i, err)
		}
		if v64 == 0 {
			continue
		}

		dupe := false
		for j := 0; j < x; j++ {
			if newj.Offsets[j] == int(v64) {
				dupe = true
				break
			}
		}
		if dupe {
			continue
		}
		newj.Offsets[x] = int(v64)
		x++
	}

	c.lock.Lock()
	defer c.lock.Unlock()
	c.jumprun = newj
	c.updateStaticData()

	return nil
}

func (c *Controller) updateStaticData() {
	if c.update != nil {
		c.update()
	}
}

func (c *Controller) restore() error {
	dataBytes, err := ioutil.ReadFile(c.stateFilename)
	if err != nil {
		return err
	}

	var newj Jumprun
	if err = json.Unmarshal(dataBytes, &newj); err != nil {
		return err
	}

	c.lock.Lock()
	c.jumprun = newj
	c.lock.Unlock()

	c.updateStaticData()
	return nil
}

func (c *Controller) Write() error {
	c.lock.Lock()
	j := c.jumprun
	c.lock.Unlock()

	dataBytes, err := json.Marshal(&j)
	if err != nil {
		return err
	}

	tempFilename := c.stateFilename + ".tmp"
	if err = ioutil.WriteFile(tempFilename, dataBytes, 0600); err == nil {
		_ = os.Rename(tempFilename, c.stateFilename)
	}
	return err
}

func (c *Controller) initializeTemplate() *template.Template {
	if c.template == nil {
		t := template.New("jumprun")

		// These functions are used to reset the origin back to the
		// configured defaults
		t = t.Funcs(template.FuncMap{
			"latitude":             func() string { return c.settings.JumprunLatitude() },
			"longitude":            func() string { return c.settings.JumprunLongitude() },
			"magnetic_declination": func() int { return c.settings.JumprunMagneticDeclination() },
			"camera_height":        func() int { return c.settings.JumprunCameraHeight() },
		})

		var err error
		c.template, err = t.Parse(jumprunHTML)
		if err != nil {
			// This should never fail -- the HTML is hard-coded, so
			// panic if this happens, because it means it's an error
			// that will never not happen.
			panic(err)
		}
	}
	return c.template
}

func (c *Controller) HTML(w http.ResponseWriter, req *http.Request) {
	c.lock.Lock()
	j := c.jumprun
	tmpl := c.initializeTemplate()
	c.lock.Unlock()

	if tmpl == nil {
		http.NotFound(w, req)
		return
	}

	b := &bytes.Buffer{}
	if err := tmpl.Execute(b, &j); err != nil {
		http.NotFound(w, req)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	r := bytes.NewReader(b.Bytes())
	http.ServeContent(w, req, "", time.Now(), r)
}

func (c *Controller) FormHandler(w http.ResponseWriter, req *http.Request) {
	contentType := req.Header.Get("content-type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := req.ParseMultipartForm(32 << 20); err != nil {
			http.NotFound(w, req)
			return
		}
		req.Form = url.Values{}
		for key, values := range req.MultipartForm.Value {
			for _, value := range values {
				req.Form.Add(key, value)
			}
		}
	} else {
		if err := req.ParseForm(); err != nil {
			http.NotFound(w, req)
			return
		}
	}
	if err := c.SetFromURLValues(req.Form); err == nil {
		_ = c.Write()
	}
}

const jumprunHTML = `<html>
	<head>
		<title>Manifest - Set Jump Run</title>
		<script>
		function reset_origin() {
			document.getElementById("latitude").value = "{{latitude}}";
			document.getElementById("longitude").value = "{{longitude}}";
			document.getElementById("magnetic_declination").value = "{{magnetic_declination}}";
			document.getElementById("camera_height").value = "{{camera_height}}";
		}
		</script>
	</head>
	<body>
		<form action="/setjumprun" id="jumprun" method="post">
			<div>
				All headings are relative to magentic north. All distances are
				specified in tenths of a mile (e.g. 1 is 1/10 mile, 5 is
				1/2 mile, 10 is 1 mile, etc.).  The exit distance is the offset
				from center where the pilot will turn on the green light. A
				negative value means that the exit point is before center. A
				positive value means that the exit point is after center.
			</div>
			<div>
				<hr>
				<h3>Jump Run</h3>
			</div>
			<div>
				<h4>Origin:</h4>
				<div>
					<label>Latitude:</label>
					<input type="text" id="latitude" name="latitude" value="{{.Latitude}}">
					<label>Longitude:</label>
					<input type="text" id="longitude" name="longitude" value="{{.Longitude}}">
				</div>
				<div>
					<label>Magnetic Declination:</label>
					<input type="text" id="magnetic_declination" name="magnetic_declination" value="{{.MagneticDeclination}}">
				</div>
				<div>
					<label>Camera Height (feet):</label>
					<input type="text" id="camera_height" name="camera_height" value="{{.CameraHeight}}">
				</div>
				<div>
					<input type="button" value="Reset to Default" onclick="reset_origin();">
				</div>
			</div>
			<div>
				<h4>Run:</h4>
				<div>
					<label>Heading:</label>
					<input type="text" name="main_heading" value="{{.Heading}}">
				</div>
				<div>
					<label>Exit Distance:<label>
					<input type="text" name="exit_distance" value="{{.ExitDistance}}">
				</div>
			</div>
			<div>
				<h4>Offset:</h4>
				<div>
					<label>Heading:</label>
					<input type="text" name="offset_heading" value="{{.OffsetHeading}}">
					<label>Distance:</label>
					<input type="text" name="offset_distance" value="{{.OffsetDistance}}">
				</div>
			</div>
			<div>
				<h4>Hook</h4>
				Leave these blank if there is to be no hook.
				<p>
				{{range $index, $element := .HookTurns}}
				<div>
					<label>Distance:<label>
					<input type="text" name="hook_distance_{{$index}}" value="{{$element.Distance}}">
					<label>Heading:<label>
					<input type="text" name="hook_heading_{{$index}}" value="{{$element.Heading}}">
				</div>
				{{end}}
			</div>
			<div>
				<h4>Parallel Runs</h4>
				Leave these blank if there are to be no additional parallel runs.
				<p>
				{{range $index, $element := .Offsets}}
				<div>
					<label>Distance:<label>
					<input type="text" name="parallel_offset_{{$index}}" value="{{$element}}">
				</div>
				{{end}}
			</div>
			<div>
				<hr>
				<button type="reset">Reset</button>
				<button type="submit">Submit</button>
			</div>
		</form>
		<script>
		var form = document.getElementById("jumprun");
		form.addEventListener("submit", function (e) {
			var params = {
				method: "post",
				body: new FormData(form),
			};
			window.fetch(form.action, params).then(function (response) {
				console.log(response.text());
			});
			e.preventDefault();
		});
		</script>
	</body>
</html>
`
