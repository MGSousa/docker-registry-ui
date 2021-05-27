package main

import (
	"flag"
	"fmt"
	"github.com/CloudyKit/jet"
	"github.com/jinzhu/configor"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/quiq/docker-registry-ui/events"
	"github.com/quiq/docker-registry-ui/registry"
	"github.com/robfig/cron"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type (
	configData struct {
		ListenAddr            string   `yaml:"listen_addr"`
		BasePath              string   `yaml:"base_path"`
		RegistryURL           string   `yaml:"registry_url"`
		VerifyTLS             bool     `yaml:"verify_tls"`
		Username              string   `yaml:"registry_username"`
		Password              string   `yaml:"registry_password"`
		PasswordFile          string   `yaml:"registry_password_file"`
		EventListenerToken    string   `yaml:"event_listener_token"`
		EventRetentionDays    int      `yaml:"event_retention_days"`
		EventDatabaseDriver   string   `yaml:"event_database_driver"`
		EventDatabaseLocation string   `yaml:"event_database_location"`
		EventDeletionEnabled  bool     `yaml:"event_deletion_enabled"`
		CacheRefreshInterval  uint8    `yaml:"cache_refresh_interval"`
		AnyoneCanDelete       bool     `yaml:"anyone_can_delete"`
		Admins                []string `yaml:"admins"`
		Debug                 bool     `yaml:"debug"`
		PurgeTagsKeepDays     int      `yaml:"purge_tags_keep_days"`
		PurgeTagsKeepCount    int      `yaml:"purge_tags_keep_count"`
		PurgeTagsSchedule     string   `yaml:"purge_tags_schedule"`
	}
	apiClient struct {
		client        *registry.Client
		eventListener *events.EventListener
		config        configData
	}
)

var (
	api apiClient
	u   *url.URL
	err error

	configFile, loggingLevel string
	purgeTags, purgeDryRun   bool
)

func main() {
	flag.StringVar(&configFile, "config-file", "config.yml", "path to the config file")
	flag.StringVar(&loggingLevel, "log-level", "info", "logging level")
	flag.BoolVar(&purgeTags, "purge-tags", false, "purge old tags instead of running api web server")
	flag.BoolVar(&purgeDryRun, "dry-run", false, "dry-run for purging task, does not delete anything")
	flag.Parse()

	if loggingLevel != "info" {
		if level, err := logrus.ParseLevel(loggingLevel); err == nil {
			logrus.SetLevel(level)
		}
	}
	// parse config file
	api.parseConfig()

	// Init registry API client.
	if api.client = registry.NewClient(
		api.config.RegistryURL, api.config.VerifyTLS, api.config.Username, api.config.Password); api.client == nil {
		log.Fatal("cannot initialize API Client or unsupported Auth method")
	}
	// execute initial tasks
	api.execInitialTasks()

	if api.config.EventDatabaseDriver != "sqlite3" && api.config.EventDatabaseDriver != "mysql" {
		log.Fatal("event_database_driver should be either sqlite3 or mysql")
	}
	api.eventListener = events.NewEventListener(
		api.config.EventDatabaseDriver, api.config.EventDatabaseLocation, api.config.EventRetentionDays, api.config.EventDeletionEnabled,
	)
	// init Template and Web engines
	api.initEngine()
}

// parseConfig read and parse config file and respective opts
func (api *apiClient) parseConfig() {
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		log.Fatal(err)
	}
	_ = configor.New(
		&configor.Config{
			AutoReload:         true,
			AutoReloadInterval: time.Second,
			AutoReloadCallback: func(c interface{}) {
				if os.Getenv("APP_RELEASE") != "true" {
					log.Println("Config auto-reloaded")
				}
			},
		},
	).Load(&api.config, configFile)

	// Validate registry URL.
	if u, err = url.Parse(api.config.RegistryURL); err != nil {
		log.Fatal(err)
	}

	// Normalize base path.
	if api.config.BasePath != "" {
		if !strings.HasPrefix(api.config.BasePath, "/") {
			api.config.BasePath = "/" + api.config.BasePath
		}
		if strings.HasSuffix(api.config.BasePath, "/") {
			api.config.BasePath = api.config.BasePath[0 : len(api.config.BasePath)-1]
		}
	}
	// Read password from file.
	if api.config.PasswordFile != "" {
		if _, err := os.Stat(api.config.PasswordFile); os.IsNotExist(err) {
			panic(err)
		}
		passwordBytes, err := ioutil.ReadFile(api.config.PasswordFile)
		if err != nil {
			panic(err)
		}
		api.config.Password = strings.TrimSuffix(string(passwordBytes[:]), "\n")
	}
}

// execInitialTasks execute or schedules initial tasks
func (api *apiClient) execInitialTasks() {
	// Execute CLI task and exit.
	if purgeTags {
		api.purgeOldTags(purgeDryRun)
		return
	}
	// Schedules to purge tags.
	if api.config.PurgeTagsSchedule != "" {
		c := cron.New()
		task := func() {
			api.purgeOldTags(purgeDryRun)
		}
		if err := c.AddFunc(api.config.PurgeTagsSchedule, task); err != nil {
			log.Fatalf("Invalid schedule format: %s", api.config.PurgeTagsSchedule)
		}
		c.Start()
	}
	// Count tags in background.
	go api.client.CountTags(api.config.CacheRefreshInterval)
}

// initEngine initiate template and web engines
func (api *apiClient) initEngine() {
	// init template engine
	e := echo.New()
	e.Renderer = setupRenderer(api.config.Debug, u.Host, api.config.BasePath)

	// init web engine routes
	e.File("/favicon.ico", "static/favicon.ico")
	e.Static(api.config.BasePath+"/static", "static")
	if api.config.BasePath != "" {
		e.GET(api.config.BasePath, api.viewRepositories)
	}
	e.GET(api.config.BasePath+"/", api.viewRepositories)
	e.GET(api.config.BasePath+"/:namespace", api.viewRepositories)
	e.GET(api.config.BasePath+"/:namespace/:repo", api.viewTags)
	e.GET(api.config.BasePath+"/:namespace/:repo/:tag", api.viewTagInfo)
	e.GET(api.config.BasePath+"/:namespace/:repo/:tag/delete", api.deleteTag)
	e.GET(api.config.BasePath+"/events", api.viewLog)

	// protected event listener
	p := e.Group(api.config.BasePath + "/api")
	p.Use(middleware.KeyAuthWithConfig(middleware.KeyAuthConfig{
		Validator: func(token string, c echo.Context) (bool, error) {
			return token == api.config.EventListenerToken, nil
		},
	}))
	p.POST("/events", api.receiveEvents)

	e.Logger.Fatal(e.Start(api.config.ListenAddr))
}

// viewRepositories
func (api *apiClient) viewRepositories(c echo.Context) error {
	namespace := c.Param("namespace")
	if namespace == "" {
		namespace = "library"
	}

	repos, _ := api.client.Repositories(true)[namespace]
	data := jet.VarMap{}
	data.Set("namespace", namespace)
	data.Set("namespaces", api.client.Namespaces())
	data.Set("repos", repos)
	data.Set("tagCounts", api.client.TagCounts())

	return c.Render(http.StatusOK, "repositories.html", data)
}

// viewTags
func (api *apiClient) viewTags(c echo.Context) error {
	namespace := c.Param("namespace")
	repo := c.Param("repo")
	repoPath := repo
	if namespace != "library" {
		repoPath = fmt.Sprintf("%s/%s", namespace, repo)
	}

	tags := api.client.Tags(repoPath)
	deleteAllowed := api.checkDeletePermission(c.Request().Header.Get("X-WEBAUTH-USER"))

	data := jet.VarMap{}
	data.Set("namespace", namespace)
	data.Set("repo", repo)
	data.Set("tags", tags)
	data.Set("deleteAllowed", deleteAllowed)
	repoPath, _ = url.PathUnescape(repoPath)
	data.Set("events", api.eventListener.GetEvents(repoPath))

	return c.Render(http.StatusOK, "tags.html", data)
}

// viewTagInfo view all available info from each tag
func (api *apiClient) viewTagInfo(c echo.Context) error {
	namespace := c.Param("namespace")
	repo := c.Param("repo")
	tag := c.Param("tag")
	repoPath := repo
	if namespace != "library" {
		repoPath = fmt.Sprintf("%s/%s", namespace, repo)
	}

	// Retrieve full image info from various versions of manifests
	sha256, infoV1, infoV2 := api.client.TagInfo(repoPath, tag, false)
	sha256list, manifests := api.client.ManifestList(repoPath, tag)
	if (infoV1 == "" || infoV2 == "") && len(manifests) == 0 {
		return c.Redirect(http.StatusSeeOther, fmt.Sprintf("%s/%s/%s", api.config.BasePath, namespace, repo))
	}

	// check if manifest has sha256 valid string
	created := gjson.Get(gjson.Get(infoV1, "history.0.v1Compatibility").String(), "created").String()
	isDigest := strings.HasPrefix(tag, "sha256:")
	if len(manifests) > 0 {
		sha256 = sha256list
	}

	// Gather layers v2
	var layersV2 []map[string]gjson.Result
	for _, s := range gjson.Get(infoV2, "layers").Array() {
		layersV2 = append(layersV2, s.Map())
	}

	// Gather layers v1
	var layersV1 []map[string]interface{}
	for _, s := range gjson.Get(infoV1, "history.#.v1Compatibility").Array() {
		m, _ := gjson.Parse(s.String()).Value().(map[string]interface{})
		// Sort key in the map to show the ordered on UI.
		m["ordered_keys"] = registry.SortedMapKeys(m)
		layersV1 = append(layersV1, m)
	}

	// Count image size
	var imageSize int64
	if gjson.Get(infoV2, "layers").Exists() {
		for _, s := range gjson.Get(infoV2, "layers.#.size").Array() {
			imageSize = imageSize + s.Int()
		}
	} else {
		for _, s := range gjson.Get(infoV2, "history.#.v1Compatibility").Array() {
			imageSize = imageSize + gjson.Get(s.String(), "Size").Int()
		}
	}

	// Count layers
	layersCount := len(layersV2)
	if layersCount == 0 {
		layersCount = len(gjson.Get(infoV1, "fsLayers").Array())
	}

	// Gather sub-image info of multi-arch or cache image
	var digestList []map[string]interface{}
	for _, s := range manifests {
		r, _ := gjson.Parse(s.String()).Value().(map[string]interface{})
		if s.Get("mediaType").String() == "application/vnd.docker.distribution.manifest.v2+json" {
			// Sub-image of the specific arch.
			_, dInfoV1, _ := api.client.TagInfo(repoPath, s.Get("digest").String(), true)
			var dSize int64
			for _, d := range gjson.Get(dInfoV1, "layers.#.size").Array() {
				dSize = dSize + d.Int()
			}
			r["size"] = dSize
			// Create link here because there is a bug with jet template when referencing a value by map key in the "if" condition under "range".
			if r["mediaType"] == "application/vnd.docker.distribution.manifest.v2+json" {
				r["digest"] = fmt.Sprintf(`<a href="%s/%s/%s/%s">%s</a>`, api.config.BasePath, namespace, repo, r["digest"], r["digest"])
			}
		} else {
			// Sub-image of the cache type.
			r["size"] = s.Get("size").Int()
		}
		r["ordered_keys"] = registry.SortedMapKeys(r)
		digestList = append(digestList, r)
	}

	// Populate template vars
	data := jet.VarMap{}
	data.Set("namespace", namespace)
	data.Set("repo", repo)
	data.Set("tag", tag)
	data.Set("repoPath", repoPath)
	data.Set("sha256", sha256)
	data.Set("imageSize", imageSize)
	data.Set("created", created)
	data.Set("layersCount", layersCount)
	data.Set("layersV2", layersV2)
	data.Set("layersV1", layersV1)
	data.Set("isDigest", isDigest)
	data.Set("digestList", digestList)

	return c.Render(http.StatusOK, "tag_info.html", data)
}

// deleteTag delete desited tag
func (api *apiClient) deleteTag(c echo.Context) error {
	namespace := c.Param("namespace")
	repo := c.Param("repo")
	tag := c.Param("tag")
	repoPath := repo
	if namespace != "library" {
		repoPath = fmt.Sprintf("%s/%s", namespace, repo)
	}

	if api.checkDeletePermission(c.Request().Header.Get("X-WEBAUTH-USER")) {
		api.client.DeleteTag(repoPath, tag)
	}

	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("%s/%s/%s", api.config.BasePath, namespace, repo))
}

// checkDeletePermission check if tag deletion is allowed whether by anyone or permitted users.
func (api *apiClient) checkDeletePermission(user string) bool {
	deleteAllowed := api.config.AnyoneCanDelete
	if !deleteAllowed {
		for _, u := range api.config.Admins {
			if u == user {
				deleteAllowed = true
				break
			}
		}
	}
	return deleteAllowed
}

// viewLog view events from sqlite.
func (api *apiClient) viewLog(c echo.Context) error {
	data := jet.VarMap{}
	data.Set("events", api.eventListener.GetEvents(""))

	return c.Render(http.StatusOK, "event_log.html", data)
}

// receiveEvents receive events.
func (api *apiClient) receiveEvents(c echo.Context) error {
	api.eventListener.ProcessEvents(c.Request())
	return c.String(http.StatusOK, "OK")
}

// purgeOldTags purges old tags.
func (api *apiClient) purgeOldTags(dryRun bool) {
	registry.PurgeOldTags(api.client, dryRun, api.config.PurgeTagsKeepDays, api.config.PurgeTagsKeepCount)
}
