package dindmanager

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"time"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"

	OSexec "os/exec"
)

type DindManagerInterface interface {
	CleanDindContainers() error
	BuildDindContainers(nDindContainer int8) error
	PrintDindList() error
	GetAvailableDind() (string, error)
	SetDindUnavailable(dindID string) error
	RemoveDindFromList(PodUID string) error
	SetPodUIDToDind(dindID string, podUID string) error
	GetDindFromPodUID(podUID string) (DindSpecs, error)
	SetDindAvailable(PodUID string) error
}

type DindSpecs struct {
	DindID        string
	PodUID        string
	DindNetworkID string
	Available     bool
}

type DindManager struct {
	DindList []DindSpecs
	Ctx      context.Context
}

// GenerateUUIDv4 generates a random UUIDv4
func GenerateUUIDv4() (string, error) {
	uuid := make([]byte, 16)
	_, err := rand.Read(uuid)
	if err != nil {
		return "", err
	}

	uuid[6] = (uuid[6] & 0x0f) | 0x40
	uuid[8] = (uuid[8] & 0x3f) | 0x80

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16]), nil
}

func (a *DindManager) CleanDindContainers() error {

	// print the number of DIND containers to be created
	log.G(a.Ctx).Info(fmt.Sprintf("\u2705 Start cleaning zombie DIND containers"))

	// exec this command docker ps -a --format "{{.Names}}" | grep '_dind$' | wc -l
	shell := exec.ExecTask{
		Command: "docker",
		Args:    []string{"ps", "-a", "--format", "{{.Names}}", "|", "grep", "_dind$", "|", "wc", "-l"},
		Shell:   true,
	}
	execReturn, err := shell.Execute()
	if err != nil {
		return err
	}

	// log the number of zombie DIND containers (remove the \n at the end of the string)
	log.G(a.Ctx).Info(fmt.Sprintf("\u2705 %s zombie DIND containers found", strings.ReplaceAll(execReturn.Stdout, "\n", "")))

	shell = exec.ExecTask{
		Command: "docker",
		Args:    []string{"ps", "-a", "--format", "{{.Names}}", "|", "grep", "_dind$", "|", "xargs", "-I", "{}", "docker", "rm", "-f", "{}"},
		Shell:   true,
	}
	_, err = shell.Execute()
	if err != nil {
		return err
	}

	// exec this command  docker network ls --filter name=_dind_network$ --format "{{.ID}}" | xargs -r docker network rm

	shell = exec.ExecTask{
		Command: "docker",
		Args:    []string{"network", "ls", "--filter", "name=_dind_network$", "--format", "{{.ID}}", "|", "xargs", "-r", "docker", "network", "rm"},
		Shell:   true,
	}
	_, err = shell.Execute()
	if err != nil {
		return err
	}

	log.G(a.Ctx).Info(fmt.Sprintf("\u2705 DIND zombie containers cleaned"))

	return nil
}

func (a *DindManager) BuildDindContainers(nDindContainer int8) error {

	// print the number of DIND containers to be created
	log.G(a.Ctx).Info(fmt.Sprintf("\u2705 Creating %d DIND containers", nDindContainer))

	// get the working dir
	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	// get the env variable GPUENABLED, if 1 then the DIND container will have GPU support, otherwise it will not

	gpuEnabled := os.Getenv("GPUENABLED")
	dindImage := "docker:dind"
	if gpuEnabled == "1" {
		dindImage = "ghcr.io/extrality/nvidia-dind"
	}

	for i := int8(0); i < nDindContainer; i++ {

		// generate a random UID for the DIND container
		randUID, err := GenerateUUIDv4()
		if err != nil {
			return err
		}

		// create the networks
		shell := exec.ExecTask{
			Command: "docker",
			Args:    []string{"network", "create", "--driver", "bridge", randUID + "_dind_network"},
			Shell:   true,
		}
		_, err = shell.Execute()
		if err != nil {
			return err
		}

		log.G(a.Ctx).Info(fmt.Sprintf("\u2705 DIND network %s created", randUID+"_dind_network"))

		dindContainerArgs := []string{"run"}
		//dindContainerArgs = append(dindContainerArgs, gpuArgsAsArray...)
		if _, err := os.Stat("/cvmfs"); err != nil {
			log.G(a.Ctx).Info(fmt.Sprintf("error while checking if /cvmfs exists: %s", err.Error()))
			return err
		} else {
			log.G(a.Ctx).Info(fmt.Sprintf("found /cvmfs"))
			dindContainerArgs = append(dindContainerArgs, "-v", "/cvmfs:/cvmfs")
		}

		// add the network to the dind container
		dindContainerArgs = append(dindContainerArgs, "--network", randUID+"_dind_network")
		// "--runtime=nvidia" is added to the dind container if the GPUENABLED env variable is set to 1

		if gpuEnabled == "1" {
			dindContainerArgs = append(dindContainerArgs, "--runtime=nvidia")
		}
		dindContainerArgs = append(dindContainerArgs, "--privileged", "-v", wd+":/"+wd, "-v", "/home:/home", "-v", "/var/lib/docker/overlay2:/var/lib/docker/overlay2", "-v", "/var/lib/docker/image:/var/lib/docker/image", "-d", "--name", randUID+"_dind", dindImage)

		var dindContainerID string
		shell = exec.ExecTask{
			Command: "docker",
			Args:    dindContainerArgs,
			Shell:   true,
		}

		execReturn, err := shell.Execute()
		if err != nil {
			return err
		}
		dindContainerID = execReturn.Stdout

		// create a variable of maximum number of retries
		maxRetries := 20
		output := []byte{}

		// wait until the dind container is up and running by check that the command docker ps inside of it does not return an error
		for {

			if maxRetries == 0 {
				return fmt.Errorf("DIND container %s not up and running", dindContainerID)
			}

			cmd := OSexec.Command("docker", "logs", randUID+"_dind")
			output, err = cmd.CombinedOutput()
			if err != nil {
				time.Sleep(1 * time.Second)
			} else {
				return err
			}

			if strings.Contains(string(output), "API listen on /var/run/docker.sock") {
				break
			} else {
				time.Sleep(1 * time.Second)
			}

			maxRetries -= 1

		}

		log.G(a.Ctx).Info(fmt.Sprintf("\u2705 DIND container %s is up and running", dindContainerID))

		// add the dind container to the list of DIND containers
		a.DindList = append(a.DindList, DindSpecs{DindID: randUID + "_dind", PodUID: "", DindNetworkID: randUID + "_dind_network", Available: true})
	}

	return nil
}

func (a *DindManager) PrintDindList() error {
	for _, dindSpec := range a.DindList {
		log.G(a.Ctx).Info(fmt.Sprintf("DindID: %s, PodUID: %s, DindNetworkID: %s, Available: %t", dindSpec.DindID, dindSpec.PodUID, dindSpec.DindNetworkID, dindSpec.Available))
	}
	return nil
}

func (a *DindManager) GetDindFromPodUID(podUID string) (DindSpecs, error) {
	for _, dindSpec := range a.DindList {
		if dindSpec.PodUID == podUID {
			return dindSpec, nil
		}
	}
	return DindSpecs{}, fmt.Errorf("DIND container with PodUID %s not found", podUID)
}

func (a *DindManager) GetAvailableDind() (string, error) {
	for _, dindSpec := range a.DindList {
		if dindSpec.Available {
			return dindSpec.DindID, nil
		}
	}
	return "", fmt.Errorf("No available DIND container")
}

func (a *DindManager) SetDindUnavailable(dindID string) error {
	for i, dindSpec := range a.DindList {
		if dindSpec.DindID == dindID {
			a.DindList[i].Available = false
			return nil
		}
	}
	return fmt.Errorf("DIND container %s not found", dindID)
}

func (a *DindManager) SetDindAvailable(PodUI string) error {
	for i, dindSpec := range a.DindList {
		if dindSpec.PodUID == PodUI {
			a.DindList[i].Available = true
			return nil
		}
	}
	return fmt.Errorf("DIND container %s not found", PodUI)
}

func (a *DindManager) SetPodUIDToDind(dindID string, podUID string) error {
	for i, dindSpec := range a.DindList {
		if dindSpec.DindID == dindID {
			a.DindList[i].PodUID = podUID
			return nil
		}
	}
	return fmt.Errorf("DIND container %s not found", dindID)
}

func (a *DindManager) RemoveDindFromList(PodUID string) error {
	for i, dindSpec := range a.DindList {
		if dindSpec.PodUID == PodUID {
			a.DindList = append(a.DindList[:i], a.DindList[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("DIND container with PodUID %s not found", PodUID)
}
