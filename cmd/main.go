package main

import (
	"context"
	"github.com/sirupsen/logrus"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

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
		address := strings.ReplaceAll(interLinkConfig.Socket, "unix://", "")
		socket, err := net.Listen("unix", address)
		if err != nil {
			panic(err)
		}
		defer func() {
			socket.Close()
			log.G(Ctx).Info("Cleaning up socket file" + address)
			os.Remove(address)
		}()

		//// Cleanup the sockfile.
		//c := make(chan os.Signal, 1)
		//signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		//go func() {
		//	<-c
		//	os.Remove(address)
		//	log.G(Ctx).Info("Cleaning up socket file" + address)
		//	os.Exit(1)
		//}()
		server := http.Server{
			Handler: mutex,
		}

		log.G(Ctx).Info(socket)

		log.G(Ctx).Info("Starting to listen on unix socket: " + address)
		if err := server.Serve(socket); err != nil {
			log.G(Ctx).Fatal(err)
		}
		log.G(Ctx).Info("Successfully listening on unix socket: " + address)
	} else {
		err = http.ListenAndServe(":"+interLinkConfig.Sidecarport, mutex)
		if err != nil {
			log.G(Ctx).Fatal(err)
		}
	}
}
