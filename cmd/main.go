package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/virtual-kubelet/virtual-kubelet/log"

	commonIL "github.com/intertwin-eu/interlink-docker-plugin/pkg/common"
	docker "github.com/intertwin-eu/interlink-docker-plugin/pkg/docker"
	"github.com/intertwin-eu/interlink-docker-plugin/pkg/docker/dindmanager"
)

func main() {
	logger := logrus.StandardLogger()

	interLinkConfig, err := commonIL.NewInterLinkConfig()
	if err != nil {
		log.L.Fatal(err)
	}

	if interLinkConfig.VerboseLogging {
		logger.SetLevel(logrus.DebugLevel)
	} else if interLinkConfig.ErrorsOnlyLogging {
		logger.SetLevel(logrus.ErrorLevel)
	} else {
		logger.SetLevel(logrus.InfoLevel)
	}

	Ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log.G(Ctx).Debug("Debug level: " + strconv.FormatBool(interLinkConfig.VerboseLogging))

	availableDinds := os.Getenv("AVAILABLEDINDS")
	if availableDinds == "" {
		availableDinds = "2"
	}
	var dindHandler dindmanager.DindManagerInterface
	dindHandler = &dindmanager.DindManager{
		DindList: []dindmanager.DindSpecs{},
		Ctx:      Ctx,
	}
	availableDindsInt, err := strconv.ParseInt(availableDinds, 10, 8)
	if err != nil {
		log.G(Ctx).Fatal(err)
	}
	dindHandler.CleanDindContainers()
	dindHandler.BuildDindContainers(int8(availableDindsInt))

	SidecarAPIs := docker.SidecarHandler{
		Config:      interLinkConfig,
		Ctx:         Ctx,
		DindManager: dindHandler,
	}

	mutex := http.NewServeMux()
	mutex.HandleFunc("/status", SidecarAPIs.StatusHandler)
	mutex.HandleFunc("/create", SidecarAPIs.CreateHandler)
	mutex.HandleFunc("/delete", SidecarAPIs.DeleteHandler)
	mutex.HandleFunc("/getLogs", SidecarAPIs.GetLogsHandler)

	if strings.HasPrefix(interLinkConfig.Socket, "unix://") {
		// Create a Unix domain socket and listen for incoming connections.
		socket, err := net.Listen("unix", strings.ReplaceAll(interLinkConfig.Socket, "unix://", ""))
		if err != nil {
			panic(err)
		}

		// Cleanup the sockfile.
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-c
			os.Remove(strings.ReplaceAll(interLinkConfig.Socket, "unix://", ""))
			os.Exit(1)
		}()
		server := http.Server{
			Handler: mutex,
		}

		log.G(Ctx).Info(socket)

		if err := server.Serve(socket); err != nil {
			log.G(Ctx).Fatal(err)
		}
	} else {
		err = http.ListenAndServe(":"+interLinkConfig.Sidecarport, mutex)
		if err != nil {
			log.G(Ctx).Fatal(err)
		}
	}
}
