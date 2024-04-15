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

	commonIL "github.com/intertwin-eu/interlink/pkg/common"
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

			var isGpuRequested bool = false
			var additionalGpuArgs []string

			if val, ok := container.Resources.Limits["nvidia.com/gpu"]; ok {

				numGpusRequested := val.Value()

				log.G(h.Ctx).Infof("Number of GPU requested: %d", numGpusRequested)

				isGpuRequested = true

				log.G(h.Ctx).Info("Container " + container.Name + " is requesting " + val.String() + " GPU")

				numGpusRequestedInt := int(numGpusRequested)
				_, err := h.GpuManager.GetAvailableGPUs(numGpusRequestedInt)

				if err != nil {
					HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
					return
				}

				gpuSpecs, err := h.GpuManager.GetAndAssignAvailableGPUs(numGpusRequestedInt, container.Name)
				if err != nil {
					HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
					return
				}

				var gpuUUIDs string = ""
				for _, gpuSpec := range gpuSpecs {
					if gpuSpec.UUID == gpuSpecs[len(gpuSpecs)-1].UUID {
						gpuUUIDs += strconv.Itoa(gpuSpec.Index)
					} else {
						gpuUUIDs += strconv.Itoa(gpuSpec.Index) + ","
					}
				}

				additionalGpuArgs = append(additionalGpuArgs, "--runtime=nvidia -e NVIDIA_VISIBLE_DEVICES="+gpuUUIDs)

			} else {
				log.G(h.Ctx).Info("Container " + container.Name + " is not requesting a GPU")
			}

			log.G(h.Ctx).Info("- Creating container " + container.Name)

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

			log.G(h.Ctx).Info("- Creating container " + container.Name)

			cmd := []string{"run", "-d", "--name", container.Name}

			cmd = append(cmd, envVars)

			if isGpuRequested {
				cmd = append(cmd, additionalGpuArgs...)
			}

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
				eval := "Conflict. The container name \"/" + container.Name + "\" is already in use"
				if strings.Contains(execReturn.Stderr, eval) {
					log.G(h.Ctx).Warning("Container named " + container.Name + " already exists. Skipping its creation.")
				} else {
					log.G(h.Ctx).Error("Unable to create container " + container.Name + " : " + execReturn.Stderr)
					HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
					return
				}
			} else {
				log.G(h.Ctx).Info("-- Created container " + container.Name)
			}

			shell = exec.ExecTask{
				Command: "docker",
				Args:    []string{"ps", "-aqf", "name=^" + container.Name + "$"},
				Shell:   true,
			}

			execReturn, err = shell.Execute()
			execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")
			if execReturn.Stderr != "" {
				log.G(h.Ctx).Error("Failed to retrieve " + container.Name + " ID : " + execReturn.Stderr)
				HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
				return
			} else if execReturn.Stdout == "" {
				log.G(h.Ctx).Error("Container name not found. Maybe creation failed?")
			} else {
				log.G(h.Ctx).Debug("-- Retrieved " + container.Name + " ID: " + execReturn.Stdout)
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
