package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	consul "github.com/hashicorp/consul/api"
	cache "github.com/patrickmn/go-cache"
	yaml "gopkg.in/yaml.v2"
)

// GoExprConfig ...
type GoExprConfig struct {
	InitConfig []string            `yaml:"init_config,flow"`
	Instances  []*GoExprConfigItem `yaml:"instances"`
}

// GoExprConfigItem ...
type GoExprConfigItem struct {
	ExpvarURL string                `yaml:"expvar_url"`
	Tags      []string              `yaml:"tags"`
	Metrics   []*GoExprMetricConfig `yaml:"metrics"`
}

// GoExprMetricConfig ...
type GoExprMetricConfig map[string]string

var goExprConfigCache = cache.New(30*time.Minute, 30*time.Second)

func monitorGoExprvarServices(nodeName string, quitCh chan string) {
	filePath := os.Getenv("TARGET_FILE_GO_EXPR")
	if filePath == "" {
		filePath = "/etc/dd-agent/conf.d/go_expvar.yaml"
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

	stream := consulServices.Observe()

	for {
		select {
		case <-quitCh:
			logger.Warn("Stopping monitorGoExprvarServices")
			return

		case <-stream.Changes():
			stream.Next()

			t := &GoExprConfig{}

			services := stream.Value().(map[string]*consul.AgentService)

			for _, service := range services {
				if !strings.HasSuffix(service.Service, "-go-expvar") {
					logger.Debugf("Service %s does not match '-go-expvar' suffix", service.Service)
					continue
				}

				// projectName := strings.TrimRight(service.Service, "-go-expvar")

				check := getRemoteConfig(fmt.Sprintf("http://%s:%d/datadog/expvar", service.Address, service.Port))
				if check.ExpvarURL != "" {
					t.Instances = append(t.Instances, check)
				}

				logger.Infof("Service %s does match '-php-fpm' suffix", service.Service)
			}

			// Sort the services by name so we get consistent output across runs
			// sort.Sort(ServiceSorter(t.Instances))

			instanceCount := len(t.Instances)
			exprInstances.Set(int64(instanceCount))

			d, err := yaml.Marshal(&t)
			if err != nil {
				logger.Fatalf("error: %v", err)
				break
			}

			text := string(d)
			text = "---\n" + text

			d = []byte(text)

			newHash := hashBytes(d)
			if newHash == currentHash {
				logger.Info("File hash is the same, NOOP")
				continue
			}

			if err := file.Truncate(0); err != nil {
				logger.Errorf("Could not truncate file %s: %s", filePath, err)
				continue
			}

			if _, err := file.Write(d); err != nil {
				logger.Errorf("Could not write file %s: %s", filePath, err)
				continue
			}

			logger.Infof("Successfully updated file: %s (old: %s | new: %s)", filePath, currentHash, newHash)
			currentHash = newHash

			reloadDataDogService()
		}
	}
}

func getRemoteConfig(url string) (config *GoExprConfigItem) {
	cached, found := goExprConfigCache.Get(url)
	if found {
		config = cached.(*GoExprConfigItem)
		return config
	}

	response, err := http.Get(url)
	if err != nil {
		logger.Errorf("Could not GET url '%s': %s", url, err.Error())
		return config
	}

	defer response.Body.Close()
	content, err := ioutil.ReadAll(response.Body)
	if err != nil {
		logger.Errorf("Could not read response '%s': %s", url, err.Error())
		return config
	}

	err = yaml.Unmarshal(content, &config)
	if err != nil {
		logger.Errorf("Could not marshal response into YAML '%s', %s", url, err.Error())
		return config
	}

	goExprConfigCache.Set(url, config, cache.DefaultExpiration)
	return config
}
