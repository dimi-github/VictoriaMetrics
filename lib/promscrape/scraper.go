package promscrape

import (
	"bytes"
	"flag"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promscrape/discovery/consul"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/procutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/prompbmarshal"
	"github.com/VictoriaMetrics/metrics"
)

var (
	configCheckInterval = flag.Duration("promscrape.configCheckInterval", 0, "Interval for checking for changes in '-promscrape.config' file. "+
		"By default the checking is disabled. Send SIGHUP signal in order to force config check for changes")
	fileSDCheckInterval = flag.Duration("promscrape.fileSDCheckInterval", 30*time.Second, "Interval for checking for changes in 'file_sd_config'. "+
		"See https://prometheus.io/docs/prometheus/latest/configuration/configuration/#file_sd_config for details")
	kubernetesSDCheckInterval = flag.Duration("promscrape.kubernetesSDCheckInterval", 30*time.Second, "Interval for checking for changes in Kubernetes API server. "+
		"This works only if `kubernetes_sd_configs` is configured in '-promscrape.config' file. "+
		"See https://prometheus.io/docs/prometheus/latest/configuration/configuration/#kubernetes_sd_config for details")
	openstackSDCheckInterval = flag.Duration("promscrape.openstackSDCheckInterval", 30*time.Second, "Interval for checking for changes in openstack API server. "+
		"This works only if `openstack_sd_configs` is configured in '-promscrape.config' file. "+
		"See https://prometheus.io/docs/prometheus/latest/configuration/configuration/#openstack_sd_config for details")
	eurekaSDCheckInterval = flag.Duration("promscrape.eurekaSDCheckInterval", 30*time.Second, "Interval for checking for changes in eureka. "+
		"This works only if `eureka_sd_configs` is configured in '-promscrape.config' file. "+
		"See https://prometheus.io/docs/prometheus/latest/configuration/configuration/#eureka_sd_config for details")
	dnsSDCheckInterval = flag.Duration("promscrape.dnsSDCheckInterval", 30*time.Second, "Interval for checking for changes in dns. "+
		"This works only if `dns_sd_configs` is configured in '-promscrape.config' file. "+
		"See https://prometheus.io/docs/prometheus/latest/configuration/configuration/#dns_sd_config for details")
	ec2SDCheckInterval = flag.Duration("promscrape.ec2SDCheckInterval", time.Minute, "Interval for checking for changes in ec2. "+
		"This works only if `ec2_sd_configs` is configured in '-promscrape.config' file. "+
		"See https://prometheus.io/docs/prometheus/latest/configuration/configuration/#ec2_sd_config for details")
	gceSDCheckInterval = flag.Duration("promscrape.gceSDCheckInterval", time.Minute, "Interval for checking for changes in gce. "+
		"This works only if `gce_sd_configs` is configured in '-promscrape.config' file. "+
		"See https://prometheus.io/docs/prometheus/latest/configuration/configuration/#gce_sd_config for details")
	dockerswarmSDCheckInterval = flag.Duration("promscrape.dockerswarmSDCheckInterval", 30*time.Second, "Interval for checking for changes in dockerswarm. "+
		"This works only if `dockerswarm_sd_configs` is configured in '-promscrape.config' file. "+
		"See https://prometheus.io/docs/prometheus/latest/configuration/configuration/#dockerswarm_sd_config for details")
	promscrapeConfigFile = flag.String("promscrape.config", "", "Optional path to Prometheus config file with 'scrape_configs' section containing targets to scrape. "+
		"See https://victoriametrics.github.io/#how-to-scrape-prometheus-exporters-such-as-node-exporter for details")
	suppressDuplicateScrapeTargetErrors = flag.Bool("promscrape.suppressDuplicateScrapeTargetErrors", false, "Whether to suppress `duplicate scrape target` errors; "+
		"see https://victoriametrics.github.io/vmagent.html#troubleshooting for details")
)

// CheckConfig checks -promscrape.config for errors and unsupported options.
func CheckConfig() error {
	if *promscrapeConfigFile == "" {
		return fmt.Errorf("missing -promscrape.config option")
	}
	_, _, err := loadConfig(*promscrapeConfigFile)
	return err
}

// Init initializes Prometheus scraper with config from the `-promscrape.config`.
//
// Scraped data is passed to pushData.
func Init(pushData func(wr *prompbmarshal.WriteRequest)) {
	globalStopCh = make(chan struct{})
	scraperWG.Add(1)
	go func() {
		defer scraperWG.Done()
		runScraper(*promscrapeConfigFile, pushData, globalStopCh)
	}()
}

// Stop stops Prometheus scraper.
func Stop() {
	close(globalStopCh)
	scraperWG.Wait()
}

var (
	globalStopCh chan struct{}
	scraperWG    sync.WaitGroup
	// PendingScrapeConfigs - zero value means, that
	// all scrapeConfigs are inited and ready for work.
	PendingScrapeConfigs int32
)

func runScraper(configFile string, pushData func(wr *prompbmarshal.WriteRequest), globalStopCh <-chan struct{}) {
	if configFile == "" {
		// Nothing to scrape.
		return
	}

	logger.Infof("reading Prometheus configs from %q", configFile)
	cfg, data, err := loadConfig(configFile)
	if err != nil {
		logger.Fatalf("cannot read %q: %s", configFile, err)
	}

	scs := newScrapeConfigs(pushData)
	scs.add("static_configs", 0, func(cfg *Config, swsPrev []ScrapeWork) []ScrapeWork { return cfg.getStaticScrapeWork() })
	scs.add("file_sd_configs", *fileSDCheckInterval, func(cfg *Config, swsPrev []ScrapeWork) []ScrapeWork { return cfg.getFileSDScrapeWork(swsPrev) })
	scs.add("kubernetes_sd_configs", *kubernetesSDCheckInterval, func(cfg *Config, swsPrev []ScrapeWork) []ScrapeWork { return cfg.getKubernetesSDScrapeWork(swsPrev) })
	scs.add("openstack_sd_configs", *openstackSDCheckInterval, func(cfg *Config, swsPrev []ScrapeWork) []ScrapeWork { return cfg.getOpenStackSDScrapeWork(swsPrev) })
	scs.add("consul_sd_configs", *consul.SDCheckInterval, func(cfg *Config, swsPrev []ScrapeWork) []ScrapeWork { return cfg.getConsulSDScrapeWork(swsPrev) })
	scs.add("eureka_sd_configs", *eurekaSDCheckInterval, func(cfg *Config, swsPrev []ScrapeWork) []ScrapeWork { return cfg.getEurekaSDScrapeWork(swsPrev) })
	scs.add("dns_sd_configs", *dnsSDCheckInterval, func(cfg *Config, swsPrev []ScrapeWork) []ScrapeWork { return cfg.getDNSSDScrapeWork(swsPrev) })
	scs.add("ec2_sd_configs", *ec2SDCheckInterval, func(cfg *Config, swsPrev []ScrapeWork) []ScrapeWork { return cfg.getEC2SDScrapeWork(swsPrev) })
	scs.add("gce_sd_configs", *gceSDCheckInterval, func(cfg *Config, swsPrev []ScrapeWork) []ScrapeWork { return cfg.getGCESDScrapeWork(swsPrev) })
	scs.add("dockerswarm_sd_configs", *dockerswarmSDCheckInterval, func(cfg *Config, swsPrev []ScrapeWork) []ScrapeWork { return cfg.getDockerSwarmSDScrapeWork(swsPrev) })

	sighupCh := procutil.NewSighupChan()

	var tickerCh <-chan time.Time
	if *configCheckInterval > 0 {
		ticker := time.NewTicker(*configCheckInterval)
		tickerCh = ticker.C
		defer ticker.Stop()
	}
	for {
		scs.updateConfig(cfg)
	waitForChans:
		select {
		case <-sighupCh:
			logger.Infof("SIGHUP received; reloading Prometheus configs from %q", configFile)
			cfgNew, dataNew, err := loadConfig(configFile)
			if err != nil {
				logger.Errorf("cannot read %q on SIGHUP: %s; continuing with the previous config", configFile, err)
				goto waitForChans
			}
			if bytes.Equal(data, dataNew) {
				logger.Infof("nothing changed in %q", configFile)
				goto waitForChans
			}
			cfg = cfgNew
			data = dataNew
		case <-tickerCh:
			cfgNew, dataNew, err := loadConfig(configFile)
			if err != nil {
				logger.Errorf("cannot read %q: %s; continuing with the previous config", configFile, err)
				goto waitForChans
			}
			if bytes.Equal(data, dataNew) {
				// Nothing changed since the previous loadConfig
				goto waitForChans
			}
			cfg = cfgNew
			data = dataNew
		case <-globalStopCh:
			logger.Infof("stopping Prometheus scrapers")
			startTime := time.Now()
			scs.stop()
			logger.Infof("stopped Prometheus scrapers in %.3f seconds", time.Since(startTime).Seconds())
			return
		}
		logger.Infof("found changes in %q; applying these changes", configFile)
		configReloads.Inc()
	}
}

var configReloads = metrics.NewCounter(`vm_promscrape_config_reloads_total`)

type scrapeConfigs struct {
	pushData func(wr *prompbmarshal.WriteRequest)
	wg       sync.WaitGroup
	stopCh   chan struct{}
	scfgs    []*scrapeConfig
}

func newScrapeConfigs(pushData func(wr *prompbmarshal.WriteRequest)) *scrapeConfigs {
	return &scrapeConfigs{
		pushData: pushData,
		stopCh:   make(chan struct{}),
	}
}

func (scs *scrapeConfigs) add(name string, checkInterval time.Duration, getScrapeWork func(cfg *Config, swsPrev []ScrapeWork) []ScrapeWork) {
	atomic.AddInt32(&PendingScrapeConfigs, 1)
	scfg := &scrapeConfig{
		name:          name,
		pushData:      scs.pushData,
		getScrapeWork: getScrapeWork,
		checkInterval: checkInterval,
		cfgCh:         make(chan *Config, 1),
		stopCh:        scs.stopCh,
	}
	scs.wg.Add(1)
	go func() {
		defer scs.wg.Done()
		scfg.run()
	}()
	scs.scfgs = append(scs.scfgs, scfg)
}

func (scs *scrapeConfigs) updateConfig(cfg *Config) {
	for _, scfg := range scs.scfgs {
		scfg.cfgCh <- cfg
	}
}

func (scs *scrapeConfigs) stop() {
	close(scs.stopCh)
	scs.wg.Wait()
	scs.scfgs = nil
}

type scrapeConfig struct {
	name          string
	pushData      func(wr *prompbmarshal.WriteRequest)
	getScrapeWork func(cfg *Config, swsPrev []ScrapeWork) []ScrapeWork
	checkInterval time.Duration
	cfgCh         chan *Config
	stopCh        <-chan struct{}
}

func (scfg *scrapeConfig) run() {
	sg := newScraperGroup(scfg.name, scfg.pushData)
	defer sg.stop()

	var tickerCh <-chan time.Time
	if scfg.checkInterval > 0 {
		ticker := time.NewTicker(scfg.checkInterval)
		defer ticker.Stop()
		tickerCh = ticker.C
	}

	cfg := <-scfg.cfgCh
	var swsPrev []ScrapeWork
	updateScrapeWork := func(cfg *Config) {
		sws := scfg.getScrapeWork(cfg, swsPrev)
		sg.update(sws)
		swsPrev = sws
	}
	updateScrapeWork(cfg)
	atomic.AddInt32(&PendingScrapeConfigs, -1)

	for {

		select {
		case <-scfg.stopCh:
			return
		case cfg = <-scfg.cfgCh:
		case <-tickerCh:
		}
		updateScrapeWork(cfg)
	}
}

type scraperGroup struct {
	name         string
	wg           sync.WaitGroup
	mLock        sync.Mutex
	m            map[string]*scraper
	pushData     func(wr *prompbmarshal.WriteRequest)
	changesCount *metrics.Counter
}

func newScraperGroup(name string, pushData func(wr *prompbmarshal.WriteRequest)) *scraperGroup {
	sg := &scraperGroup{
		name:         name,
		m:            make(map[string]*scraper),
		pushData:     pushData,
		changesCount: metrics.NewCounter(fmt.Sprintf(`vm_promscrape_config_changes_total{type=%q}`, name)),
	}
	metrics.NewGauge(fmt.Sprintf(`vm_promscrape_targets{type=%q, status="up"}`, name), func() float64 {
		return float64(tsmGlobal.StatusByGroup(sg.name, true))
	})
	metrics.NewGauge(fmt.Sprintf(`vm_promscrape_targets{type=%q, status="down"}`, name), func() float64 {
		return float64(tsmGlobal.StatusByGroup(sg.name, false))
	})
	return sg
}

func (sg *scraperGroup) stop() {
	sg.mLock.Lock()
	for _, sc := range sg.m {
		close(sc.stopCh)
	}
	sg.m = nil
	sg.mLock.Unlock()
	sg.wg.Wait()
}

func (sg *scraperGroup) update(sws []ScrapeWork) {
	sg.mLock.Lock()
	defer sg.mLock.Unlock()

	additionsCount := 0
	deletionsCount := 0
	swsMap := make(map[string][]prompbmarshal.Label, len(sws))
	for i := range sws {
		sw := &sws[i]
		key := sw.key()
		originalLabels := swsMap[key]
		if originalLabels != nil {
			if !*suppressDuplicateScrapeTargetErrors {
				logger.Errorf("skipping duplicate scrape target with identical labels; endpoint=%s, labels=%s; "+
					"make sure service discovery and relabeling is set up properly; "+
					"see also https://victoriametrics.github.io/vmagent.html#troubleshooting; "+
					"original labels for target1: %s; original labels for target2: %s",
					sw.ScrapeURL, sw.LabelsString(), promLabelsString(originalLabels), promLabelsString(sw.OriginalLabels))
			}
			droppedTargetsMap.Register(sw.OriginalLabels)
			continue
		}
		swsMap[key] = sw.OriginalLabels
		if sg.m[key] != nil {
			// The scraper for the given key already exists.
			continue
		}

		// Start a scraper for the missing key.
		sc := newScraper(sw, sg.name, sg.pushData)
		sg.wg.Add(1)
		go func() {
			defer sg.wg.Done()
			sc.sw.run(sc.stopCh)
			tsmGlobal.Unregister(sw)
		}()
		tsmGlobal.Register(sw)
		sg.m[key] = sc
		additionsCount++
	}

	// Stop deleted scrapers, which are missing in sws.
	for key, sc := range sg.m {
		if _, ok := swsMap[key]; !ok {
			close(sc.stopCh)
			delete(sg.m, key)
			deletionsCount++
		}
	}

	if additionsCount > 0 || deletionsCount > 0 {
		sg.changesCount.Add(additionsCount + deletionsCount)
		logger.Infof("%s: added targets: %d, removed targets: %d; total targets: %d", sg.name, additionsCount, deletionsCount, len(sg.m))
	}
}

type scraper struct {
	sw     scrapeWork
	stopCh chan struct{}
}

func newScraper(sw *ScrapeWork, group string, pushData func(wr *prompbmarshal.WriteRequest)) *scraper {
	sc := &scraper{
		stopCh: make(chan struct{}),
	}
	c := newClient(sw)
	sc.sw.Config = *sw
	sc.sw.ScrapeGroup = group
	sc.sw.ReadData = c.ReadData
	sc.sw.GetStreamReader = c.GetStreamReader
	sc.sw.PushData = pushData
	return sc
}
