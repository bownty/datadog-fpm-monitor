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
		logger.Warnf("[go-expvar] Could not get initial hash for %s: %s", filePath, err)
		currentHash = ""
	}

	logger.Infof("[go-expvar] Existing file hash %s: %s", filePath, currentHash)

	file, err := os.Create(filePath)
	if err != nil {
		logger.Fatalf("[go-expvar] Could not create file %s: %s", filePath, err)
	}

	defer file.Close()

	stream := consulServices.Observe()

	for {
		select {
		case <-quitCh:
			logger.Warn("[go-expvar] stopping")
			return

		case <-stream.Changes():
			stream.Next()

			t := &GoExprConfig{}

			services := stream.Value().(map[string]*consul.AgentService)

			for _, service := range services {
				if !strings.HasSuffix(service.Service, "-go-expvar") {
					logger.Debugf("[go-expvar] Service %s does not match '-go-expvar' suffix", service.Service)
					continue
				}
				logger.Infof("[go-expvar] Service %s does match '-php-fpm' suffix", service.Service)

				// projectName := strings.TrimRight(service.Service, "-go-expvar")

				check := getRemoteConfig(fmt.Sprintf("http://%s:%d/datadog/expvar", service.Address, service.Port))
				if check.ExpvarURL != "" {
					t.Instances = append(t.Instances, check)
				}
			}

			// Sort the services by name so we get consistent output across runs
			// sort.Sort(ServiceSorter(t.Instances))

			instanceCount := len(t.Instances)
			exprInstances.Set(int64(instanceCount))

			d, err := yaml.Marshal(&t)
			if err != nil {
				logger.Fatalf("[go-expvar] could not marshal yaml: %v", err)
			}

			text := string(d)
			text = "---\n" + text

			d = []byte(text)

			newHash := hashBytes(d)
			if newHash == currentHash {
				logger.Info("[go-expvar] File hash is the same, NOOP")
				continue
			}

			if err := file.Truncate(0); err != nil {
				logger.Errorf("[go-expvar] Could not truncate file %s: %s", filePath, err)
				continue
			}

			if _, err := file.Write(d); err != nil {
				logger.Errorf("[go-expvar] Could not write file %s: %s", filePath, err)
				continue
			}

			logger.Infof("[go-expvar] Successfully updated file: %s (old: %s | new: %s)", filePath, currentHash, newHash)
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
		logger.Errorf("[go-expvar] Could not GET url '%s': %s", url, err.Error())
		return config
	}

	defer response.Body.Close()
	content, err := ioutil.ReadAll(response.Body)
	if err != nil {
		logger.Errorf("[go-expvar] Could not read response '%s': %s", url, err.Error())
		return config
	}

	err = yaml.Unmarshal(content, &config)
	if err != nil {
		logger.Errorf("[go-expvar] Could not marshal response into YAML '%s', %s", url, err.Error())
		return config
	}

	goExprConfigCache.Set(url, config, cache.DefaultExpiration)
	return config
}
