package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/DRuggeri/labwatch/watchers/loki"
	"github.com/DRuggeri/labwatch/watchers/talos"
	"github.com/alecthomas/kingpin/v2"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"

	_ "net/http/pprof"
)

var (
	Version  = "testing"
	logLevel = kingpin.Flag("log-level", "Log Level (one of debug|info|warn|error)").Short('l').Envar("LABWATCH_LOGLEVEL").String()
	config   = kingpin.Flag("config", "Configuration file path").Short('c').Envar("LABWATCH_CONFIG").ExistingFile()
)

type LabwatchConfig struct {
	LokiAddress      string `yaml:"loki-address"`
	LokiQuery        string `yaml:"loki-query"`
	TalosConfigFile  string `yaml:"talos-config"`
	TalosClusterName string `yaml:"talos-cluster"`
}
type LabStatus struct {
	Talos map[string]talos.NodeStatus `json:"talos"`
	Logs  loki.LogStats               `json:"logs"`
}

var currentStatus = LabStatus{}
var statusClients = map[string]chan<- LabStatus{}
var eventClients = map[string]chan<- loki.LogEvent{}
var lock = &sync.Mutex{}

func main() {
	kingpin.Version(Version)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	switch *logLevel {
	case "error":
		opts.Level = slog.LevelError
	case "warn":
		opts.Level = slog.LevelWarn
	case "info":
		opts.Level = slog.LevelInfo
	case "debug":
		opts.Level = slog.LevelDebug
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, opts)).With("operation", "main")
	log.Info("starting up labwatch", "version", Version)

	cfg := LabwatchConfig{
		LokiAddress:      "boss.local:3100",
		LokiQuery:        `{ host_name =~ ".+" } | json`,
		TalosConfigFile:  "/home/boss/talos/talosconfig",
		TalosClusterName: "koobs",
	}

	if *config != "" {
		d, err := os.ReadFile(*config)
		if err != nil {
			log.Error("failed to read provided config file", "error", err.Error())
			os.Exit(1)
		}
		err = yaml.Unmarshal(d, &cfg)
		if err != nil {
			log.Error("failed to parse provided config file", "error", err.Error())
			os.Exit(1)
		}
	}

	err := startWatchers(cfg, log)
	if err != nil {
		log.Error("failed to start watchers", "error", err.Error())
		os.Exit(1)
	}

	log.Info("watchers initialized")

	u := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			b, _ := json.Marshal(currentStatus)
			w.Write(b)
			return
		}

		conn, err := u.Upgrade(w, r, nil)
		if err != nil {
			slog.Info("upgrade failed", "error", err.Error())
			return
		}

		thisChan := make(chan LabStatus)
		uuid := uuid.New().String()

		addStatusClient(uuid, thisChan)
		defer removeStatusClient(uuid)

		data, _ := json.Marshal(currentStatus)
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			slog.Info("write failed", "error", err.Error())
			return
		}

		for {
			var status LabStatus
			select {
			case <-r.Context().Done():
				return
			case status = <-thisChan:
			}
			data, _ := json.Marshal(status)
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		}
	})

	http.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		conn, err := u.Upgrade(w, r, nil)
		if err != nil {
			slog.Info("upgrade failed", "error", err.Error())
			return
		}

		thisChan := make(chan loki.LogEvent)
		uuid := uuid.New().String()

		addEventClient(uuid, thisChan)
		defer removeEventClient(uuid)

		for {
			select {
			case <-r.Context().Done():
				return
			case e := <-thisChan:
				data, _ := json.Marshal(e)
				if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
					return
				}
			}
		}
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "websockets.html")
	})

	err = http.ListenAndServe(":8080", nil)
	log.With("operation", "main", "error", err.Error()).Info("shutting down")
}

func startWatchers(cfg LabwatchConfig, log *slog.Logger) error {
	log = log.With("operation", "startWatchers")
	status := LabStatus{}

	tWatcher, err := talos.NewTalosWatcher(context.Background(), cfg.TalosConfigFile, cfg.TalosClusterName, log)
	if err != nil {
		return err
	}
	tInfo := make(chan map[string]talos.NodeStatus)
	go tWatcher.Watch(context.Background(), tInfo)

	lWatcher, err := loki.NewLokiWatcher(context.Background(), cfg.LokiAddress, cfg.LokiQuery, log)
	if err != nil {
		return err
	}
	events := make(chan loki.LogEvent)
	stats := make(chan loki.LogStats)
	go lWatcher.Watch(context.Background(), events, stats)

	log = log.With("operation", "watchloop")
	go func() {
		for {
			broadcastStatusUpdate := false
			select {
			case t, ok := <-tInfo:
				if ok {
					status.Talos = t
					broadcastStatusUpdate = true
				} else {
					log.Error("error encountered reading talos states")
				}
			case s, ok := <-stats:
				if ok {
					status.Logs = s
					broadcastStatusUpdate = true
				} else {
					log.Error("error encountered reading log stats")
				}
			case e, ok := <-events:
				if ok {
					log.Debug("broadcasting event", "clients", len(eventClients))
					for _, ch := range eventClients {
						ch <- e
					}
				} else {
					log.Error("error encountered reading ")
				}
			default:
				time.Sleep(time.Millisecond * 100)
				continue
			}

			if broadcastStatusUpdate {
				currentStatus = status
				log.Debug("broadcasting status", "clients", len(statusClients))
				for _, ch := range statusClients {
					ch <- status
				}
			}
		}
	}()

	return nil
}

func addStatusClient(id string, ch chan<- LabStatus) {
	lock.Lock()
	statusClients[id] = ch
	lock.Unlock()
}

func removeStatusClient(id string) {
	lock.Lock()
	delete(statusClients, id)
	lock.Unlock()
}

func addEventClient(id string, ch chan<- loki.LogEvent) {
	lock.Lock()
	eventClients[id] = ch
	lock.Unlock()
}

func removeEventClient(id string) {
	lock.Lock()
	delete(eventClients, id)
	lock.Unlock()
}
