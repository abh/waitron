package main

// @APITitle Waitron
// @APIDescription Templates for server provisioning
// @License BSD
// @LicenseUrl http://opensource.org/licenses/BSD-2-Clause
import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"sync"
	"time"

	"github.com/gorilla/handlers"
	"github.com/julienschmidt/httprouter"
)

type result struct {
	Token string `json:",omitempty"`
	Error string `json:",omitempty"`
	State string `json:",omitempty"`
}

type HttpResponse struct {
	Message    string
	StatusCode int
}

// @Title templateHandler
// @Description Render either the finish or the preseed template
// @Param hostname    path    string    true    "Hostname"
// @Param template    path    string    true    "The template to be rendered"
// @Param token        path    string    true    "Token"
// @Success 200    {object} string "Rendered template"
// @Failure 400    {object} string "Not in build mode or definition does not exist"
// @Failure 400    {object} string "Unable to render template"
// @Failure 401    {object} string "Invalid token"
// @Router /template/{template}/{hostname}/{token} [GET]
func templateHandler(response http.ResponseWriter, request *http.Request, ps httprouter.Params, config Config, state State) {

	hostname := ps.ByName("hostname")

	if ps.ByName("token") != state.Tokens[hostname] {
		http.Error(response, "Invalid Token", 401)
		log.Println(ps.ByName("token"))
		return
	}

	// Get machine
	state.Mux.Lock()
	m, found := state.MachineByUUID[ps.ByName("token")]
	state.Mux.Unlock()

	if !found {
		http.Error(response, "Not in build mode or definition does not exist", 400)
		log.Println(m)
		return
	}

	// Render preseed as default
	var template string

	switch ps.ByName("template") {
	case "preseed":
		template = path.Join(config.TemplatePath, m.Preseed)

		hookType := "pre-hook"
		err := executeHooks(hookType, m, config)
		if err != nil {
			log.Println(err)
			http.Error(response, fmt.Sprintf("Cannot execute pre hooks"), 500)
			return
		}

	case "finish":
		template = path.Join(config.TemplatePath, m.Finish)
	case "cloud-init":
		template = path.Join(config.MachinePath, hostname+".cloud-init")
	}

	renderedTemplate, err := m.renderTemplate(template, config)
	if err != nil {
		log.Println(err)
		http.Error(response, "Unable to render template", http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(response, renderedTemplate)
}

// @Title hostConfigHandler
// @Description Renders the host configuration
// @Param hostname  path  string  true  "Hostname"
// @Success 200 {object} string "Rendered template"
// @Failure 400 {object} string "Unable to find host definition for hostname"
// @Router /config/{hostname} [GET]
func hostConfigHandler(response http.ResponseWriter, request *http.Request,
	ps httprouter.Params,
	config Config) {

	hostname := ps.ByName("hostname")

	m, err := machineDefinition(hostname, config.MachinePath, config)
	if err != nil {
		log.Println(err)
		http.Error(response, "", http.StatusNotFound)
		return
	}

	response.Header().Set("content-type", "application/json")
	result, _ := json.Marshal(m)
	response.Write(result)
}

// @Title hostConfigVmHandler
// @Description Renders the host configuration
// @Param hostname  path  string  true  "Hostname"
// @Success 200 {object} string "Config"
// @Failure 400 {object} string "Unable to find vm definition for hostname"
// @Router /config/{hostname}/vm [GET]
func hostConfigVmHandler(response http.ResponseWriter, request *http.Request,
	ps httprouter.Params,
	config Config) {

	hostname := ps.ByName("hostname")

	m, err := vmDefinition(hostname, config.VmPath)
	if err != nil {
		log.Println(err)
		http.Error(response, "", http.StatusNotFound)
		return
	}

	response.Header().Set("content-type", "application/json")
	result, _ := json.Marshal(m)
	response.Write(result)
}

// @Title buildHandler
// @Description Put the server in build mode
// @Param hostname    path    string    true    "Hostname"
// @Success 200    {object} string "{"State": "OK", "Token": <UUID of the build>}"
// @Failure 500    {object} string "Unable to find host definition for hostname"
// @Failure 500    {object} string "Failed to set build mode on hostname"
// @Router build/{hostname} [PUT]
func buildHandler(response http.ResponseWriter, request *http.Request,
	ps httprouter.Params, config Config, state State) {
	hostname := ps.ByName("hostname")

	m, err := machineDefinition(hostname, config.MachinePath, config)
	if err != nil {
		log.Println(err)
		http.Error(response, fmt.Sprintf("Unable to find host definition for %s", hostname), http.StatusNotFound)
		return
	}

	token, err := m.setBuildMode(config, state)
	if err != nil {
		log.Println(err)
		http.Error(response, fmt.Sprintf("Failed to set build mode on %s", hostname), http.StatusInternalServerError)
		return
	}

	result, _ := json.Marshal(&result{State: "OK", Token: token})

	fmt.Fprintf(response, string(result))
}

// @Title rescueHandler
// @Description Put the server in build mode for a rescue boot
// @Param hostname    path    string    true    "Hostname"
// @Success 200    {object} string "{"State": "OK", "Token": <UUID of the build>}"
// @Failure 500    {object} string "Unable to find host definition for hostname"
// @Failure 500    {object} string "Failed to set build mode for rescue on hostname"
// @Router rescue/{hostname} [PUT]
func rescueHandler(response http.ResponseWriter, request *http.Request,
	ps httprouter.Params, config Config, state State) {
	hostname := ps.ByName("hostname")

	m, err := machineDefinition(hostname, config.MachinePath, config)
	if err != nil {
		log.Println(err)
		http.Error(response, fmt.Sprintf("Unable to find host definition for %s", hostname), 500)
		return
	}

	m.RescueMode = true

	token, err := m.setBuildMode(config, state)
	if err != nil {
		log.Println(err)
		http.Error(response, fmt.Sprintf("Failed to set build mode for rescue on %s", hostname), 500)
		return
	}

	result, _ := json.Marshal(&result{State: "OK", Token: token})

	fmt.Fprintf(response, string(result))
}

// @Title doneHandler
// @Description Remove the server from build mode
// @Param hostname    path    string    true    "Hostname"
// @Param token        path    string    true    "Token"
// @Success 200    {object} string "{"State": "OK"}"
// @Failure 500    {object} string "Failed to finish build mode"
// @Failure 400    {object} string "Not in build mode or definition does not exist"
// @Failure 401    {object} string "Invalid token"
// @Router /done/{hostname}/{token} [GET]
func doneHandler(response http.ResponseWriter, request *http.Request,
	ps httprouter.Params, config Config, state State) {
	hostname := ps.ByName("hostname")

	if ps.ByName("token") != state.Tokens[hostname] {
		http.Error(response, "Invalid Token", 401)
		return
	}

	// Get machine
	state.Mux.Lock()
	m, found := state.MachineByUUID[ps.ByName("token")]
	state.Mux.Unlock()

	if !found {
		http.Error(response, "Not in build mode or definition does not exist", 400)
		return
	}

	err := m.doneBuildMode(config, state)
	if err != nil {
		log.Println(err)
		http.Error(response, "Failed to finish build mode", 500)
		return
	}

	result, _ := json.Marshal(&result{State: "OK"})

	fmt.Fprintf(response, string(result))
}

// @Title cancelHandler
// @Description Remove the server from build mode
// @Param hostname    path    string    true    "Hostname"
// @Param token        path    string    true    "Token"
// @Success 200    {object} string "{"State": "OK"}"
// @Failure 500    {object} string "Failed to cancel build mode"
// @Failure 400    {object} string "Not in build mode or definition does not exist"
// @Failure 401    {object} string "Invalid token"
// @Router /cancel/{hostname}/{token} [GET]
func cancelHandler(response http.ResponseWriter, request *http.Request,
	ps httprouter.Params, config Config, state State) {
	hostname := ps.ByName("hostname")

	if ps.ByName("token") != state.Tokens[hostname] {
		http.Error(response, "Invalid Token", 401)
		return
	}

	// Get machine
	state.Mux.Lock()
	m, found := state.MachineByUUID[ps.ByName("token")]
	state.Mux.Unlock()

	if !found {
		http.Error(response, "Not in build mode or definition does not exist", 400)
		return
	}

	err := m.cancelBuildMode(config, state)
	if err != nil {
		log.Println(err)
		http.Error(response, "Failed to cancel build mode", 500)
		return
	}

	hookType := "post-hook"
	err = executeHooks(hookType, m, config)
	if err != nil {
		log.Println(err)
		http.Error(response, fmt.Sprintf("Cannot execute post hooks"), 500)
		return
	}

	response.Header().Set("content-type", "application/json")
	result, _ := json.Marshal(&result{State: "OK"})
	fmt.Fprintf(response, string(result))
}

// @Title hostStatus
// @Description Build status of the server
// @Param hostname    path    string    true    "Hostname"
// @Success 200    {object} string "The status: (installing or installed)"
// @Failure 500    {object} string "Unknown state"
// @Router /status/{hostname} [GET]
func hostStatus(response http.ResponseWriter, request *http.Request,
	ps httprouter.Params, config Config, state State) {
	m, found := state.MachineByHostname[ps.ByName("hostname")]
	if !found || m.Status == "" {
		http.Error(response, "Unknown state", 500)
		return
	}
	fmt.Fprintf(response, m.Status)
}

// @Title listMachinesHandler
// @Description List machines handled by waitron
// @Success 200    {array} string "List of machines"
// @Failure 500    {object} string "Unable to list machines"
// @Router /list [GET]
func listMachinesHandler(response http.ResponseWriter, request *http.Request,
	_ httprouter.Params, config Config, state State) {
	machines, err := config.listMachines()
	if err != nil {
		log.Println(err)
		http.Error(response, "Unable to list machines", 500)
		return
	}
	js, _ := json.Marshal(machines)
	response.Header().Set("content-type", "application/json")
	response.Write(js)
}

// @Title listHooksHandler
// @Description List all available pre- and post hooks
// @Success 200 {array} string "List of hooks"
// @Failure 500 {object} string "Unable to list hooks"
// @Router /hooks [GET]
func listHooksHandler(response http.ResponseWriter, request *http.Request,
	_ httprouter.Params, config Config) {
	hooks, err := config.listHooks()
	if err != nil {
		log.Println(err)
		http.Error(response, "Unable to list hooks", 500)
		return
	}
	js, _ := json.Marshal(hooks)
	response.Header().Set("content-type", "application/json")
	response.Write(js)
}

// @Title status
// @Description Dictionary with machines and its status
// @Success 200    {object} string "Dictionary with machines and its status"
// @Router /status [GET]
func status(response http.ResponseWriter, request *http.Request,
	ps httprouter.Params, config Config, state State) {
	result, _ := json.Marshal(&state.MachineByHostname)
	response.Write(result)
}

// @Title pixieHandler
// @Description Dictionary with kernel, intrd(s) and commandline for pixiecore
// @Param macaddr    path    string    true    "MacAddress"
// @Success 200    {object} string "Dictionary with kernel, intrd(s) and commandline for pixiecore"
// @Failure 404    {object} string "Not in build mode"
// @Failure 500    {object} string "Unable to find host definition for hostname"
// @Router /v1/boot/{macaddr} [GET]
func pixieHandler(response http.ResponseWriter, request *http.Request,
	ps httprouter.Params, config Config, state State) {

	macaddr := ps.ByName("macaddr")

	state.Mux.Lock()
	m, found := state.MachineByMAC[macaddr]
	state.Mux.Unlock()

	if found == false {
		log.Println(found)
		http.Error(response, "Not in build mode or definition does not exist", 404)
		return
	}

	pxeconfig, _ := m.pixieInit()
	result, _ := json.Marshal(pxeconfig)
	response.Write(result)
}

// @Title healthHandler
// @Description Check that Waitron is running
// @Success 200    {object} string "{"State": "OK"}"
// @Router /health [GET]
func healthHandler(response http.ResponseWriter, request *http.Request,
	ps httprouter.Params, config Config, state State) {

	result, _ := json.Marshal(&result{State: "OK"})

	fmt.Fprintf(response, string(result))
}

func checkForStaleBuilds(state State) {

	staleBuilds := make([]*Machine, 0)

	state.Mux.Lock()

	for _, m := range state.MachineByMAC {
		if int(time.Now().Sub(m.BuildStart).Seconds()) >= m.StaleBuildThresholdSeconds {
			staleBuilds = append(staleBuilds, m)
		}
	}

	state.Mux.Unlock()

	for _, m := range staleBuilds {
		go func() {
			if err := m.RunBuildCommands(m.StaleBuildCommands); err != nil {
				log.Print(err)
			}
		}()
	}
}

func main() {

	config := flag.String("config", "", "Path to config file.")
	address := flag.String("address", "", "Address to listen for requests.")
	port := flag.String("port", "9090", "Port to listen for requests.")
	flag.Parse()

	configFile := *config

	if configFile == "" {
		if configFile = os.Getenv("CONFIG_FILE"); configFile == "" {
			log.Fatal("environment variables CONFIG_FILE must be set or use -config")
		}
	}

	configuration, err := loadConfig(configFile)
	if err != nil {
		log.Fatal(err)
	}

	state := loadState()

	r := httprouter.New()
	r.GET("/list",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			listMachinesHandler(response, request, ps, configuration, state)
		})
	r.GET("/hooks",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			listHooksHandler(response, request, ps, configuration)
		})
	r.PUT("/build/:hostname",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			buildHandler(response, request, ps, configuration, state)
		})
	r.GET("/rescue/:hostname",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			rescueHandler(response, request, ps, configuration, state)
		})
	r.GET("/status/:hostname",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			hostStatus(response, request, ps, configuration, state)
		})
	r.GET("/config/:hostname",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			hostConfigHandler(response, request, ps, configuration)
		})
	r.GET("/config/:hostname/vm",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			hostConfigVmHandler(response, request, ps, configuration)
		})
	r.GET("/status",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			status(response, request, ps, configuration, state)
		})
	r.GET("/done/:hostname/:token",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			doneHandler(response, request, ps, configuration, state)
		})
	r.GET("/cancel/:hostname/:token",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			cancelHandler(response, request, ps, configuration, state)
		})
	r.GET("/template/:template/:hostname/:token",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			templateHandler(response, request, ps, configuration, state)
		})
	r.GET("/v1/boot/:macaddr",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			pixieHandler(response, request, ps, configuration, state)
		})
	r.GET("/health",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			healthHandler(response, request, ps, configuration, state)
		})

	if configuration.StaticFilesPath != "" {
		fs := http.FileServer(http.Dir(configuration.StaticFilesPath))
		r.Handler("GET", "/files/:filename", http.StripPrefix("/files/", fs))
		log.Println("Serving static files from " + configuration.StaticFilesPath)
	}

	if configuration.StaleBuildCheckFrequency <= 0 {
		configuration.StaleBuildCheckFrequency = 300
	}

	ticker := time.NewTicker(time.Duration(configuration.StaleBuildCheckFrequency) * time.Second)

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		for _ = range ticker.C {
			checkForStaleBuilds(state)
		}
	}()

	log.Println("Starting Server on " + *address + ":" + *port)
	log.Fatal(http.ListenAndServe(*address+":"+*port, handlers.LoggingHandler(os.Stdout, r)))

	ticker.Stop()
	wg.Wait()
}
