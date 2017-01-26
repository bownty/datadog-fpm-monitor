package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"sort"
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
	filePath := os.Getenv("GO_EXPR_CONFIG_FILE")
	if filePath == "" {
		filePath = "/etc/dd-agent/conf.d/go_expvar.yaml"
	}

	currentHash, err := hashFileMd5(filePath)
	if err != nil {
		logger.Warnf("[go-expvar] Could not get initial hash for %s: %s", filePath, err)
		currentHash = ""
	}

	logger.Infof("[go-expvar] Existing file hash %s: %s", filePath, currentHash)

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

				url := fmt.Sprintf("http://%s:%d/datadog/expvar", service.Address, service.Port)

				check, err := getRemoteConfig(url)
				if err != nil {
					logger.Warnf("[go-expvar] Could not get remote config for %s: %s", url, err)
					continue
				}

				if check.ExpvarURL != "" {
					t.Instances = append(t.Instances, check)
				}
			}

			// Sort the services by name so we get consistent output across runs
			sort.Sort(GoExprServiceSorter(t.Instances))

			instanceCount := len(t.Instances)
			exprInstances.Set(int64(instanceCount))

			data, err := yaml.Marshal(&t)
			if err != nil {
				logger.Fatalf("[go-expvar] could not marshal yaml: %v", err)
			}

			text := string(data)
			text = "---\n" + text

			// turn the text back to bytes for hashing
			data = []byte(text)

			// compare hash of the new content vs file on disk
			newHash := hashBytes(data)
			if newHash == currentHash {
				logger.Info("[go-expvar] File hash is the same, NOOP")
				continue
			}

			// open file for write (truncated)
			file, err := os.Create(filePath)
			if err != nil {
				logger.Fatalf("[go-expvar] Could not create file %s: %s", filePath, err)
				continue
			}

			// write file to disk
			if _, err := file.Write(data); err != nil {
				logger.Errorf("[go-expvar] Could not write file %s: %s", filePath, err)
				file.Close()
				continue
			}
			file.Close()

			logger.Infof("[go-expvar] Successfully updated file: %s (old: %s | new: %s)", filePath, currentHash, newHash)
			currentHash = newHash

			reloadDataDogService()
		}
	}
}

func getRemoteConfig(url string) (config *GoExprConfigItem, err error) {
	cached, found := goExprConfigCache.Get(url)
	if found {
		config = cached.(*GoExprConfigItem)
		return config, nil
	}

	response, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("Could not GET url '%s': %s", url, err.Error())
	}

	defer response.Body.Close()
	content, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("Could not read response '%s': %s", url, err.Error())
	}

	err = yaml.Unmarshal(content, &config)
	if err != nil {
		return nil, fmt.Errorf("Could not marshal response into YAML '%s', %s", url, err.Error())
	}

	goExprConfigCache.Set(url, config, cache.DefaultExpiration)
	return config, nil
}

// GoExprServiceSorter sorts planets by ExpvarURL
type GoExprServiceSorter []*GoExprConfigItem

func (a GoExprServiceSorter) Len() int           { return len(a) }
func (a GoExprServiceSorter) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a GoExprServiceSorter) Less(i, j int) bool { return a[i].ExpvarURL < a[j].ExpvarURL }
