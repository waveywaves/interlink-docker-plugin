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

func (h *SidecarHandler) prepareDockerRuns(podData commonIL.RetrievedPodData, w http.ResponseWriter) ([]DockerRunStruct, error) {

	var dockerRunStructs []DockerRunStruct
	var gpuArgs string = ""

	podUID := string(podData.Pod.UID)
	podNamespace := string(podData.Pod.Namespace)

	pathsOfVolumes := make(map[string]string)

	for _, volume := range podData.Pod.Spec.Volumes {
		if volume.HostPath != nil {
			if *volume.HostPath.Type == v1.HostPathDirectoryOrCreate || *volume.HostPath.Type == v1.HostPathDirectory {
				_, err := os.Stat(volume.HostPath.Path)
				if *volume.HostPath.Type == v1.HostPathDirectory {
					if os.IsNotExist(err) {
						HandleErrorAndRemoveData(h, w, "Host path directory does not exist", err, podNamespace, podUID)
						return dockerRunStructs, errors.New("Host path directory does not exist")
					}
					pathsOfVolumes[volume.Name] = volume.HostPath.Path
				} else if *volume.HostPath.Type == v1.HostPathDirectoryOrCreate {
					if os.IsNotExist(err) {
						err = os.MkdirAll(volume.HostPath.Path, os.ModePerm)
						if err != nil {
							HandleErrorAndRemoveData(h, w, "An error occurred during mkdir of host path directory", err, podNamespace, podUID)
							return dockerRunStructs, errors.New("An error occurred during mkdir of host path directory")
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
				// WIP: this is a temporary solution to mount CVMFS volumes for persistent volume claims case
				pathsOfVolumes[volume.PersistentVolumeClaim.ClaimName] = "/cvmfs"
			}

		}
	}

	allContainers := map[string][]v1.Container{
		"initContainers": podData.Pod.Spec.InitContainers,
		"containers":     podData.Pod.Spec.Containers,
	}

	for containerType, containers := range allContainers {
		isInitContainer := containerType == "initContainers"

		for _, container := range containers {

			containerName := podNamespace + "-" + podUID + "-" + container.Name

			var envVars string = ""
			for _, envVar := range container.Env {
				if envVar.Value != "" {
					if strings.Contains(envVar.Value, "[") {
						envVars += " -e " + envVar.Name + "='" + envVar.Value + "'"
					} else {
						envVars += " -e " + envVar.Name + "=" + envVar.Value
					}
				} else {
					envVars += " -e " + envVar.Name
				}
			}

			for _, volumeMount := range container.VolumeMounts {
				if volumeMount.MountPath != "" {

					if _, ok := pathsOfVolumes[volumeMount.Name]; !ok {
						continue
					}
					if volumeMount.ReadOnly {
						envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath + ":ro"
					} else {
						if volumeMount.MountPropagation != nil && *volumeMount.MountPropagation == v1.MountPropagationBidirectional {
							envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath + ":shared"
						} else {
							envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath
						}
					}
				}
			}

			envVars += " --network=host"
			cmd := []string{"run", "-d", "--name", containerName}

			cmd = append(cmd, envVars)

			if container.SecurityContext != nil && container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
				cmd = append(cmd, "--privileged")
			}

			var additionalPortArgs []string

			for _, port := range container.Ports {
				if port.HostPort != 0 {
					additionalPortArgs = append(additionalPortArgs, "-p", strconv.Itoa(int(port.HostPort))+":"+strconv.Itoa(int(port.ContainerPort)))
				}
			}

			cmd = append(cmd, additionalPortArgs...)

			mounts, err := prepareMounts(h.Ctx, h.Config, podData, container)
			if err != nil {
				HandleErrorAndRemoveData(h, w, "An error occurred during preparing mounts for the POD", err, podNamespace, podUID)
				return dockerRunStructs, errors.New("An error occurred during preparing mounts for the POD")
			}

			cmd = append(cmd, mounts)

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
				mountFileCommand, containerCommands, containerArgs, err = parseContainerCommandAndReturnArgs(h.Ctx, h.Config, podUID, podNamespace, container)
				if err != nil {
					HandleErrorAndRemoveData(h, w, "An error occurred during the parse of the container commands and arguments", err, podNamespace, podUID)
					return dockerRunStructs, errors.New("An error occurred during the parse of the container commands and arguments")
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

			shell := exec.ExecTask{
				Command: "docker" + dockerOptions,
				Args:    cmd,
				Shell:   true,
			}

			dockerRunStructs = append(dockerRunStructs, DockerRunStruct{
				Name:            containerName,
				Command:         "docker " + strings.Join(shell.Args, " "),
				IsInitContainer: isInitContainer,
				GpuArgs:         gpuArgs,
			})
		}
	}

	return dockerRunStructs, nil
}

func (h *SidecarHandler) CreateHandler(w http.ResponseWriter, r *http.Request) {

	log.G(h.Ctx).Info("\u23F3 [CREATE CALL] Received create call from InterLink ")

	// create bool variable to set if a new dind container has to be created
	newDindContainerCreated := false

	// get a dind container ID from dind manager of the sidecard handler
	dindContainerID, err := h.DindManager.GetAvailableDind()
	if err != nil {

		log.G(h.Ctx).Info("\u2705 [POD FLOW] No available DIND container found, creating a new one")

		h.DindManager.BuildDindContainers(1)
		dindContainerID, err = h.DindManager.GetAvailableDind()
		if err != nil {
			HandleErrorAndRemoveData(h, w, "During creation of new DIND container, an error occurred during the request of get available DIND container", err, "", "")
			return
		}
		newDindContainerCreated = true
	}

	// remove the dind container from the list of available dind containers
	err = h.DindManager.SetDindUnavailable(dindContainerID)
	if err != nil {
		HandleErrorAndRemoveData(h, w, "An error occurred during the removal of the DIND container from the list of available DIND containers", err, "", "")
		return
	}

	if !newDindContainerCreated {
		// create a new dind container in background
		go h.DindManager.BuildDindContainers(1)
	}

	//var execReturn exec.ExecResult
	statusCode := http.StatusOK

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		HandleErrorAndRemoveData(h, w, "An error occurred during read of body request for pod creation", err, "", "")
		return
	}

	var req []commonIL.RetrievedPodData
	err = json.Unmarshal(bodyBytes, &req)

	if err != nil {
		HandleErrorAndRemoveData(h, w, "An error occurred during json unmarshal of data from pod creation request", err, "", "")
		return
	}

	wd, err := os.Getwd()
	if err != nil {
		HandleErrorAndRemoveData(h, w, "Unable to get current working directory", err, "", "")
		return
	}

	log.G(h.Ctx).Info("\u2705 [POD FLOW] Request data unmarshalled successfully and current working directory detected")

	for _, data := range req {

		podUID := string(data.Pod.UID)
		podNamespace := string(data.Pod.Namespace)

		podDirectoryPath := filepath.Join(wd, h.Config.DataRootFolder+"/"+podNamespace+"-"+podUID)

		// if the podDirectoryPath does not exist, create it
		if _, err := os.Stat(podDirectoryPath); os.IsNotExist(err) {
			err = os.MkdirAll(podDirectoryPath, os.ModePerm)
			if err != nil {
				HandleErrorAndRemoveData(h, w, "An error occurred during the creation of the pod directory", err, "", "")
				return
			}
		}

		// call prepareDockerRuns to get the DockerRunStruct array
		dockerRunStructs, err := h.prepareDockerRuns(data, w)
		if err != nil {
			HandleErrorAndRemoveData(h, w, "An error occurred during preparing of docker run commmands", err, "", "")
			return
		}

		log.G(h.Ctx).Info("\u2705 [POD FLOW] Docker run commands prepared successfully")

		// from dockerRunStructs, create two arrays: one for initContainers and one for containers
		var initContainers []DockerRunStruct
		var containers []DockerRunStruct
		//var gpuArgs string

		for _, dockerRunStruct := range dockerRunStructs {
			if dockerRunStruct.IsInitContainer {
				initContainers = append(initContainers, dockerRunStruct)
			} else {
				containers = append(containers, dockerRunStruct)
			}
		}

		// // check if between the containers there is a container that requires a GPU
		// for _, container := range containers {
		// 	if container.GpuArgs != "" {
		// 		gpuArgs = container.GpuArgs
		// 	}
		// }

		// gpuArgsAsArray := []string{}
		// if gpuArgs != "" {
		// 	gpuArgsAsArray = strings.Split(gpuArgs, " ")
		// }

		// dindImage := "ghcr.io/extrality/nvidia-dind"
		// if gpuArgs == "" {
		// 	dindImage = "docker:dind"
		// }

		// // create a dedicated docker network for the dind container
		// shell := exec.ExecTask{
		// 	Command: "docker",
		// 	Args:    []string{"network", "create", "--driver", "bridge", string(data.Pod.UID) + "_dind_network"},
		// 	Shell:   true,
		// }
		// execReturnNetworkCommand, err := shell.Execute()
		// if err != nil {
		// 	HandleErrorAndRemoveData(h, w, "An error occurred during the creation of the network for the DIND container", err, "", "")
		// 	return
		// }

		// // log the docker network creation command
		// log.G(h.Ctx).Info("\u2705 [POD FLOW] Docker network created successfully with command: " + "docker " + strings.Join(shell.Args, " "))

		// dindContainerArgs := []string{"run"}
		// dindContainerArgs = append(dindContainerArgs, gpuArgsAsArray...)
		// if _, err := os.Stat("/cvmfs"); err == nil {
		// 	dindContainerArgs = append(dindContainerArgs, "-v", "/cvmfs:/cvmfs")
		// }

		// // add the network to the dind container
		// dindContainerArgs = append(dindContainerArgs, "--network", string(data.Pod.UID)+"_dind_network")
		// dindContainerArgs = append(dindContainerArgs, "--privileged", "-v", wd+":/"+wd, "-v", "/home:/home", "-v", "/var/lib/docker/overlay2:/var/lib/docker/overlay2", "-v", "/var/lib/docker/image:/var/lib/docker/image", "-d", "--name", string(data.Pod.UID)+"_dind", dindImage)

		// var dindContainerID string
		// shell = exec.ExecTask{
		// 	Command: "docker",
		// 	Args:    dindContainerArgs,
		// 	Shell:   true,
		// }

		// execReturn, err = shell.Execute()
		// if err != nil {
		// 	HandleErrorAndRemoveData(h, w, "An error occurred during the execution of DIND container command", err, "", "")
		// 	return
		// }
		// dindContainerID = execReturn.Stdout

		// // log also the command executed to create the DIND container
		// log.G(h.Ctx).Info("\u2705 [POD FLOW] DIND container command executed successfully: " + "docker " + strings.Join(shell.Args, " "))

		// log.G(h.Ctx).Info("\u2705 [POD FLOW] DIND container created successfully with ID: " + dindContainerID)

		// // create a variable of maximum number of retries
		// maxRetries := 20
		// output := []byte{}

		// // wait until the dind container is up and running by check that the command docker ps inside of it does not return an error
		// for {

		// 	if maxRetries == 0 {
		// 		HandleErrorAndRemoveData(h, w, "The number of attempts to check if the DIND container is running is 0. This means that an error occurred during the creation of the DIND container UID. "+dindContainerID+" output: "+string(output)+" Network creation output "+string(execReturnNetworkCommand.Stdout), err, "", "")
		// 		return
		// 	}

		// 	cmd := OSexec.Command("docker", "logs", string(data.Pod.UID)+"_dind")
		// 	output, err = cmd.CombinedOutput()

		// 	if err != nil {
		// 		time.Sleep(1 * time.Second)
		// 	}

		// 	if strings.Contains(string(output), "API listen on /var/run/docker.sock") {
		// 		break
		// 	} else {
		// 		time.Sleep(1 * time.Second)
		// 	}

		// 	maxRetries -= 1

		// }

		// log.G(h.Ctx).Info("\u2705 [POD FLOW] DIND container is up and running, ready to create the containers inside of it")

		// set the podUID to the dind container
		err = h.DindManager.SetPodUIDToDind(dindContainerID, podUID)
		if err != nil {
			HandleErrorAndRemoveData(h, w, "An error occurred during the setting of the pod UID to the DIND container", err, "", "")
			return
		}

		// run the docker command to rename the container to the pod UID
		shell := exec.ExecTask{
			Command: "docker",
			Args:    []string{"rename", dindContainerID, string(data.Pod.UID) + "_dind"},
			Shell:   true,
		}

		_, err = shell.Execute()
		if err != nil {
			HandleErrorAndRemoveData(h, w, "An error occurred during the rename of the DIND container", err, "", "")
			return
		}

		if len(initContainers) > 0 {

			log.G(h.Ctx).Info("\u2705 [POD FLOW] Start creating init containers")

			initContainersCommand := "#!/bin/sh\n"
			for _, initContainer := range initContainers {
				initContainersCommand += initContainer.Command + "\n"
			}
			err = os.WriteFile(podDirectoryPath+"/init_containers_command.sh", []byte(initContainersCommand), 0644)
			if err != nil {
				HandleErrorAndRemoveData(h, w, "An error occurred during the creation of the init container script file", err, "", "")
				return
			}

			shell := exec.ExecTask{
				Command: "docker",
				Args:    []string{"exec", string(data.Pod.UID) + "_dind", "/bin/sh", podDirectoryPath + "/init_containers_command.sh"},
			}

			_, err := shell.Execute()
			if err != nil {
				HandleErrorAndRemoveData(h, w, "An error occurred during the exec of the init container command script ", err, "", "")
				return
			}
			// Poll the container status until it exits
			for {

				allInitContainersCompleted := false
				initContainersCompleted := 0

				for _, initContainer := range initContainers {

					shell = exec.ExecTask{
						Command: "docker",
						Args:    []string{"exec", string(data.Pod.UID) + "_dind", "docker", "inspect", "--format='{{.State.Status}}'", initContainer.Name},
					}

					statusReturn, err := shell.Execute()
					if err != nil {
						HandleErrorAndRemoveData(h, w, "An error occurred during inspect of init container", err, "", "")
						return
					}

					status := strings.Trim(statusReturn.Stdout, "'\n")
					if status == "exited" {
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

			log.G(h.Ctx).Info("\u2705 [POD FLOW] Init containers created and executed successfully")
		}

		// create a file called containers_command.sh and write the containers commands to it, use WriteFile function
		containersCommand := "#!/bin/sh\n"
		for _, container := range containers {
			containersCommand += container.Command + "\n"
		}
		err = os.WriteFile(podDirectoryPath+"/containers_command.sh", []byte(containersCommand), 0644)
		if err != nil {
			HandleErrorAndRemoveData(h, w, "An error occurred during the creation of the container commands script.", err, "", "")
			return
		}

		log.G(h.Ctx).Info("\u2705 [POD FLOW] Containers commands written to the script file")

		shell = exec.ExecTask{
			Command: "docker",
			Args:    []string{"exec", string(data.Pod.UID) + "_dind", "/bin/sh", podDirectoryPath + "/containers_command.sh"},
		}

		_, err = shell.Execute()
		if err != nil {
			HandleErrorAndRemoveData(h, w, "An error occurred during the execution of the container command script", err, "", "")
			return
		}

		log.G(h.Ctx).Info("\u2705 [POD FLOW] Containers created successfully")

		createResponse := CreateStruct{PodUID: string(data.Pod.UID), PodJID: dindContainerID}
		createResponseBytes, err := json.Marshal(createResponse)
		if err != nil {
			statusCode = http.StatusInternalServerError
			HandleErrorAndRemoveData(h, w, "An error occurred during the json marshal of the returned JID", err, "", "")
			return
		}

		w.WriteHeader(statusCode)

		if statusCode != http.StatusOK {
			w.Write([]byte("Some errors occurred while creating containers. Check Docker Sidecar's logs"))
		} else {
			w.Write(createResponseBytes)
		}
	}

}

func HandleErrorAndRemoveData(h *SidecarHandler, w http.ResponseWriter, s string, err error, podNamespace string, podUID string) {
	log.G(h.Ctx).Error(err)
	log.G(h.Ctx).Info("\u274C Error description: " + s)
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte("Some errors occurred while creating container. Check Docker Sidecar's logs"))

	if podNamespace != "" && podUID != "" {
		os.RemoveAll(h.Config.DataRootFolder + podNamespace + "-" + podUID)
	}
}
