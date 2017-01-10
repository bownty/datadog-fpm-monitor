package main

import (
	"expvar"
	"fmt"
	"net/http"
	"os"
	"sort"

	"strconv"
	"strings"

	"io/ioutil"

	"github.com/gorilla/mux"
	consul "github.com/hashicorp/consul/api"
	"github.com/scukonick/go-fastcgi-client"
	yaml "gopkg.in/yaml.v2"
)

var (
	exprInstances = expvar.NewInt("php_fpm_instances")
	exprReloads   = expvar.NewInt("datadog_reload")
)

// PhpFpmConfig ...
type PhpFpmConfig struct {
	InitConfig []string            `yaml:"init_config,flow"`
	Instances  []*PhpFpmConfigItem `yaml:"instances"`
}

// PhpFpmConfigItem ...
type PhpFpmConfigItem struct {
	StatusUURL string   `yaml:"status_url"`
	PingURL    string   `yaml:"ping_url"`
	PingReply  string   `yaml:"ping_reply"`
	Tags       []string `yaml:"tags"`
}

// Connect to the upstream php-fpm process and get its current status
func httpShowPhpFpmFastCgiStatus(w http.ResponseWriter, r *http.Request) {
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
		message := fmt.Sprintf("[php-fpm] Invalid port %s: %s", port, err)
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
	env["QUERY_STRING"] = "json=1"

	// create fastcgi client
	fcgi, err := fcgiclient.New(ip, realPort)
	if err != nil {
		message := fmt.Sprintf("[php-fpm] Could not create fastcgi client: %s", err)
		logger.Errorf(message)
		http.Error(w, message, 500)
		return
	}

	// do the fastcgi request
	response, err := fcgi.Request(env, "json=1")
	if err != nil {
		message := fmt.Sprintf("[php-fpm] Failed fastcgi request: %s", err)
		logger.Errorf(message)
		http.Error(w, message, 500)
		return
	}

	// parse the fastcgi response
	body, err := response.ParseStdouts()
	if err != nil {
		message := fmt.Sprintf("[php-fpm] Failed to parse fastcgi response: %s", err)
		logger.Errorf(message)
		http.Error(w, message, 500)
		return
	}

	// read the response into a []bytes
	resp, err := ioutil.ReadAll(body.Body)
	if err != nil {
		message := fmt.Sprintf("[php-fpm] Failed to read fastcgi response: %s", err)
		logger.Errorf(message)
		http.Error(w, message, 500)
		return
	}

	// write to client
	w.Write(resp)

	logger.Debugf("[php-fpm] Request complete. Sent %d bytes", len(resp))
}

// continuously monitor the local agent services for php-fpm services
// and register them to the local datadog client
func monitorPhpFpmServices(nodeName string, quitCh chan string) {
	filePath := os.Getenv("PHP_FPM_CONFIG_FILE")
	if filePath == "" {
		filePath = "/etc/dd-agent/conf.d/php_fpm.yaml"
	}

	currentHash, err := hashFileMd5(filePath)
	if err != nil {
		logger.Warnf("[php-fpm] Could not get initial hash for %s: %s", filePath, err)
		currentHash = ""
	}

	logger.Infof("[php-fpm] Existing file hash %s: %s", filePath, currentHash)

	file, err := os.Create(filePath)
	if err != nil {
		logger.Fatalf("[php-fpm] Could not create file %s: %s", filePath, err)
		return
	}
	defer file.Close()

	stream := consulServices.Observe()

	for {
		select {
		case <-quitCh:
			logger.Warn("[php-fpm] Stopping")
			return

		case <-stream.Changes():
			stream.Next()

			t := &PhpFpmConfig{}

			services := stream.Value().(map[string]*consul.AgentService)

			for _, service := range services {
				if !strings.HasSuffix(service.Service, "-php-fpm") {
					logger.Debugf("[php-fpm] Service %s does not match '-php-fpm' suffix", service.Service)
					continue
				}

				projectName := strings.TrimRight(service.Service, "-php-fpm")

				check := &PhpFpmConfigItem{}
				check.PingURL = fmt.Sprintf("http://%s:%s/php-fpm/%s/%s/%d/ping", service.Address, listenPort, projectName, service.Address, service.Port)
				check.PingReply = "pong"
				check.StatusUURL = fmt.Sprintf("http://%s:%s/php-fpm/%s/%s/%d/status", service.Address, listenPort, projectName, service.Address, service.Port)
				check.Tags = []string{
					fmt.Sprintf("project:%s", projectName),
				}

				t.Instances = append(t.Instances, check)

				logger.Infof("[php-fpm] Service %s does match '-php-fpm' suffix", service.Service)
			}

			// Sort the services by name so we get consistent output across runs
			sort.Sort(PhpFpmServiceSorter(t.Instances))

			instanceCount := len(t.Instances)
			exprInstances.Set(int64(instanceCount))

			d, err := yaml.Marshal(&t)
			if err != nil {
				logger.Fatalf("[php-fpm] Could not marshal yaml: %v", err)
				break
			}

			text := string(d)
			text = strings.Trim(text, " ")
			text = "---\n" + text

			d = []byte(text)

			newHash := hashBytes(d)
			if newHash == currentHash {
				logger.Info("[php-fpm] File hash is the same, NOOP")
				continue
			}

			if err := file.Truncate(0); err != nil {
				logger.Errorf("[php-fpm] Could not truncate file %s: %s", filePath, err)
				continue
			}

			if _, err := file.Write(d); err != nil {
				logger.Errorf("[php-fpm] Could not write file %s: %s", filePath, err)
				continue
			}

			if err := file.Sync(); err != nil {
				logger.Errorf("[php-fpm] Could not sync file %s: %s", filePath, err)
				continue
			}

			logger.Infof("[php-fpm] Successfully updated file: %s (old: %s | new: %s)", filePath, currentHash, newHash)
			currentHash = newHash

			reloadDataDogService()
		}
	}
}

// PhpFpmServiceSorter sorts planets by PingURL
type PhpFpmServiceSorter []*PhpFpmConfigItem

func (a PhpFpmServiceSorter) Len() int           { return len(a) }
func (a PhpFpmServiceSorter) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a PhpFpmServiceSorter) Less(i, j int) bool { return a[i].PingURL < a[j].PingURL }
