package docker

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"

	commonIL "github.com/intertwin-eu/interlink-docker-plugin/pkg/common"
)

// StatusHandler checks Docker Container's status by running docker ps -af command and returns that status
func (h *SidecarHandler) StatusHandler(w http.ResponseWriter, r *http.Request) {
	log.G(h.Ctx).Info("\u23F3 [STATUS CALL] received get status call")
	var resp []commonIL.PodStatus
	var req []*v1.Pod
	statusCode := http.StatusOK

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		w.Write([]byte("Some errors occurred while checking container status. Check Docker Sidecar's logs"))
		log.G(h.Ctx).Error(err)
		return
	}

	err = json.Unmarshal(bodyBytes, &req)
	if err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		w.Write([]byte("Some errors occurred while checking container status. Check Docker Sidecar's logs"))
		log.G(h.Ctx).Error(err)
		return
	}

	for i, pod := range req {

		podUID := string(pod.UID)
		podNamespace := string(pod.Namespace)

		resp = append(resp, commonIL.PodStatus{PodName: pod.Name, PodUID: podUID, PodNamespace: podNamespace})
		for _, container := range pod.Spec.Containers {
			containerName := podNamespace + "-" + podUID + "-" + container.Name
			cmd := []string{"exec " + podUID + "_dind" + " docker ps -af name=^" + containerName + "$ --format \"{{.Status}}\""}

			shell := exec.ExecTask{
				Command: "docker",
				Args:    cmd,
				Shell:   true,
			}
			execReturn, err := shell.Execute()
			execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")

			if err != nil {
				log.G(h.Ctx).Error(err)
				statusCode = http.StatusInternalServerError
				break
			}

			log.G(h.Ctx).Info("\u2705 [STATUS CALL] Status of the container retrieved successfully")

			containerstatus := strings.Split(execReturn.Stdout, " ")

			if execReturn.Stdout != "" {
				log.G(h.Ctx).Info("\u2705 [STATUS CALL] The container " + container.Name + " is in the state: " + containerstatus[0])

				if containerstatus[0] == "Created" {
					resp[i].Containers = append(resp[i].Containers, v1.ContainerStatus{Name: container.Name, State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}}, Ready: false})
				} else if containerstatus[0] == "Up" {
					resp[i].Containers = append(resp[i].Containers, v1.ContainerStatus{Name: container.Name, State: v1.ContainerState{Running: &v1.ContainerStateRunning{}}, Ready: true})
				} else if containerstatus[0] == "Exited" {
					containerExitCode := strings.Split(containerstatus[1], "(")
					exitCode, err := strconv.Atoi(strings.Trim(containerExitCode[1], ")"))
					if err != nil {
						log.G(h.Ctx).Error(err)
						exitCode = 0
					}
					resp[i].Containers = append(resp[i].Containers, v1.ContainerStatus{Name: container.Name, State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{ExitCode: int32(exitCode)}}, Ready: false})
				}
			} else {
				resp[i].Containers = append(resp[i].Containers, v1.ContainerStatus{Name: container.Name, State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}}, Ready: false})
			}
		}
	}

	w.WriteHeader(statusCode)

	if statusCode != http.StatusOK {
		w.Write([]byte("Some errors occurred while checking container status. Check Docker Sidecar's logs"))
	} else {
		bodyBytes, err = json.Marshal(resp)
		if err != nil {
			log.G(h.Ctx).Error(err)
			statusCode = http.StatusInternalServerError
			w.WriteHeader(statusCode)
			w.Write([]byte("Some errors occurred while checking container status. Check Docker Sidecar's logs"))
		}
		w.Write(bodyBytes)
	}
}
