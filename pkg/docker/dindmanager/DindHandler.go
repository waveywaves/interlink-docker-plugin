package dindhandler

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/virtual-kubelet/virtual-kubelet/log"
)

type DindManagerInterface interface {
	Init(nDindContainer int8) error
	PrintDindList() error
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

func (a *DindManager) Init(nDindContainer int8) error {

	// print the number of DIND containers to be created
	log.G(a.Ctx).Info(fmt.Sprintf("\u2705 Creating %d DIND containers", nDindContainer))

	// get the working dir
	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	dindImage := "ghcr.io/extrality/nvidia-dind"

	for i := int8(0); i < nDindContainer; i++ {

		// generate a random UID for the DIND container
		randUID, err := GenerateUUIDv4()
		if err != nil {
			return err
		}

		// log the random UID
		log.G(a.Ctx).Info(fmt.Sprintf("Random UID: %s", randUID))

		// create the networks
		shell := exec.ExecTask{
			Command: "docker",
			Args:    []string{"network", "create", "--driver", "bridge", randUID + "_dind_network"},
			Shell:   true,
		}
		execReturnNetworkCommand, err := shell.Execute()

		log.G(a.Ctx).Info("Network creation command: ", execReturnNetworkCommand)

		if err != nil {
			return err
		}

		dindContainerArgs := []string{"run"}
		//dindContainerArgs = append(dindContainerArgs, gpuArgsAsArray...)
		if _, err := os.Stat("/cvmfs"); err == nil {
			dindContainerArgs = append(dindContainerArgs, "-v", "/cvmfs:/cvmfs")
		}

		// add the network to the dind container
		dindContainerArgs = append(dindContainerArgs, "--network", randUID+"_dind_network")
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

		// add the dind container to the list of DIND containers
		a.DindList = append(a.DindList, DindSpecs{DindID: dindContainerID, PodUID: "", DindNetworkID: randUID + "_dind_network", Available: true})
	}

	return nil
}

func (a *DindManager) PrintDindList() error {
	for _, dindSpec := range a.DindList {
		log.G(a.Ctx).Info(fmt.Sprintf("DindID: %s, PodUID: %s, DindNetworkID: %s, Available: %t", dindSpec.DindID, dindSpec.PodUID, dindSpec.DindNetworkID, dindSpec.Available))
	}
	return nil
}
