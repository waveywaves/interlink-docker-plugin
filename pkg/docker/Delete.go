package docker

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"

	"path/filepath"
)

// DeleteHandler stops and deletes Docker containers from provided data
func (h *SidecarHandler) DeleteHandler(w http.ResponseWriter, r *http.Request) {
	log.G(h.Ctx).Info("\u23F3 [DELETE CALL] Received delete call from Interlink")
	var execReturn exec.ExecResult
	statusCode := http.StatusOK
	bodyBytes, err := io.ReadAll(r.Body)

	if err != nil {
		statusCode = http.StatusInternalServerError
		log.G(h.Ctx).Error(err)
		w.WriteHeader(statusCode)
		w.Write([]byte("Some errors occurred while deleting container. Check Docker Sidecar's logs"))
		return
	}

	var pod v1.Pod
	err = json.Unmarshal(bodyBytes, &pod)
	if err != nil {
		statusCode = http.StatusInternalServerError
		w.WriteHeader(statusCode)
		w.Write([]byte("Some errors occurred while creating container. Check Docker Sidecar's logs"))
		log.G(h.Ctx).Error(err)
		return
	}

	podUID := string(pod.UID)
	podNamespace := string(pod.Namespace)

	for _, container := range pod.Spec.Containers {

		containerName := podNamespace + "-" + podUID + "-" + container.Name

		log.G(h.Ctx).Debug("\u2705 [DELETE CALL] Deleting container " + containerName)

		// added a timeout to the stop container command
		cmd := []string{"exec", podUID + "_dind", "docker", "stop", "-t", "10", containerName}
		shell := exec.ExecTask{
			Command: "docker",
			Args:    cmd,
			Shell:   true,
		}
		execReturn, _ = shell.Execute()

		if execReturn.Stderr != "" {
			if strings.Contains(execReturn.Stderr, "No such container") {
				log.G(h.Ctx).Debug("\u26A0 [DELETE CALL] Unable to find container " + containerName + ". Probably already removed? Skipping its removal")
			} else {
				log.G(h.Ctx).Error("\u274C [DELETE CALL] Error stopping container " + containerName + ". Skipping its removal")
				statusCode = http.StatusInternalServerError
				w.WriteHeader(statusCode)
				w.Write([]byte("Some errors occurred while deleting container. Check Docker Sidecar's logs"))
				return
			}
			continue
		}

		if execReturn.Stdout != "" {
			cmd = []string{"exec", podUID + "_dind", "docker", "rm", execReturn.Stdout}
			shell = exec.ExecTask{
				Command: "docker",
				Args:    cmd,
				Shell:   true,
			}
			execReturn, _ = shell.Execute()
			execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")

			if execReturn.Stderr != "" {
				log.G(h.Ctx).Error("\u274C [DELETE CALL] Error deleting container " + containerName)
				statusCode = http.StatusInternalServerError
				w.WriteHeader(statusCode)
				w.Write([]byte("Some errors occurred while deleting container. Check Docker Sidecar's logs"))
				return
			} else {
				log.G(h.Ctx).Info("\u2705 [DELETE CALL] Deleted container " + containerName)
			}
		}

		cmd = []string{"rm", "-f", podUID + "_dind"}
		shell = exec.ExecTask{
			Command: "docker",
			Args:    cmd,
			Shell:   true,
		}
		execReturn, _ = shell.Execute()
		execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")

		if execReturn.Stderr != "" {
			log.G(h.Ctx).Error("\u274C [DELETE CALL] Error deleting container " + podUID + "_dind")
			statusCode = http.StatusInternalServerError
			w.WriteHeader(statusCode)
			w.Write([]byte("Some errors occurred while deleting container. Check Docker Sidecar's logs"))
			return
		} else {
			log.G(h.Ctx).Info("\u2705 [DELETE CALL] Deleted container " + podUID + "_dind")
		}

		// check if the container has GPU devices attacched using the GpuManager and release them
		wd, err := os.Getwd()
		if err != nil {
			HandleErrorAndRemoveData(h, w, "Unable to get current working directory", err, "", "")
			return
		}
		podDirectoryPathToDelete := filepath.Join(wd, h.Config.DataRootFolder+"/"+podNamespace+"-"+podUID)
		log.G(h.Ctx).Info("\u2705 [DELETE CALL] Deleting directory " + podDirectoryPathToDelete)

		err = os.RemoveAll(podDirectoryPathToDelete)

		//os.RemoveAll(h.Config.DataRootFolder + pod.Namespace + "-" + string(pod.UID))
	}

	w.WriteHeader(statusCode)
	if statusCode != http.StatusOK {
		w.Write([]byte("Some errors occurred deleting containers. Check Docker Sidecar's logs"))
	} else {
		w.Write([]byte("All containers for submitted Pods have been deleted"))
	}
}
