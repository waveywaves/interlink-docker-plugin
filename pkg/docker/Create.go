package docker

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"

	commonIL "github.com/intertwin-eu/interlink-docker-plugin/pkg/common"
)

// CreateHandler creates a Docker Container based on data provided by the InterLink API.
func (h *SidecarHandler) CreateHandler(w http.ResponseWriter, r *http.Request) {
	log.G(h.Ctx).Info("Docker Sidecar: received Create call")
	var execReturn exec.ExecResult
	statusCode := http.StatusOK
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, nil)
		return
	}

	var req []commonIL.RetrievedPodData
	err = json.Unmarshal(bodyBytes, &req)

	if err != nil {
		HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, nil)
		return
	}

	for _, data := range req {

		podUID := string(data.Pod.UID)
		podNamespace := string(data.Pod.Namespace)

		pathsOfVolumes := make(map[string]string)

		for _, volume := range data.Pod.Spec.Volumes {
			if volume.HostPath != nil {
				if *volume.HostPath.Type == v1.HostPathDirectoryOrCreate || *volume.HostPath.Type == v1.HostPathDirectory {
					_, err := os.Stat(volume.HostPath.Path + "/" + volume.Name)
					if os.IsNotExist(err) {
						log.G(h.Ctx).Info("-- Creating directory " + volume.HostPath.Path + "/" + volume.Name)
						err = os.MkdirAll(volume.HostPath.Path+"/"+volume.Name, os.ModePerm)
						if err != nil {
							HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
						} else {
							log.G(h.Ctx).Info("-- Created directory " + volume.HostPath.Path)
							pathsOfVolumes[volume.Name] = volume.HostPath.Path + "/" + volume.Name
						}
					} else {
						log.G(h.Ctx).Info("-- Directory " + volume.HostPath.Path + "/" + volume.Name + " already exists")
						pathsOfVolumes[volume.Name] = volume.HostPath.Path + "/" + volume.Name
					}
				}
			}
		}

		for _, container := range data.Pod.Spec.Containers {

			containerName := podNamespace + "-" + podUID + "-" + container.Name

			log.G(h.Ctx).Info("-- Preparing environment variables for " + containerName)

			var envVars string = ""
			// add environment variables to the docker command
			for _, envVar := range container.Env {
				if envVar.Value != "" {
					// check if the env variable is an array, in this case the value needs to be between ''
					if strings.Contains(envVar.Value, "[") {
						envVars += " -e " + envVar.Name + "='" + envVar.Value + "'"
					} else {
						envVars += " -e " + envVar.Name + "=" + envVar.Value
					}
				} else {
					envVars += " -e " + envVar.Name
				}
			}

			// iterate over the container volumes and mount them in the docker command line; get the volume path in the host from pathsOfVolumes
			for _, volumeMount := range container.VolumeMounts {
				if volumeMount.MountPath != "" {

					// check if volumeMount.name is inside pathsOfVolumes, if it is add the volume to the docker command
					if _, ok := pathsOfVolumes[volumeMount.Name]; !ok {
						log.G(h.Ctx).Error("Volume " + volumeMount.Name + " not found in pathsOfVolumes")
						continue
					}

					if volumeMount.ReadOnly {
						envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath + ":ro"
					} else {
						envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath
					}
				}
			}

			log.G(h.Ctx).Info("- Creating container " + containerName)

			cmd := []string{"run", "-d", "--name", containerName}

			cmd = append(cmd, envVars)

			var additionalPortArgs []string
			for _, port := range container.Ports {
				if port.HostPort != 0 {
					additionalPortArgs = append(additionalPortArgs, "-p", strconv.Itoa(int(port.HostPort))+":"+strconv.Itoa(int(port.ContainerPort)))
				}
			}

			cmd = append(cmd, additionalPortArgs...)

			if h.Config.ExportPodData {
				mounts, err := prepareMounts(h.Ctx, h.Config, req, container)
				if err != nil {
					HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
					return
				}
				// print the mounts for debugging
				log.G(h.Ctx).Info("Mounts: " + mounts)

				cmd = append(cmd, mounts)
			}

			cmd = append(cmd, container.Image)
			cmd = append(cmd, container.Command...)
			cmd = append(cmd, container.Args...)

			dockerOptions := ""

			if dockerFlags, ok := data.Pod.ObjectMeta.Annotations["docker-options.vk.io/flags"]; ok {
				parsedDockerOptions := strings.Split(dockerFlags, " ")
				for _, option := range parsedDockerOptions {
					dockerOptions += " " + option
				}
			}

			// print the docker command
			log.G(h.Ctx).Info("Docker command: " + "docker" + dockerOptions + " " + strings.Join(cmd, " "))

			shell := exec.ExecTask{
				Command: "docker" + dockerOptions,
				Args:    cmd,
				Shell:   true,
			}

			execReturn, err = shell.Execute()
			if err != nil {
				HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
				return
			}

			if execReturn.Stdout == "" {
				eval := "Conflict. The container name \"/" + containerName + "\" is already in use"
				if strings.Contains(execReturn.Stderr, eval) {
					log.G(h.Ctx).Warning("Container named " + containerName + " already exists. Skipping its creation.")
				} else {
					log.G(h.Ctx).Error("Unable to create container " + containerName + " : " + execReturn.Stderr)
					HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
					return
				}
			} else {
				log.G(h.Ctx).Info("-- Created container " + containerName)
			}

			shell = exec.ExecTask{
				Command: "docker",
				Args:    []string{"ps", "-aqf", "name=^" + containerName + "$"},
				Shell:   true,
			}

			execReturn, err = shell.Execute()
			execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")
			if execReturn.Stderr != "" {
				log.G(h.Ctx).Error("Failed to retrieve " + containerName + " ID : " + execReturn.Stderr)
				HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
				return
			} else if execReturn.Stdout == "" {
				log.G(h.Ctx).Error("Container name not found. Maybe creation failed?")
			} else {
				log.G(h.Ctx).Debug("-- Retrieved " + containerName + " ID: " + execReturn.Stdout)
			}
		}
	}

	w.WriteHeader(statusCode)

	if statusCode != http.StatusOK {
		w.Write([]byte("Some errors occurred while creating containers. Check Docker Sidecar's logs"))
	} else {
		w.Write([]byte("Containers created"))
	}
}

func HandleErrorAndRemoveData(h *SidecarHandler, w http.ResponseWriter, statusCode int, s string, err error, data *commonIL.RetrievedPodData) {
	statusCode = http.StatusInternalServerError
	log.G(h.Ctx).Error(err)
	w.WriteHeader(statusCode)
	w.Write([]byte("Some errors occurred while creating container. Check Docker Sidecar's logs"))

	if data != nil {
		os.RemoveAll(h.Config.DataRootFolder + data.Pod.Namespace + "-" + string(data.Pod.UID))
	}
}
