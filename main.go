package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bestruirui/bestsub/config"
	"github.com/bestruirui/bestsub/proxy"
	"github.com/bestruirui/bestsub/proxy/checker"
	"github.com/bestruirui/bestsub/proxy/info"
	"github.com/bestruirui/bestsub/proxy/saver"
	"github.com/bestruirui/bestsub/utils"
	"github.com/fsnotify/fsnotify"
	"github.com/metacubex/mihomo/log"
	"gopkg.in/yaml.v3"
)

type App struct {
	renamePath  string
	configPath  string
	interval    int
	watcher     *fsnotify.Watcher
	reloadTimer *time.Timer
}

func NewApp() *App {
	configPath := flag.String("f", "", "config file path")
	renamePath := flag.String("r", "", "rename file path")
	flag.Parse()

	return &App{
		configPath: *configPath,
		renamePath: *renamePath,
	}
}

func (app *App) Initialize() error {
	if err := app.initConfigPath(); err != nil {
		return fmt.Errorf("init config path failed: %w", err)
	}

	if err := app.loadConfig(); err != nil {
		return fmt.Errorf("load config failed: %w", err)
	}
	checkConfig()
	if err := app.initConfigWatcher(); err != nil {
		return fmt.Errorf("init config watcher failed: %w", err)
	}

	app.interval = config.GlobalConfig.Check.Interval
	log.SetLevel(log.ERROR)
	if config.GlobalConfig.Save.Method == "http" {
		saver.StartHTTPServer()
	}
	return nil
}

func (app *App) initConfigPath() error {
	execPath := utils.GetExecutablePath()
	configDir := filepath.Join(execPath, "config")

	if app.configPath == "" {
		if err := os.MkdirAll(configDir, 0755); err != nil {
			return fmt.Errorf("create config dir failed: %w", err)
		}

		app.configPath = filepath.Join(configDir, "config.yaml")
	}
	if app.renamePath == "" {
		app.renamePath = filepath.Join(configDir, "rename.yaml")
	}
	return nil
}

func (app *App) loadConfig() error {
	yamlFile, err := os.ReadFile(app.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return app.createDefaultConfig()
		}
		return fmt.Errorf("read config file failed: %w", err)
	}

	if err := yaml.Unmarshal(yamlFile, &config.GlobalConfig); err != nil {
		return fmt.Errorf("parse config file failed: %w", err)
	}

	utils.LogInfo("read config file success")

	info.CountryCodeRegexInit(app.renamePath)

	return nil
}

func (app *App) createDefaultConfig() error {
	utils.LogInfo("config file not found, create default config file")

	if err := os.WriteFile(app.configPath, []byte(config.DefaultConfigTemplate), 0644); err != nil {
		return fmt.Errorf("write default config file failed: %w", err)
	}

	utils.LogInfo("default config file created")
	utils.LogInfo("please edit config file: %v", app.configPath)
	os.Exit(0)
	return nil
}

func (app *App) initConfigWatcher() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create file watcher failed: %w", err)
	}

	app.watcher = watcher
	app.reloadTimer = time.NewTimer(0)
	<-app.reloadTimer.C

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					if app.reloadTimer != nil {
						app.reloadTimer.Stop()
					}
					app.reloadTimer.Reset(100 * time.Millisecond)

					go func() {
						<-app.reloadTimer.C
						utils.LogInfo("config file changed, reloading")
						if err := app.loadConfig(); err != nil {
							utils.LogError("reload config file failed: %v", err)
							return
						}
						app.interval = config.GlobalConfig.Check.Interval
					}()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				utils.LogError("config file watcher error: %v", err)
			}
		}
	}()

	if err := watcher.Add(app.configPath); err != nil {
		return fmt.Errorf("add config file watcher failed: %w", err)
	}

	utils.LogInfo("config file watcher started")
	return nil
}

func (app *App) Run() {
	defer func() {
		app.watcher.Close()
		if app.reloadTimer != nil {
			app.reloadTimer.Stop()
		}
	}()

	for {
		maintask()
		utils.UpdateSubs()
		nextCheck := time.Now().Add(time.Duration(app.interval) * time.Minute)
		utils.LogInfo("next check time: %v", nextCheck.Format("2006-01-02 15:04:05"))
		time.Sleep(time.Duration(app.interval) * time.Minute)
	}
}

func main() {

	app := NewApp()

	if err := app.Initialize(); err != nil {
		utils.LogError("initialize failed: %v", err)
		os.Exit(1)
	}

	app.Run()
}
func maintask() {

	proxies, err := proxy.GetProxies()
	if err != nil {
	}

	utils.LogInfo("get proxies success: %v proxies", len(proxies))

	proxies = info.DeduplicateProxies(proxies)

	utils.LogInfo("deduplicate proxies: %v proxies", len(proxies))

	proxyTasks := make([]interface{}, len(proxies))
	for i, proxy := range proxies {
		proxyTasks[i] = proxy
	}

	pool := utils.NewThreadPool(config.GlobalConfig.Check.Concurrent, proxyAliveTask)
	pool.Start()
	pool.AddTaskArgs(proxyTasks)
	pool.Wait()
	results := pool.GetResults()
	var success int
	var successProxies []info.Proxy
	for _, result := range results {
		if result.Err != nil {
			continue
		}
		proxy := result.Result.(*info.Proxy)
		if proxy.Info.Alive {
			success++
			proxy.Id = success
			successProxies = append(successProxies, *proxy)
		}
	}

	renameTasks := make([]interface{}, len(successProxies))
	for i, proxy := range successProxies {
		renameTasks[i] = proxy
	}
	pool = utils.NewThreadPool(config.GlobalConfig.Check.Concurrent, proxyRenameTask)
	pool.Start()
	pool.AddTaskArgs(renameTasks)
	pool.Wait()
	renameResults := pool.GetResults()
	var renamedProxies []info.Proxy
	for _, result := range renameResults {
		if result.Err != nil {
			continue
		}
		proxy := result.Result.(info.Proxy)
		renamedProxies = append(renamedProxies, proxy)
	}
	utils.LogInfo("check and rename end %v proxies", len(renamedProxies))
	saver.SaveConfig(renamedProxies)
}
func proxyAliveTask(task interface{}) (interface{}, error) {
	proxy := proxy.NewProxy(task.(map[string]any))
	checker := checker.NewChecker(proxy)
	checker.AliveTest("https://gstatic.com/generate_204", 204)
	for _, item := range config.GlobalConfig.Check.Items {
		switch item {
		case "openai":
			checker.OpenaiTest()
		case "youtube":
			checker.YoutubeTest()
		case "netflix":
			checker.NetflixTest()
		case "disney":
			checker.DisneyTest()
		case "speed":
			checker.CheckSpeed()
		}
	}
	return proxy, nil
}
func proxyRenameTask(task interface{}) (interface{}, error) {
	proxy := task.(info.Proxy)
	switch config.GlobalConfig.Rename.Method {
	case "api":
		proxy.CountryCodeFromApi()
	case "regex":
		proxy.CountryCodeRegex()
	case "mix":
		proxy.CountryCodeRegex()
		if proxy.Info.Country == "UN" {
			proxy.CountryCodeFromApi()
		}
	}
	name := fmt.Sprintf("%v %03d", proxy.Info.Country, proxy.Id)
	if config.GlobalConfig.Rename.Flag {
		proxy.CountryFlag()
		name = fmt.Sprintf("%v %v", proxy.Info.Flag, name)
	}

	if utils.Contains(config.GlobalConfig.Check.Items, "speed") {
		speed := proxy.Info.Speed
		var speedStr string
		switch {
		case speed < 1024:
			speedStr = fmt.Sprintf("%d KB/s", speed)
		case speed < 1024*1024:
			speedStr = fmt.Sprintf("%.2f MB/s", float64(speed)/1024)
		default:
			speedStr = fmt.Sprintf("%.2f GB/s", float64(speed)/(1024*1024))
		}
		name = fmt.Sprintf("%v | ⬇️ %s", name, speedStr)
	}

	proxy.Raw["name"] = name
	return proxy, nil
}
func checkConfig() {
	if config.GlobalConfig.Check.Concurrent <= 0 {
		utils.LogError("concurrent must be greater than 0")
		os.Exit(1)
	}
	utils.LogInfo("concurrents: %v", config.GlobalConfig.Check.Concurrent)
	switch config.GlobalConfig.Save.Method {
	case "webdav":
		if config.GlobalConfig.Save.WebDAVURL == "" {
			utils.LogError("webdav-url is required when save-method is webdav")
			os.Exit(1)
		} else {
			utils.LogInfo("save method: webdav")
		}
	case "http":
		if config.GlobalConfig.Save.Port <= 0 {
			utils.LogError("port must be greater than 0 when save-method is http")
			os.Exit(1)
		} else {
			utils.LogInfo("save method: http")
		}
	case "gist":
		if config.GlobalConfig.Save.GithubGistID == "" {
			utils.LogError("github-gist-id is required when save-method is gist")
			os.Exit(1)
		}
		if config.GlobalConfig.Save.GithubToken == "" {
			utils.LogError("github-token is required when save-method is gist")
			os.Exit(1)
		}
		utils.LogInfo("save method: gist")
	}
	if config.GlobalConfig.SubUrls == nil {
		utils.LogError("sub-urls is required")
		os.Exit(1)
	}
	switch config.GlobalConfig.Rename.Method {
	case "api":
		utils.LogInfo("rename method: api")
	case "regex":
		utils.LogInfo("rename method: regex")
	case "mix":
		utils.LogInfo("rename method: mix")
	default:
		utils.LogError("rename-method must be one of api, regex, mix")
		os.Exit(1)
	}
	if config.GlobalConfig.Proxy.Type == "http" {
		utils.LogInfo("proxy type: http")
	} else if config.GlobalConfig.Proxy.Type == "socks" {
		utils.LogInfo("proxy type: socks")
	} else {
		utils.LogInfo("not use proxy")
	}
	utils.LogInfo("progress display: %v", config.GlobalConfig.PrintProgress)
	if config.GlobalConfig.Check.Interval < 10 {
		utils.LogError("check-interval must be greater than 10 minutes")
		os.Exit(1)
	}
	if len(config.GlobalConfig.Check.Items) == 0 {
		utils.LogInfo("check items: none")
	} else {
		utils.LogInfo("check items: %v", config.GlobalConfig.Check.Items)
	}
	if config.GlobalConfig.MihomoApiUrl != "" {
		version, err := utils.GetVersion()
		if err != nil {
			utils.LogError("get version failed: %v", err)
		} else {
			utils.LogInfo("auto update provider: true")
			utils.LogInfo("mihomo version: %v", version)
		}
	}
}
