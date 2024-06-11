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

	"time"

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
					_, err := os.Stat(volume.HostPath.Path)
					if *volume.HostPath.Type == v1.HostPathDirectory {
						if os.IsNotExist(err) {
							HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
							return
						}
						pathsOfVolumes[volume.Name] = volume.HostPath.Path
					} else if *volume.HostPath.Type == v1.HostPathDirectoryOrCreate {
						if os.IsNotExist(err) {
							err = os.MkdirAll(volume.HostPath.Path, os.ModePerm)
							if err != nil {
								HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
								return
							} else {
								pathsOfVolumes[volume.Name] = volume.HostPath.Path
							}
						} else {
							pathsOfVolumes[volume.Name] = volume.HostPath.Path
						}
					}
				}
			}

			if volume.PersistentVolumeClaim != nil {
				if _, ok := pathsOfVolumes[volume.PersistentVolumeClaim.ClaimName]; !ok {
					// WIP: This is a temporary solution to mount CVMFS volumes
					pathsOfVolumes[volume.PersistentVolumeClaim.ClaimName] = "/mnt/cvmfs"
				}

			}
		}

		allContainers := map[string][]v1.Container{
			"initContainers": data.Pod.Spec.InitContainers,
			"containers":     data.Pod.Spec.Containers,
		}

		// iterate over all containers
		for containerType, containers := range allContainers {
			isInitContainer := containerType == "initContainers"

			for _, container := range containers {

				containerName := podNamespace + "-" + podUID + "-" + container.Name

				var isGpuRequested bool = false
				var additionalGpuArgs []string

				if val, ok := container.Resources.Limits["nvidia.com/gpu"]; ok {

					numGpusRequested := val.Value()

					// if the container is requesting 0 GPU, skip the GPU assignment
					if numGpusRequested == 0 {
						log.G(h.Ctx).Info("Container " + containerName + " is not requesting a GPU")
					} else {

						log.G(h.Ctx).Info("Container " + containerName + " is requesting " + val.String() + " GPU")

						isGpuRequested = true

						numGpusRequestedInt := int(numGpusRequested)
						_, err := h.GpuManager.GetAvailableGPUs(numGpusRequestedInt)

						if err != nil {
							HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
							return
						}

						gpuSpecs, err := h.GpuManager.GetAndAssignAvailableGPUs(numGpusRequestedInt, containerName)
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
					}

				} else {
					log.G(h.Ctx).Info("Container " + containerName + " is not requesting a GPU")
				}

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
							// if it is Bidirectional, add :shared to the volume
							if *volumeMount.MountPropagation == v1.MountPropagationBidirectional {
								envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath + ":shared"
							} else {
								envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath
							}
						}
					}
				}

				//docker run --privileged -v /home:/home -d --name demo1 docker:dind

				log.G(h.Ctx).Info("- Creating container " + containerName)

				cmd := []string{"run", "-d", "--name", containerName}

				cmd = append(cmd, envVars)

				if container.SecurityContext != nil && container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
					cmd = append(cmd, "--privileged")
					//cmd = append(cmd, "--cap-add=SYS_ADMIN")
					//cmd = append(cmd, "--device=/dev/fuse")
					//cmd = append(cmd, "--security-opt=apparmor:unconfined")
				}

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

				//if h.Config.ExportPodData {
				mounts, err := prepareMounts(h.Ctx, h.Config, req, container)
				if err != nil {
					HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
					return
				}
				// print the mounts for debugging
				log.G(h.Ctx).Info("Mounts: " + mounts)

				cmd = append(cmd, mounts)
				//}

				memoryLimitsArray := []string{}
				cpuLimitsArray := []string{}

				if container.Resources.Limits.Memory().Value() != 0 {
					memoryLimitsArray = append(memoryLimitsArray, "--memory", strconv.Itoa(int(container.Resources.Limits.Memory().Value()))+"b")
				}
				if container.Resources.Limits.Cpu().Value() != 0 {
					cpuLimitsArray = append(cpuLimitsArray, "--cpus", strconv.FormatFloat(float64(container.Resources.Limits.Cpu().Value()), 'f', -1, 64))
				}

				cmd = append(cmd, memoryLimitsArray...)
				cmd = append(cmd, cpuLimitsArray...)

				containerCommands := []string{}
				containerArgs := []string{}
				mountFileCommand := []string{}

				// if container has a command and args, call parseContainerCommandAndReturnArgs
				if len(container.Command) > 0 || len(container.Args) > 0 {
					log.G(h.Ctx).Info("Container has command and args defined. Parsing...")
					log.G(h.Ctx).Info("Container command: " + strings.Join(container.Command, " "))
					log.G(h.Ctx).Info("Container args: " + strings.Join(container.Args, " "))

					mountFileCommand, containerCommands, containerArgs, err = parseContainerCommandAndReturnArgs(h.Ctx, h.Config, req, container)
					if err != nil {
						HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
						return
					}
					cmd = append(cmd, mountFileCommand...)
				}

				// log container commands and args
				log.G(h.Ctx).Info("Container commands: " + strings.Join(containerCommands, " "))
				log.G(h.Ctx).Info("Container args: " + strings.Join(containerArgs, " "))

				cmd = append(cmd, container.Image)
				cmd = append(cmd, containerCommands...)
				cmd = append(cmd, containerArgs...)

				dockerOptions := ""

				if dockerFlags, ok := data.Pod.ObjectMeta.Annotations["docker-options.vk.io/flags"]; ok {
					parsedDockerOptions := strings.Split(dockerFlags, " ")
					for _, option := range parsedDockerOptions {
						dockerOptions += " " + option
					}
				}

				shell := exec.ExecTask{
					Command: "docker" + dockerOptions,
					Args:    cmd,
					Shell:   true,
				}

				log.G(h.Ctx).Info("Docker command: " + strings.Join(shell.Args, " "))

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
					containerID := execReturn.Stdout

					log.G(h.Ctx).Debug("-- Retrieved " + containerName + " ID: " + execReturn.Stdout)

					if isInitContainer {
						log.G(h.Ctx).Info("Waiting for Init Container " + containerName + " to complete")

						// Poll the container status until it exits
						for {
							statusShell := exec.ExecTask{
								Command: "docker",
								Args:    []string{"inspect", "--format='{{.State.Status}}'", containerID},
								Shell:   true,
							}

							statusReturn, err := statusShell.Execute()
							if err != nil {
								log.G(h.Ctx).Error("Failed to inspect Init Container " + containerName + " : " + err.Error())
								HandleErrorAndRemoveData(h, w, statusCode, "Some errors occurred while inspecting container. Check Docker Sidecar's logs", err, &data)
								return
							}

							status := strings.Trim(statusReturn.Stdout, "'\n")
							if status == "exited" {
								log.G(h.Ctx).Info("Init Container " + containerName + " has completed")
								break
							} else {
								time.Sleep(1 * time.Second) // Wait for a second before polling again
							}
						}
					}
				}

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
