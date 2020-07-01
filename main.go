package main

import (
	"fmt"
	"net/http"
	"os"
	t "time"

	"strconv"

	"github.com/rancher/go-rancher-metadata/metadata"
	"github.com/rancher/go-rancher/v2"
	"github.com/rancher/log"
	logserver "github.com/rancher/log/server"
	"github.com/rancher/scheduler/events"
	"github.com/rancher/scheduler/resourcewatchers"
	"github.com/rancher/scheduler/scheduler"
	"github.com/urfave/cli"
)

var VERSION = "v0.1.0-dev"
var MaxRetries = 2

func main() {
	logserver.StartServerWithDefaults()
	app := cli.NewApp()
	app.Name = "scheduler"
	app.Version = VERSION
	app.Usage = "An external resource based scheduler for Rancher."
	app.Action = run
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "metadata-address",
			Usage: "The metadata service address",
			Value: "rancher-metadata",
		},
		cli.IntFlag{
			Name:  "health-check-port",
			Usage: "Port to listen on for healthchecks",
			Value: 80,
		},
	}

	app.Run(os.Args)
}

func run(c *cli.Context) error {
	if os.Getenv("RANCHER_DEBUG") == "true" {
		log.SetLevelString("debug")
	}

	sleep := os.Getenv("CATTLE_SCHEDULER_SLEEPTIME")
	time := 1
	if sleep != "" {
		if val, err := strconv.Atoi(sleep); err != nil {
			time = val
		}
	}
	scheduler := scheduler.NewScheduler(time)
	mdClient := metadata.NewClient(fmt.Sprintf("http://%s/2016-07-29", c.String("metadata-address")))
	scheduler.SetMetadataClient(mdClient)

	url := os.Getenv("CATTLE_URL")
	ak := os.Getenv("CATTLE_ACCESS_KEY")
	sk := os.Getenv("CATTLE_SECRET_KEY")
	if url == "" || ak == "" || sk == "" {
		log.Fatalf("Cattle connection environment variables not available. URL: %v, access key %v, secret key redacted.", url, ak)
	}
	apiClient, err := client.NewRancherClient(&client.ClientOpts{
		Timeout:   t.Second * 30,
		Url:       url,
		AccessKey: ak,
		SecretKey: sk,
	})
	if err != nil {
		return err
	}

	exit := make(chan error)
	go func(exit chan<- error) {
		defer func() {
			if r := recover(); r != nil {
				switch e := r.(type) {
				case error:
					log.Fatal(e)
				}
			}
		}()
		var attempt = 1
		for {
			err := events.ConnectToEventStream(url, ak, sk, scheduler)
			if err != nil && attempt > MaxRetries {
				log.Debug("connectToEventStream exceeded max-retry limit with error %s", err.Error())
				break
			} else {
				attempt++
				log.Debugf("make ConnectToEventStream retry %d", attempt)
			}
		}
		exit <- fmt.Errorf("cattle event subscriber exited: %s", err.Error())
	}(exit)

	go func(exit chan<- error) {
		defer func() {
			if r := recover(); r != nil {
				switch e := r.(type) {
				case error:
					log.Fatal(e)
				}
			}
		}()
		var attempt = 1
		for {
			err := resourcewatchers.WatchMetadata(mdClient, scheduler, apiClient)
			if err != nil && attempt > MaxRetries {
				log.Debug("watchMetadata exceeded max-retry limit with error %s", err.Error())
				break
			} else {
				attempt++
				log.Debugf("make watchMetadata retry %d", attempt)
			}
		}
		exit <- fmt.Errorf("metadata watcher exited: %s", err.Error())
	}(exit)

	go func(exit chan<- error) {
		defer func() {
			if r := recover(); r != nil {
				switch e := r.(type) {
				case error:
					log.Fatal(e)
				}
			}
		}()
		var attempt = 1
		for {
			err := startHealthCheck(c.Int("health-check-port"), mdClient)
			if err != nil && attempt > MaxRetries {
				log.Debug("startHealthCheck exceeded max-retry limit with error %s", err.Error())
				break
			} else {
				attempt++
				log.Debugf("make startHealthCheck retry %d", attempt)
			}
		}
		exit <- fmt.Errorf("healthcheck provider died: %s", err.Error())
	}(exit)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				switch e := r.(type) {
				case error:
					log.Fatal(e)
				}
			}
		}()
		for {
			t.Sleep(t.Minute * 3)
			log.Info("Syncing scheduler information with rancher metadata")
			for {
				ok, err := scheduler.UpdateWithMetadata(true)
				if err != nil {
					log.Warnf("Error syncing with metadata: %v", err)
					break
				}

				if !ok {
					log.Infof("Delaying metadata sync by 5 seconds since scheduler is actively handling events.")
					t.Sleep(t.Second * 5)
					continue
				}
				// Sync was completed successfully. Break out of inner loop
				break
			}
		}
	}()

	err = <-exit
	log.Errorf("Exiting scheduler with error: %v", err)
	return err
}

func startHealthCheck(listen int, md metadata.Client) error {
	http.HandleFunc("/healthcheck", func(w http.ResponseWriter, r *http.Request) {
		var errMsg string
		healthy := true
		_, err := md.GetVersion()
		if err != nil {
			healthy = false
			errMsg = fmt.Sprintf("error fetching metadata version: %v", err)
		}
		cattleURL := os.Getenv("CATTLE_URL")
		resp, err := http.Get(cattleURL[:len(cattleURL)-2] + "ping")
		if err != nil {
			errMsg = fmt.Sprintf("%v unable to reach rancher/server: %v", errMsg, err)
			log.Errorf("failed healtcheck: %v", errMsg)
			http.Error(w, "Rancher server is unreachable", http.StatusNotFound)
			return
		}
		defer resp.Body.Close()
		if healthy {
			fmt.Fprint(w, "ok")
		} else {
			log.Errorf("failed healtcheck: %v", errMsg)
			http.Error(w, "Metadata and dns is unreachable", http.StatusNotFound)
		}
	})
	log.Infof("Listening for health checks on 0.0.0.0:%d/healthcheck", listen)
	err := http.ListenAndServe(fmt.Sprintf(":%d", listen), nil)
	return err
}
