package docker

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"

	"errors"

	commonIL "github.com/intertwin-eu/interlink-docker-plugin/pkg/common"

	"path/filepath"
)

type DockerRunStruct struct {
	Name            string `json:"name"`
	Command         string `json:"command"`
	IsInitContainer bool   `json:"isInitContainer"`
}

// prepareDockerRuns functions return an array of DockerRunStruct objects or error, which are used to create the Docker containers
// take as argument the a commonIL.RetrievedPodData object

func (h *SidecarHandler) prepareDockerRuns(podData commonIL.RetrievedPodData, w http.ResponseWriter) ([]DockerRunStruct, error) {

	// initialize an empy DockerRunStruct array
	var dockerRunStructs []DockerRunStruct

	//for _, data := range podData {

	podUID := string(podData.Pod.UID)
	podNamespace := string(podData.Pod.Namespace)

	pathsOfVolumes := make(map[string]string)

	for _, volume := range podData.Pod.Spec.Volumes {
		if volume.HostPath != nil {
			if *volume.HostPath.Type == v1.HostPathDirectoryOrCreate || *volume.HostPath.Type == v1.HostPathDirectory {
				_, err := os.Stat(volume.HostPath.Path)
				if *volume.HostPath.Type == v1.HostPathDirectory {
					if os.IsNotExist(err) {
						HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, podNamespace, podUID)
						return dockerRunStructs, errors.New("Some errors occurred while creating container. Check Docker Sidecar's logs")
					}
					pathsOfVolumes[volume.Name] = volume.HostPath.Path
				} else if *volume.HostPath.Type == v1.HostPathDirectoryOrCreate {
					if os.IsNotExist(err) {
						err = os.MkdirAll(volume.HostPath.Path, os.ModePerm)
						if err != nil {
							HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, podNamespace, podUID)
							return dockerRunStructs, errors.New("Some errors occurred while creating container. Check Docker Sidecar's logs")
						} else {
							pathsOfVolumes[volume.Name] = volume.HostPath.Path
						}
					} else {
						pathsOfVolumes[volume.Name] = volume.HostPath.Path
					}
				}
			}
		}

		// check if inside data.Pod.spec.Volumes is requesting a PVC
		if volume.PersistentVolumeClaim != nil {
			// check if volume.PersistentVolumeClaim.ClaimName is already in pathsOfVolumes, if it is skip the PVC, otherwise add it to pathsOfVolumes with value ""
			if _, ok := pathsOfVolumes[volume.PersistentVolumeClaim.ClaimName]; !ok {
				// check the storage class of the PVC, if it is cvmfs, mount the cvmfs volume
				pathsOfVolumes[volume.PersistentVolumeClaim.ClaimName] = "/mnt/cvmfs"
			}

		}
	}

	// define all containers, that is an object with two keys: 'initContainers' and 'containers'. The value of each key is an array of containers
	allContainers := map[string][]v1.Container{
		"initContainers": podData.Pod.Spec.InitContainers,
		"containers":     podData.Pod.Spec.Containers,
	}

	for containerType, containers := range allContainers {
		isInitContainer := containerType == "initContainers"

		for _, container := range containers {

			containerName := podNamespace + "-" + podUID + "-" + container.Name

			var isGpuRequested bool = false
			var additionalGpuArgs []string

			if val, ok := container.Resources.Limits["nvidia.com/gpu"]; ok {

				numGpusRequested := val.Value()

				numGpusRequested = 0
				// if the container is requesting 0 GPU, skip the GPU assignment
				if numGpusRequested == 0 {
					log.G(h.Ctx).Info("Container " + containerName + " is not requesting a GPU")
				} else {

					log.G(h.Ctx).Info("Container " + containerName + " is requesting " + val.String() + " GPU")

					isGpuRequested = true

					numGpusRequestedInt := int(numGpusRequested)
					_, err := h.GpuManager.GetAvailableGPUs(numGpusRequestedInt)

					if err != nil {
						HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, podNamespace, podUID)
						return dockerRunStructs, errors.New("Some errors occurred while creating container. Check Docker Sidecar's logs")
					}

					gpuSpecs, err := h.GpuManager.GetAndAssignAvailableGPUs(numGpusRequestedInt, containerName)
					if err != nil {
						HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, podNamespace, podUID)
						return dockerRunStructs, errors.New("Some errors occurred while creating container. Check Docker Sidecar's logs")
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
						if volumeMount.MountPropagation != nil && *volumeMount.MountPropagation == v1.MountPropagationBidirectional {
							envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath + ":shared"
						} else {
							envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath
						}
					}
				}
			}

			// add --network=host to the docker command
			envVars += " --network=host"

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
				log.G(h.Ctx).Info("Container " + containerName + " is requesting port " + strconv.Itoa(int(port.ContainerPort)) + " to be exposed on host port " + strconv.Itoa(int(port.HostPort)))
				if port.HostPort != 0 {
					additionalPortArgs = append(additionalPortArgs, "-p", strconv.Itoa(int(port.HostPort))+":"+strconv.Itoa(int(port.ContainerPort)))
				}
			}

			cmd = append(cmd, additionalPortArgs...)

			//if h.Config.ExportPodData {
			mounts, err := prepareMounts(h.Ctx, h.Config, podData, container)
			if err != nil {
				HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, podNamespace, podUID)
				return dockerRunStructs, errors.New("Some errors occurred while creating container. Check Docker Sidecar's logs")
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

				mountFileCommand, containerCommands, containerArgs, err = parseContainerCommandAndReturnArgs(h.Ctx, h.Config, podUID, podNamespace, container)
				if err != nil {
					HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, podNamespace, podUID)
					return dockerRunStructs, errors.New("Some errors occurred while creating container. Check Docker Sidecar's logs")
				}
				cmd = append(cmd, mountFileCommand...)
			}

			cmd = append(cmd, container.Image)
			cmd = append(cmd, containerCommands...)
			cmd = append(cmd, containerArgs...)

			dockerOptions := ""

			if dockerFlags, ok := podData.Pod.ObjectMeta.Annotations["docker-options.vk.io/flags"]; ok {
				parsedDockerOptions := strings.Split(dockerFlags, " ")
				for _, option := range parsedDockerOptions {
					dockerOptions += " " + option
				}
			}

			// run docker run --privileged -v /home:/home -d --name PODUID_dind docker:dind

			shell := exec.ExecTask{
				Command: "docker" + dockerOptions,
				Args:    cmd,
				Shell:   true,
			}

			//log.G(h.Ctx).Info("Docker command: " + strings.Join(shell.Args, " "))

			dockerRunStructs = append(dockerRunStructs, DockerRunStruct{
				Name:            containerName,
				Command:         "docker " + strings.Join(shell.Args, " "),
				IsInitContainer: isInitContainer,
			})
		}
		//}
	}

	return dockerRunStructs, nil
}

// CreateHandler creates a Docker Container based on data provided by the InterLink API.
func (h *SidecarHandler) CreateHandler(w http.ResponseWriter, r *http.Request) {
	log.G(h.Ctx).Info("Docker Sidecar: received Create call")
	var execReturn exec.ExecResult
	statusCode := http.StatusOK

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, "", "")
		return
	}

	var req []commonIL.RetrievedPodData
	err = json.Unmarshal(bodyBytes, &req)

	if err != nil {
		HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, "", "")
		return
	}

	wd, err := os.Getwd()
	if err != nil {
		HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, "", "")
		return
	}

	for _, data := range req {

		podUID := string(data.Pod.UID)
		podNamespace := string(data.Pod.Namespace)

		podDirectoryPath := filepath.Join(wd, h.Config.DataRootFolder+podNamespace+"-"+podUID)

		log.G(h.Ctx).Info("POD directory path is: " + podDirectoryPath)

		// call prepareDockerRuns to get the DockerRunStruct array
		dockerRunStructs, err := h.prepareDockerRuns(data, w)
		if err != nil {
			HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, "", "")
			return
		}

		log.G(h.Ctx).Info("DockerRunStructs: ", dockerRunStructs)

		// from dockerRunStructs, create two arrays: one for initContainers and one for containers
		var initContainers []DockerRunStruct
		var containers []DockerRunStruct

		for _, dockerRunStruct := range dockerRunStructs {
			if dockerRunStruct.IsInitContainer {
				initContainers = append(initContainers, dockerRunStruct)
			} else {
				containers = append(containers, dockerRunStruct)
			}
		}

		var dindContainerID string
		shell := exec.ExecTask{
			Command: "docker",
			Args:    []string{"run", "--privileged", "-v", "/home:/home", "-v", "/mnt/docker-data/:/var/lib/docker", "-d", "--name", string(data.Pod.UID) + "_dind", "docker:dind"},
		}

		// log the command to exec
		log.G(h.Ctx).Info("Executing command: docker " + strings.Join(shell.Args, " "))

		execReturn, err = shell.Execute()
		if err != nil {
			HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, "", "")
			return
		}
		dindContainerID = execReturn.Stdout
		log.G(h.Ctx).Info("Docker dind container ID: " + dindContainerID)

		// wait until the dind container is up and running by check that the command docker ps inside of it does not return an error
		for {
			shell = exec.ExecTask{
				Command: "docker",
				Args:    []string{"exec", string(data.Pod.UID) + "_dind", "docker", "run", "--rm", "ubuntu:latest"},
			}

			execReturn, err = shell.Execute()
			if err != nil {
				log.G(h.Ctx).Error("Failed to check if dind container is up and running: " + err.Error())
				time.Sleep(1 * time.Second) // Wait for a second before polling again
			}

			if execReturn.ExitCode == 0 {
				log.G(h.Ctx).Info("Docker dind container is up and running")
				break
			} else {
				log.G(h.Ctx).Info("Docker dind container is not up and running")
				log.G(h.Ctx).Info("execReturn.ExitCode ", execReturn.ExitCode)
				log.G(h.Ctx).Info("execReturn.Stderr ", execReturn.Stderr)
				time.Sleep(1 * time.Second) // Wait for a second before polling again
			}
		}

		if len(initContainers) > 0 {

			initContainersCommand := "#!/bin/sh\n"
			for _, initContainer := range initContainers {
				initContainersCommand += initContainer.Command + "\n"
			}
			err = os.WriteFile(podDirectoryPath+"/init_containers_command.sh", []byte(initContainersCommand), 0644)
			if err != nil {
				HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, "", "")
				return
			}

			// exec docker exec -it PODUID_dind /bin/sh /init_containers_command.sh
			shell = exec.ExecTask{
				Command: "docker",
				Args:    []string{"exec", string(data.Pod.UID) + "_dind", "/bin/sh", podDirectoryPath + "/init_containers_command.sh"},
			}

			log.G(h.Ctx).Info("Executing command: docker " + strings.Join(shell.Args, " "))

			_, err := shell.Execute()
			if err != nil {
				HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, "", "")
				return
			}
			// Poll the container status until it exits
			for {

				// initialize bool variable to check if all initContainers have completed
				allInitContainersCompleted := false
				initContainersCompleted := 0

				// iterate over initContainers and check if they have completed
				for _, initContainer := range initContainers {

					// query the dind container with docker exec dindContainerID docker "inspect", "--format='{{.State.Status}}'", containerName
					shell = exec.ExecTask{
						Command: "docker",
						Args:    []string{"exec", string(data.Pod.UID) + "_dind", "docker", "inspect", "--format='{{.State.Status}}'", initContainer.Name},
					}

					statusReturn, err := shell.Execute()
					if err != nil {
						log.G(h.Ctx).Error("Failed to inspect Init Container " + initContainer.Name + " : " + err.Error())
						HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, "", "")
						return
					}

					status := strings.Trim(statusReturn.Stdout, "'\n")
					if status == "exited" {
						log.G(h.Ctx).Info("Init Container " + initContainer.Name + " has completed")
						initContainersCompleted += 1
					} else {
						time.Sleep(1 * time.Second) // Wait for a second before polling again
					}
				}
				if initContainersCompleted == len(initContainers) {
					allInitContainersCompleted = true
				}

				if allInitContainersCompleted {
					break
				}
			}
		}

		// create a file called containers_command.sh and write the containers commands to it, use WriteFile function
		containersCommand := "#!/bin/sh\n"
		for _, container := range containers {
			containersCommand += container.Command + "\n"
		}
		err = os.WriteFile(podDirectoryPath+"/containers_command.sh", []byte(containersCommand), 0644)
		if err != nil {
			HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, "", "")
			return
		}

		// exec docker exec -it PODUID_dind /bin/sh /containers_command.sh
		shell = exec.ExecTask{
			Command: "docker",
			Args:    []string{"exec", string(data.Pod.UID) + "_dind", "/bin/sh", podDirectoryPath + "/containers_command.sh"},
		}

		// log the command to exec
		log.G(h.Ctx).Info("Executing command: " + strings.Join(shell.Args, " "))

		// exec the command and log the output
		execReturn, err = shell.Execute()
		if err != nil {
			HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, "", "")
			return
		}

		// log the output of the command
		log.G(h.Ctx).Info("error of the command: " + execReturn.Stderr)
		log.G(h.Ctx).Info("output of the command: " + execReturn.Stdout)
		log.G(h.Ctx).Info("exit code of the command: " + strconv.Itoa(execReturn.ExitCode))
		log.G(h.Ctx).Info("Started all other containers")

		w.WriteHeader(statusCode)

		if statusCode != http.StatusOK {
			w.Write([]byte("Some errors occurred while creating containers. Check Docker Sidecar's logs"))
		} else {
			w.Write([]byte("Containers created"))
		}
	}

	// initialize the docker_commands_to_exec variable as an empty array of strings
	//docker_commands_to_exec := []string{}

	/* for _, data := range req {

		podUID := string(data.Pod.UID)
		podNamespace := string(data.Pod.Namespace)

		pathsOfVolumes := make(map[string]string)

		for _, volume := range data.Pod.Spec.Volumes {
			if volume.HostPath != nil {
				if *volume.HostPath.Type == v1.HostPathDirectoryOrCreate || *volume.HostPath.Type == v1.HostPathDirectory {
					_, err := os.Stat(volume.HostPath.Path)
					if *volume.HostPath.Type == v1.HostPathDirectory {
						if os.IsNotExist(err) {
							HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
							return
						}
						pathsOfVolumes[volume.Name] = volume.HostPath.Path
					} else if *volume.HostPath.Type == v1.HostPathDirectoryOrCreate {
						if os.IsNotExist(err) {
							err = os.MkdirAll(volume.HostPath.Path, os.ModePerm)
							if err != nil {
								HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
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

			// check if inside data.Pod.spec.Volumes is requesting a PVC
			if volume.PersistentVolumeClaim != nil {
				// check if volume.PersistentVolumeClaim.ClaimName is already in pathsOfVolumes, if it is skip the PVC, otherwise add it to pathsOfVolumes with value ""
				if _, ok := pathsOfVolumes[volume.PersistentVolumeClaim.ClaimName]; !ok {
					// check the storage class of the PVC, if it is cvmfs, mount the cvmfs volume
					pathsOfVolumes[volume.PersistentVolumeClaim.ClaimName] = "/mnt/cvmfs"
				}

			}
		}

		// define all containers, that is an object with two keys: 'initContainers' and 'containers'. The value of each key is an array of containers
		allContainers := map[string][]v1.Container{
			"initContainers": data.Pod.Spec.InitContainers,
			"containers":     data.Pod.Spec.Containers,
		}

		// // if allContainers is greater than 0, create a network for the pod
		// if len(allContainers) > 0 {
		// 	// create a network for the pod
		// 	shell := exec.ExecTask{
		// 		Command: "docker",
		// 		Args:    []string{"network", "create", podNamespace + "-" + podUID},
		// 		Shell:   true,
		// 	}
		// 	shell.Execute()
		// }

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
							HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
							return
						}

						gpuSpecs, err := h.GpuManager.GetAndAssignAvailableGPUs(numGpusRequestedInt, containerName)
						if err != nil {
							HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
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
					log.G(h.Ctx).Info("Container " + containerName + " is requesting port " + strconv.Itoa(int(port.ContainerPort)) + " to be exposed on host port " + strconv.Itoa(int(port.HostPort)))
					if port.HostPort != 0 {
						additionalPortArgs = append(additionalPortArgs, "-p", strconv.Itoa(int(port.HostPort))+":"+strconv.Itoa(int(port.ContainerPort)))
					}
				}

				cmd = append(cmd, additionalPortArgs...)

				//if h.Config.ExportPodData {
				mounts, err := prepareMounts(h.Ctx, h.Config, req, container)
				if err != nil {
					HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
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
						HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
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

				// run docker run --privileged -v /home:/home -d --name PODUID_dind docker:dind

				shell := exec.ExecTask{
					Command: "docker" + dockerOptions,
					Args:    cmd,
					Shell:   true,
				}

				// docker_commands_to_exec = append(docker_commands_to_exec, "docker "+strings.Join(cmd, " "))

				log.G(h.Ctx).Info("Docker command: " + strings.Join(shell.Args, " "))

				execReturn, err = shell.Execute()
				if err != nil {
					HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
					return
				}

				if execReturn.Stdout == "" {
					eval := "Conflict. The container name \"/" + containerName + "\" is already in use"
					if strings.Contains(execReturn.Stderr, eval) {
						log.G(h.Ctx).Warning("Container named " + containerName + " already exists. Skipping its creation.")
					} else {
						log.G(h.Ctx).Error("Unable to create container " + containerName + " : " + execReturn.Stderr)
						HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
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
					HandleErrorAndRemoveData(h, w, "Some errors occurred while creating container. Check Docker Sidecar's logs", err, &data)
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
								HandleErrorAndRemoveData(h, w, "Some errors occurred while inspecting container. Check Docker Sidecar's logs", err, &data)
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
	} */
}

func HandleErrorAndRemoveData(h *SidecarHandler, w http.ResponseWriter, s string, err error, podNamespace string, podUID string) {
	log.G(h.Ctx).Error(err)
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte("Some errors occurred while creating container. Check Docker Sidecar's logs"))

	if podNamespace != "" && podUID != "" {
		os.RemoveAll(h.Config.DataRootFolder + podNamespace + "-" + podUID)
	}
}
