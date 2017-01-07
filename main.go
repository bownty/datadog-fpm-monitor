package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	consul "github.com/hashicorp/consul/api"
	observer "github.com/imkira/go-observer"
	"github.com/sirupsen/logrus"
	graceful "gopkg.in/tylerb/graceful.v1"
	yaml "gopkg.in/yaml.v2"
)

var logger = logrus.New()
var consulServices = observer.NewProperty(make(map[string]*consul.AgentService, 0))
var listenPort = getListenPort()

func main() {
	logger.Info("Starting datadog monitoring ")

	// Create consul client
	config := consul.DefaultConfig()
	client, err := consul.NewClient(config)
	if err != nil {
		logger.Fatalf("Could not connect to Consul backend: %s", err)
	}

	// Get local agent information
	self, err := client.Agent().Self()
	if err != nil {
		logger.Fatalf("Could not look up self(): %s", err)
	}

	// look up the agent node name
	nodeName := self["Config"]["NodeName"].(string)
	logger.Infof("Connected to Consul node: %s", nodeName)

	// create quit channel for go-routines
	quitCh := make(chan string, 1)

	// start monitoring of consul services
	go monitorConsulServices(client, quitCh)
	go monitorPhpFpmServices(nodeName, quitCh)
	go monitorGoExprvarServices(nodeName, quitCh)

	// start the http reserver that proxies http requests to php-cgi
	router := mux.NewRouter()
	router.Handle("/debug/vars", http.DefaultServeMux)
	router.HandleFunc("/datadog/expvar", showExprVar)
	router.HandleFunc("/php-fpm/{project}/{ip}/{port}/{type}", httpShowPhpFpmFastCgiStatus)

	logger.Infof("")
	logger.Info("Entrypoints:")
	logger.Infof("  - http://127.0.0.1:%s/debug/vars", listenPort)
	logger.Infof("  - http://127.0.0.1:%s/datadog/expvar", listenPort)
	logger.Infof("  - http://127.0.0.1:%s/php-fpm/{project}/{ip}/{port}/{type}", listenPort)
	logger.Infof("")

	// create logger for http server
	w := logger.Writer()
	defer w.Close()

	server := &graceful.Server{
		Timeout:          5 * time.Second,
		TCPKeepAlive:     5 * time.Second,
		Server:           &http.Server{Addr: ":" + listenPort, Handler: router},
		Logger:           log.New(w, "HTTP: ", 0),
		NoSignalHandling: true,
	}

	// setup signal handlers
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		signal := <-signalCh
		logger.Warnf("We got signal: %s", signal.String())

		logger.Warn("Closing quitCh")
		close(quitCh)

		logger.Warn("Telling HTTP server to shut down")
		server.Stop(5 * time.Second)

		logger.Info("Shutdown complete")
	}()

	// start the HTTP server (this will block)
	err = server.ListenAndServe()
	if err != nil {
		logger.Fatal(err)
		return
	}

	logger.Info("end of program")
}

func reloadDataDogService() {
	exprReloads.Add(1)

	if os.Getenv("DONT_RELOAD_DATADOG") != "" {
		logger.Infof("Not reloading datadog-agent (env: DONT_RELOAD_DATADOG)")
		return
	}

	cmd := "/usr/sbin/service"
	args := []string{"datadog-agent", "reload"}
	if err := exec.Command(cmd, args...).Run(); err != nil {
		logger.Fatalf("Failed to reload datadog-agent: %s", err)
		return
	}

	logger.Infof("Successfully reloaded datadog-agent")
}

func hashBytes(data []byte) string {
	hash := md5.New()

	// write the data to hasher
	hash.Write(data)

	// Get the 16 bytes hash
	hashInBytes := hash.Sum(nil)

	// Convert the bytes to a string
	returnMD5String := hex.EncodeToString(hashInBytes)

	return returnMD5String
}

func hashFileMd5(filePath string) (string, error) {
	// Initialize variable returnMD5String now in case an error has to be returned
	var returnMD5String string

	// Open the passed argument and check for any error
	file, err := os.Open(filePath)
	if err != nil {
		return returnMD5String, err
	}

	// Tell the program to call the following function when the current function returns
	defer file.Close()

	// Open a new hash interface to write to
	hash := md5.New()

	// Copy the file in the hash interface and check for any error
	if _, err := io.Copy(hash, file); err != nil {
		return returnMD5String, err
	}

	// Get the 16 bytes hash
	hashInBytes := hash.Sum(nil)

	// Convert the bytes to a string
	returnMD5String = hex.EncodeToString(hashInBytes)

	return returnMD5String, nil
}

// Monitor consul services and emit updates when they change
func monitorConsulServices(client *consul.Client, quitCh chan string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-quitCh:
			logger.Warn("Stopping monitorConsulServices")
			return

		case <-ticker.C:
			services, err := client.Agent().Services()
			if err != nil {
				logger.Warnf("Could not fetch Consul clients: %s", err)
			}

			consulServices.Update(services)
		}
	}
}

func getListenPort() string {
	port := os.Getenv("NOMAD_PORT_http")
	if port == "" {
		port = "4000"
	}

	return port
}

func showExprVar(w http.ResponseWriter, r *http.Request) {
	metrics := make([]map[string]string, 0)
	metrics = append(metrics, map[string]string{"path": "php_fpm_instances"})
	metrics = append(metrics, map[string]string{"path": "datadog_reload"})

	config := struct {
		ExpvarURL string              `yaml:"expvar_url"`
		Tags      []string            `yaml:"tags"`
		Metrics   []map[string]string `yaml:"metrics"`
	}{
		"http://127.0.0.1:" + listenPort + "/debug/vars",
		[]string{"project:datadog-monitor"},
		metrics,
	}

	resp, err := yaml.Marshal(&config)
	if err != nil {
		message := fmt.Sprintf("[showExprVar] Could not marshal YAML: %s", err)
		logger.Errorf(message)
		http.Error(w, message, 500)
		return
	}

	text := string(resp)
	text = "---\n" + text

	resp = []byte(text)

	w.Header().Add("Content-Type", "text/yaml")
	w.Write(resp)
}
