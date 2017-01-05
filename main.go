package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"

	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"io/ioutil"

	"github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
	consul "github.com/hashicorp/consul/api"
	"github.com/scukonick/go-fastcgi-client"
	graceful "gopkg.in/tylerb/graceful.v1"
	yaml "gopkg.in/yaml.v2"
)

var logger = logrus.New()

// PhpFpmServices ...
type PhpFpmServices struct {
	InitConfig []string       `yaml:"init_config"`
	Instances  []*PhpFpmCheck `yaml:"instances"`
}

// PhpFpmCheck ...
type PhpFpmCheck struct {
	StatusUURL string   `yaml:"status_url"`
	PingURL    string   `yaml:"ping_url"`
	Tags       []string `yaml:"tags,flow"`
}

func main() {
	logger.Info("Starting PHP-FPM datadog monitoring ")

	// Create consul client
	config := consul.DefaultConfig()
	client, err := consul.NewClient(config)
	if err != nil {
		logger.Errorf("Could not connect to Consul backend: %s", err)
		return
	}

	// Get local agent information
	self, err := client.Agent().Self()
	if err != nil {
		logger.Errorf("Could not look up self(): %s", err)
		return
	}

	// look up the agent node name
	nodeName := self["Config"]["NodeName"].(string)
	logger.Infof("Hello, my name is %s", nodeName)

	// create quit channel for go-routines
	quitCh := make(chan string, 1)

	// start monitoring of consul services
	go monitorServices(client, nodeName, quitCh)

	// start the http reserver that proxies http requests to php-cgi
	router := mux.NewRouter()
	router.HandleFunc("/php-fpm/{project}/{ip}/{port}/{type}", showStatus)

	// create logger for http server
	w := logger.Writer()
	defer w.Close()

	server := &graceful.Server{
		Timeout:          5 * time.Second,
		TCPKeepAlive:     5 * time.Second,
		Server:           &http.Server{Addr: ":4000", Handler: router},
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

// Connect to the upstream php-fpm process and get its current status
func showStatus(w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)

	// variables we require to have present in the URL
	// they always exist thanks to the router
	project := params["project"]
	ip := params["ip"]
	port := params["port"]
	endpoint := params["type"]

	// convert the string port to int
	realPort, err := strconv.Atoi(port)
	if err != nil {
		message := fmt.Sprintf("Invalid port %s: %s", port, err)
		logger.Errorf(message)
		http.Error(w, message, 500)
		return
	}

	// construct the env we need for php-fpm to allow ac
	env := make(map[string]string)
	env["REQUEST_METHOD"] = "GET"
	env["SCRIPT_FILENAME"] = fmt.Sprintf("/%s/internal/%s", project, endpoint)
	env["SCRIPT_NAME"] = fmt.Sprintf("/%s/internal/%s", project, endpoint)
	env["SERVER_SOFTWARE"] = "go / fcgiclient "

	// create fastcgi client
	fcgi, err := fcgiclient.New(ip, realPort)
	if err != nil {
		message := fmt.Sprintf("Could not create fastcgi client: %s", err)
		logger.Errorf(message)
		http.Error(w, message, 500)
		return
	}

	// do the fastcgi request
	response, err := fcgi.Request(env, "")
	if err != nil {
		message := fmt.Sprintf("Failed fastcgi request: %s", err)
		logger.Errorf(message)
		http.Error(w, message, 500)
		return
	}

	// parse the fastcgi response
	body, err := response.ParseStdouts()
	if err != nil {
		message := fmt.Sprintf("Failed to parse fastcgi response: %s", err)
		logger.Errorf(message)
		http.Error(w, message, 500)
		return
	}

	// read the response into a []bytes
	resp, err := ioutil.ReadAll(body.Body)
	if err != nil {
		message := fmt.Sprintf("Failed to read fastcgi response: %s", err)
		logger.Errorf(message)
		http.Error(w, message, 500)
		return
	}

	// write to client
	w.Write(resp)

	logger.Infof("Request complete. Sent %d bytes", len(resp))
}

// continuously monitor the local agent services for php-fpm services
// and register them to the local datadog client
func monitorServices(client *consul.Client, nodeName string, quitCh chan string) {
	filePath := os.Getenv("TARGET_FILE")
	if filePath == "" {
		filePath = "/etc/dd-agent/conf.d/php_fpm.yaml"
	}

	currentHash, err := hashFileMd5(filePath)
	if err != nil {
		logger.Warnf("Could not get initial hash for %s: %s", filePath, err)
		currentHash = ""
	}

	logger.Infof("Existing file hash %s: %s", filePath, currentHash)

	file, err := os.Create(filePath)
	if err != nil {
		logger.Fatalf("Could not create file %s: %s", filePath, err)
		return
	}
	defer file.Close()

	for {
		select {
		case <-quitCh:
			logger.Warn("Stopping monitorServices")
			return

		default:
			logger.Infof("Monitoring services...")
			t := &PhpFpmServices{}

			services, err := client.Agent().Services()
			if err != nil {
				logger.Errorf("Could not fetch agent services: %s", err)
				return
			}

			for _, service := range services {
				if !strings.HasSuffix(service.Service, "-php-fpm") {
					logger.Debugf("Service %s does not match '-php-fpm' suffix", service.Service)
					continue
				}

				projectName := strings.TrimRight(service.Service, "-php-fpm")

				check := &PhpFpmCheck{}
				check.PingURL = fmt.Sprintf("http://127.0.0.1:4000/php-fpm/%s/%s/%d/ping", projectName, service.Address, service.Port)
				check.StatusUURL = fmt.Sprintf("http://127.0.0.1:4000/php-fpm/%s/%s/%d/status", projectName, service.Address, service.Port)
				check.Tags = []string{projectName}

				t.Instances = append(t.Instances, check)

				logger.Infof("Service %s does match '-php-fpm' suffix", service.Service)
			}

			// Sort the services by name so we get consistent output across runs
			sort.Sort(ServiceSorter(t.Instances))

			d, err := yaml.Marshal(&t)
			if err != nil {
				logger.Fatalf("error: %v", err)
				break
			}

			newHash := hashBytes(d)
			if newHash == currentHash {
				logger.Info("File hash is the same, NOOP")
				time.Sleep(5 * time.Second)
				continue
			}

			if err := file.Truncate(0); err != nil {
				logger.Errorf("Could not truncate file %s: %s", filePath, err)
				time.Sleep(5 * time.Second)
				continue
			}

			if _, err := file.Write(d); err != nil {
				logger.Errorf("Could not write file %s: %s", filePath, err)
				time.Sleep(5 * time.Second)
				continue
			}

			logger.Infof("Successfully updated file: %s (old: %s | new: %s)", filePath, currentHash, newHash)
			currentHash = newHash

			reloadService()

			time.Sleep(5 * time.Second)
		}
	}
}

func reloadService() {
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

// ServiceSorter sorts planets by PingURL
type ServiceSorter []*PhpFpmCheck

func (a ServiceSorter) Len() int           { return len(a) }
func (a ServiceSorter) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ServiceSorter) Less(i, j int) bool { return a[i].PingURL < a[j].PingURL }
