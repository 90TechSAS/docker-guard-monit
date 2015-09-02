package core

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"time"

	dguard "github.com/90TechSAS/libgo-docker-guard"
)

var (
	// HTTP client used to get probe infos
	HTTPClient = &http.Client{}
	Probes     []*Probe
)

/*
	Probe
*/
type Probe struct {
	Name        string  `yaml:"name"`
	URI         string  `yaml:"uri"`
	APIPassword string  `yaml:"api-password"`
	ReloadTime  float64 `yaml:"reload-time"`
	Infos       *dguard.ProbeInfos
}

/*
	Initialize Core
*/
func Init() {
	// Init Containers Controller
	InitContainersController()

	// Init InfluxDB client
	InitDB()

	// Launch probe monitors
	for _, p := range DGConfig.Probes {
		var probe = Probe{
			Name:        p.Name,
			URI:         p.URI,
			APIPassword: p.APIPassword,
			ReloadTime:  p.ReloadTime,
			Infos:       new(dguard.ProbeInfos),
		}
		Probes = append(Probes, &probe)
		go MonitorProbe(probe)
	}

	// Launch API
	HTTPServer()
}

/*
	Loop for monitoring a probe
*/
func MonitorProbe(p Probe) {
	var resp *http.Response                        // Http response
	var req *http.Request                          // Http response
	var body []byte                                // Http body
	var err error                                  // Error handling
	var containers map[string]*dguard.Container    // Returned container list
	var oldContainers map[string]*dguard.Container // Old returned container list (used to compare running state)
	var dbContainers []Container                   // Containers in DB
	var tmpProbeInfos dguard.ProbeInfos            // Temporary probe infos

	// Reloading loop
	for {
		var statsToInsert []Stat // Stats to insert

		oldContainers = containers
		containers = nil
		l.Verbose("Reloading", p.Name)

		/*
			GET PROBE INFOS
		*/
		// Make HTTP GET request
		reqURI := p.URI + "/probeinfos"
		l.Debug("MonitorProbe: GET", reqURI)
		req, err = http.NewRequest("GET", reqURI, bytes.NewBufferString(""))
		if err != nil {
			l.Error("MonitorProbe ("+p.Name+"): Can't create", p.Name, "HTTP request:", err)
			time.Sleep(time.Second * time.Duration(p.ReloadTime))
			continue
		}
		req.Header.Set("Auth", p.APIPassword)

		// Do request
		l.Debug("MonitorProbe: Get probe infos")
		resp, err = HTTPClient.Do(req)
		if err != nil {
			l.Error("MonitorProbe ("+p.Name+"): Can't get", p.Name, "probe infos:", err)
			p.Infos.Running = false
			time.Sleep(time.Second * time.Duration(p.ReloadTime))
			continue
		}
		if resp.StatusCode != 200 {
			l.Error("MonitorProbe ("+p.Name+"): Probe returned a non 200 HTTP status code:", resp.StatusCode)
			p.Infos.Running = false
			time.Sleep(time.Second * time.Duration(p.ReloadTime))
			continue
		}

		// Get request body
		body, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			l.Error("MonitorProbe ("+p.Name+"): Can't get", p.Name, "probe infos body:", err)
			time.Sleep(time.Second * time.Duration(p.ReloadTime))
			continue
		}

		l.Silly("MonitorProbe ("+p.Name+"):", "GET", reqURI, "body:\n", string(body))

		// Parse body
		err = json.Unmarshal([]byte(body), &(tmpProbeInfos))
		if err != nil {
			l.Error("MonitorProbe ("+p.Name+"): Parsing probe infos:", err)
			time.Sleep(time.Second * time.Duration(p.ReloadTime))
			continue
		}
		tmpProbeInfos.Running = true
		tmpProbeInfos.Name = p.Name
		*(p.Infos) = tmpProbeInfos // Swap probe infos

		/*
			GET LIST OF CONTAINERS
		*/
		// Make HTTP GET request
		reqURI = p.URI + "/list"
		l.Debug("MonitorProbe: GET", reqURI)
		req, err = http.NewRequest("GET", reqURI, bytes.NewBufferString(""))
		if err != nil {
			l.Error("MonitorProbe ("+p.Name+"): Can't create", p.Name, "HTTP request:", err)
			time.Sleep(time.Second * time.Duration(p.ReloadTime))
			continue
		}
		req.Header.Set("Auth", p.APIPassword)

		// Do request
		l.Debug("MonitorProbe: Get list of containers")
		resp, err = HTTPClient.Do(req)
		if err != nil {
			l.Error("MonitorProbe ("+p.Name+"): Can't get", p.Name, "container list:", err)
			time.Sleep(time.Second * time.Duration(p.ReloadTime))
			continue
		}
		if resp.StatusCode != 200 {
			l.Error("MonitorProbe ("+p.Name+"): Probe returned a non 200 HTTP status code:", resp.StatusCode)
			time.Sleep(time.Second * time.Duration(p.ReloadTime))
			continue
		}

		// Get request body
		body, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			l.Error("MonitorProbe ("+p.Name+"): Can't get", p.Name, "container list body:", err)
			time.Sleep(time.Second * time.Duration(p.ReloadTime))
			continue
		}

		l.Silly("MonitorProbe ("+p.Name+"):", "GET", reqURI, "body:\n", string(body))

		// Parse body
		err = json.Unmarshal([]byte(body), &containers)
		if err != nil {
			l.Error("MonitorProbe ("+p.Name+"): Parsing container list:", err)
			time.Sleep(time.Second * time.Duration(p.ReloadTime))
			continue
		}

		// Remove in DB old removed containers
		l.Debug("MonitorProbe: GetContainersByProbe(", p.Name, ")")
		dbContainers, err = GetContainersByProbe(p.Name)
		if err != nil {
			if err.Error() != "Not found" {
				l.Error("MonitorProbe ("+p.Name+"): containers not found:", err)
				time.Sleep(time.Second * time.Duration(p.ReloadTime))
				continue
			}
		}
		for _, dbC := range dbContainers {
			var containerStillExist = false
			for _, c := range containers {
				if dbC.CID == c.ID {
					containerStillExist = true
					// Check if container started or stopped
					c1, ok1 := containers[dbC.CID]
					c2, ok2 := oldContainers[dbC.CID]
					if ok1 && ok2 && (c1.Running != c2.Running) {
						var event dguard.Event
						var eventSeverity int
						var eventType int
						if c1.Running {
							eventSeverity = dguard.EventNotice
							eventType = dguard.EventContainerStarted
						} else {
							eventSeverity = dguard.EventCritical
							eventType = dguard.EventContainerStopped
						}
						event = dguard.Event{
							Severity: eventSeverity,
							Type:     eventType,
							Target:   dbC.Hostname + " (" + dbC.CID + ")",
							Probe:    p.Name,
							Data:     ""}
						Alert(event)
					}
				}
			}
			if !containerStillExist {
				var event = dguard.Event{
					Severity: dguard.EventNotice,
					Type:     dguard.EventContainerRemoved,
					Target:   dbC.Hostname + " (" + dbC.CID + ")",
					Probe:    p.Name,
					Data:     ""}

				dbC.Delete()

				Alert(event)
			}
		}

		// Add containers and stats in DB
		for _, c := range containers {
			var id string
			var tmpContainer Container
			var newStat Stat

			// Add containers in DB
			tmpContainer, err = GetContainerByCID(c.ID)
			if err != nil {
				if err.Error() == "Not found" {
					var event dguard.Event

					newContainer := Container{c.ID, p.Name, c.Hostname, c.Image, c.IPAddress, c.MacAddress}

					event = dguard.Event{
						Severity: dguard.EventNotice,
						Type:     dguard.EventContainerCreated,
						Target:   newContainer.Hostname + " (" + newContainer.CID + ")",
						Probe:    p.Name,
						Data:     "Image: " + newContainer.Image}
					err = newContainer.Insert()

					Alert(event)
					if err != nil {
						l.Error("MonitorProbe ("+p.Name+"): container insert:", err)
						continue
					}
					id = newContainer.CID
				} else {
					l.Error("MonitorProbe ("+p.Name+"): GetContainerById:", err)
					continue
				}
			} else {
				id = tmpContainer.CID
			}

			newStat = Stat{id,
				time.Unix(int64(c.Time), 0),
				uint64(c.SizeRootFs),
				uint64(c.SizeRw),
				uint64(c.MemoryUsed),
				uint64(c.NetBandwithRX),
				uint64(c.NetBandwithTX),
				uint64(c.CPUUsage),
				c.Running}

			statsToInsert = append(statsToInsert, newStat)
		}
		err = InsertStats(statsToInsert)
		if err != nil {
			l.Error("MonitorProbe ("+p.Name+"): insert stats:", err)
			continue
		}

		time.Sleep(time.Second * time.Duration(p.ReloadTime))
	}
}
