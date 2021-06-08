package main

import (
	"flag"
	"fmt"
	"github.com/CloudyKit/jet"
	"github.com/MGSousa/cron/v3"
	"github.com/MGSousa/docker-registry-ui/events"
	"github.com/MGSousa/docker-registry-ui/registry"
	"github.com/jinzhu/configor"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"io/ioutil"
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
		client             *registry.Client
		eventListener      *events.EventListener
		config             configData
		tpl                jet.VarMap
		cron               *cron.Cron
		purgeJob, countJob cron.EntryID
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
		if level, err := log.ParseLevel(loggingLevel); err == nil {
			log.SetLevel(level)
		}
	}
	// parse config file
	api.parseConfig()

	// Init registry API client.
	if api.client = registry.NewClient(
		api.config.RegistryURL, api.config.VerifyTLS, api.config.Username, api.config.Password); api.client == nil {
		log.Fatal("cannot initialize API Client or unsupported Auth method")
	}

	// Execute task to remove old tags and exit.
	if purgeTags {
		api.purgeOldTags(purgeDryRun)
		return
	}

	// execute initial tasks
	api.execInitialTasks(false)

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

				// update intial tasks schedule when refresh
				api.execInitialTasks(true)
			},
		},
	).Load(&api.config, configFile)

	// init cron runner
	api.cron = cron.New(
		cron.WithChain(cron.Recover(cron.DefaultLogger)))

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
func (api *apiClient) execInitialTasks(update bool) {
	defer api.cron.Start()

	// Schedule to purge tags.
	if api.config.PurgeTagsSchedule != "" {
		if update {
			if err = api.cron.UpdateScheduleWithSpec(
				api.purgeJob, api.config.PurgeTagsSchedule); err != nil {
				log.WithField("task", "purge_tags").
					Fatalf("invalid schedule format: %s", api.config.PurgeTagsSchedule)
			}
		} else {
			if api.purgeJob, err = api.cron.AddFunc(api.config.PurgeTagsSchedule, func() {
				api.purgeOldTags(purgeDryRun)
			}); err != nil {
				log.WithField("task", "purge_tags").
					Fatalf("invalid schedule format: %s", api.config.PurgeTagsSchedule)
			}
		}
	}
	// Schedule to register synced tags
	if api.config.CacheRefreshInterval > 0 {
		if update {
			if err = api.cron.UpdateScheduleWithSpec(
				api.countJob, fmt.Sprintf("*/%d * * * *", api.config.CacheRefreshInterval)); err != nil {
				log.WithField("task", "purge_tags").
					Fatalf("invalid schedule format: %s", api.config.PurgeTagsSchedule)
			}
		} else {
			if api.countJob, err = api.cron.AddFunc(
				fmt.Sprintf("*/%d * * * *", api.config.CacheRefreshInterval), func() {
					api.client.CountTags()
				}); err != nil {
				log.WithField("task", "count_tags").
					Fatalf("invalid schedule format: %s", api.config.PurgeTagsSchedule)
			}
		}
	}
}

// initEngine initiate template and web engines
func (api *apiClient) initEngine() {
	// init template engine
	e := echo.New()
	api.tpl = jet.VarMap{}
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
	api.tpl.Set("namespace", namespace)
	api.tpl.Set("namespaces", api.client.Namespaces())
	api.tpl.Set("repos", repos)
	api.tpl.Set("tagCounts", api.client.TagCounts())

	return c.Render(http.StatusOK, "repositories.html", api.tpl)
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

	api.tpl.Set("namespace", namespace)
	api.tpl.Set("repo", repo)
	api.tpl.Set("tags", tags)
	api.tpl.Set("deleteAllowed", deleteAllowed)
	repoPath, _ = url.PathUnescape(repoPath)
	api.tpl.Set("events", api.eventListener.GetEvents(repoPath))

	return c.Render(http.StatusOK, "tags.html", api.tpl)
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

	if !api.gatherLayers(namespace, repo, repoPath, tag) {
		return c.Redirect(http.StatusSeeOther, fmt.Sprintf("%s/%s/%s", api.config.BasePath, namespace, repo))
	}

	// Populate template vars
	api.tpl.Set("namespace", namespace)
	api.tpl.Set("repo", repo)
	api.tpl.Set("tag", tag)
	api.tpl.Set("repoPath", repoPath)

	return c.Render(http.StatusOK, "tag_info.html", api.tpl)
}

// gatherLayers
func (api *apiClient) gatherLayers(namespace, repo string, repoPath string, tag string) bool {
	var (
		layersV1    []map[string]interface{}
		layersV2    []map[string]gjson.Result
		digestList  []map[string]interface{}
		imageSize   int64
		layersCount int
	)

	// Retrieve full image info from various versions of manifests
	sha256, infoV1, infoV2 := api.client.TagInfo(repoPath, tag, false)
	sha256list, manifests := api.client.ManifestList(repoPath, tag)
	if (infoV1 == "" || infoV2 == "") && len(manifests) == 0 {
		return false
	}

	// check if manifest has sha256 valid string
	created := gjson.Get(gjson.Get(infoV1, "history.0.v1Compatibility").String(), "created").String()
	isDigest := strings.HasPrefix(tag, "sha256:")
	if len(manifests) > 0 {
		sha256 = sha256list
	}
	api.tpl.Set("sha256", sha256)
	api.tpl.Set("isDigest", isDigest)
	api.tpl.Set("created", created)

	// Gather layers v2
	for _, s := range gjson.Get(infoV2, "layers").Array() {
		layersV2 = append(layersV2, s.Map())
	}
	api.tpl.Set("layersV2", layersV2)

	// Gather layers v1
	for _, s := range gjson.Get(infoV1, "history.#.v1Compatibility").Array() {
		m, _ := gjson.Parse(s.String()).Value().(map[string]interface{})
		// Sort key in the map to show the ordered on UI.
		m["ordered_keys"] = registry.SortedMapKeys(m)
		layersV1 = append(layersV1, m)
	}
	api.tpl.Set("layersV1", layersV1)

	// Count image size
	if gjson.Get(infoV2, "layers").Exists() {
		for _, s := range gjson.Get(infoV2, "layers.#.size").Array() {
			imageSize = imageSize + s.Int()
		}
	} else {
		for _, s := range gjson.Get(infoV2, "history.#.v1Compatibility").Array() {
			imageSize = imageSize + gjson.Get(s.String(), "Size").Int()
		}
	}
	api.tpl.Set("imageSize", imageSize)

	// Count layers
	if layersCount = len(layersV2); layersCount == 0 {
		layersCount = len(gjson.Get(infoV1, "fsLayers").Array())
	}
	api.tpl.Set("layersCount", layersCount)

	// Gather sub-image info of multi-arch or cache image
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
				r["digest"] = fmt.Sprintf(
					`<a href="%s/%s/%s/%[4]s">%[4]s</a>`, api.config.BasePath, namespace, repo, r["digest"])
			}
		} else {
			// Sub-image of the cache type.
			r["size"] = s.Get("size").Int()
		}
		r["ordered_keys"] = registry.SortedMapKeys(r)
		digestList = append(digestList, r)
	}
	api.tpl.Set("digestList", digestList)
	return true
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
	api.tpl.Set("events", api.eventListener.GetEvents(""))

	return c.Render(http.StatusOK, "event_log.html", api.tpl)
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
