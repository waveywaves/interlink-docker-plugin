package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	exec2 "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"

	"fmt"

	commonIL "github.com/intertwin-eu/interlink-docker-plugin/pkg/common"
	"github.com/intertwin-eu/interlink-docker-plugin/pkg/docker/gpustrategies"
)

type SidecarHandler struct {
	Config     commonIL.InterLinkConfig
	Ctx        context.Context
	GpuManager gpustrategies.GPUManagerInterface
}

func parseContainerCommandAndReturnArgs(Ctx context.Context, config commonIL.InterLinkConfig, podUID string, podNamespace string, container v1.Container) ([]string, []string, []string, error) {

	//podUID := ""
	//podNamespace := ""

	//for _, podData := range data {
	//podUID = string(podData.Pod.UID)
	//podNamespace = string(podData.Pod.Namespace)

	// check if the directory exists, if not create it
	dirPath := config.DataRootFolder + podNamespace + "-" + podUID
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		err := os.MkdirAll(dirPath, os.ModePerm)
		if err != nil {
			log.G(Ctx).Error(err)
		} else {
			log.G(Ctx).Info("-- Created directory " + dirPath)
		}
	}

	//}

	if container.Command == nil {
		return []string{}, container.Command, container.Args, nil
	}

	prefileName := container.Name + "_" + podUID + "_" + podNamespace

	wd, err := os.Getwd()
	if err != nil {
		log.G(Ctx).Error(err)
		return nil, nil, nil, err
	}

	if len(container.Command) > 0 {

		fileName := prefileName + "_script.sh"

		if len(container.Args) == 0 {
			fileNamePath := filepath.Join(wd, config.DataRootFolder+podNamespace+"-"+podUID, fileName)
			log.G(Ctx).Info("Creating file " + fileNamePath)
			err = os.WriteFile(fileNamePath, []byte(strings.Join(container.Command, " ")), 0644)
			if err != nil {
				log.G(Ctx).Error(err)
				return nil, nil, nil, err
			}
			return []string{"-v " + fileNamePath + ":/" + fileName}, []string{"/bin/sh" + " /" + fileName}, []string{}, nil
		}

		argsFileName := container.Name + "_args"
		argsFileNamePath := filepath.Join(wd, config.DataRootFolder+podNamespace+"-"+podUID, argsFileName)
		log.G(Ctx).Info("Creating file " + argsFileNamePath)
		err = os.WriteFile(argsFileNamePath, []byte(strings.Join(container.Args, " ")), 0644)
		if err != nil {
			log.G(Ctx).Error(err)
			return nil, nil, nil, err
		}

		fullFileContent := strings.Join(container.Command, " ") + " \"$(cat " + argsFileName + ")\""
		fullFileNamePath := filepath.Join(wd, config.DataRootFolder+podNamespace+"-"+podUID, fileName)
		log.G(Ctx).Info("Creating file " + fullFileNamePath)
		err = os.WriteFile(fullFileNamePath, []byte(fullFileContent), 0644)
		if err != nil {
			log.G(Ctx).Error(err)
			return nil, nil, nil, err
		}

		// mount also the args file
		return []string{"-v " + argsFileNamePath + ":/" + argsFileName, "-v " + fullFileNamePath + ":/" + fileName}, []string{"/bin/sh" + " /" + fileName}, []string{}, nil

	} else {
		return []string{}, container.Command, container.Args, nil
	}
}

// prepareMounts iterates along the struct provided in the data parameter and checks for ConfigMaps, Secrets and EmptyDirs to be mounted.
// For each element found, the mountData function is called.
// It returns a string composed as the docker -v command to bind mount directories and files and the first encountered error.
func prepareMounts(Ctx context.Context, config commonIL.InterLinkConfig, data commonIL.RetrievedPodData, container v1.Container) (string, error) {
	log.G(Ctx).Info("- Preparing mountpoints for " + container.Name)
	mountedData := ""

	//for _, podData := range data {

	podUID := string(data.Pod.UID)
	podNamespace := string(data.Pod.UID)

	err := os.MkdirAll(config.DataRootFolder+data.Pod.Namespace+"-"+podUID, os.ModePerm)
	if err != nil {
		log.G(Ctx).Error(err)
		return "", err
	} else {
		log.G(Ctx).Info("-- Created directory " + config.DataRootFolder + data.Pod.Namespace + "-" + podUID)
	}

	// print the len of the init containers
	log.G(Ctx).Info("Init containers: " + fmt.Sprintf("%+v", data.InitContainers))

	allContainers := append(data.Containers, data.InitContainers...)

	log.G(Ctx).Info("All containers: " + fmt.Sprintf("%+v", allContainers))

	for _, cont := range allContainers {

		if cont.Name != container.Name {
			continue
		}

		containerName := podNamespace + "-" + podUID + "-" + container.Name

		log.G(Ctx).Info("cont values: " + fmt.Sprintf("%+v", cont))

		log.G(Ctx).Info("-- Inside Preparing mountpoints for " + cont.Name)
		for _, cfgMap := range cont.ConfigMaps {
			if containerName == podNamespace+"-"+podUID+"-"+cont.Name {
				log.G(Ctx).Info("-- Mounting ConfigMap " + cfgMap.Name)
				paths, err := mountData(Ctx, config, data.Pod, cfgMap, container)
				log.G(Ctx).Info("-- Paths: " + strings.Join(paths, ","))
				if err != nil {
					log.G(Ctx).Error("Error mounting ConfigMap " + cfgMap.Name)
					return "", errors.New("Error mounting ConfigMap " + cfgMap.Name)
				}
				for _, path := range paths {
					mountedData += "-v " + path + " "
				}
			}
		}

		for _, secret := range cont.Secrets {
			if containerName == podNamespace+"-"+podUID+"-"+cont.Name {
				paths, err := mountData(Ctx, config, data.Pod, secret, container)
				if err != nil {
					log.G(Ctx).Error("Error mounting Secret " + secret.Name)
					return "", errors.New("Error mounting Secret " + secret.Name)
				}
				for _, path := range paths {
					mountedData += "-v " + path + " "
				}
			}
		}

		for _, emptyDir := range cont.EmptyDirs {
			log.G(Ctx).Info("-- EmptyDir to handle " + emptyDir)
			if containerName == podNamespace+"-"+podUID+"-"+cont.Name {
				paths, err := mountData(Ctx, config, data.Pod, emptyDir, container)
				if err != nil {
					log.G(Ctx).Error("Error mounting EmptyDir " + emptyDir)
					return "", errors.New("Error mounting EmptyDir " + emptyDir)
				}
				for _, path := range paths {
					mountedData += "-v " + path + " "
				}
			}
		}
	}
	//}

	if last := len(mountedData) - 1; last >= 0 && mountedData[last] == ',' {
		mountedData = mountedData[:last]
	}
	return mountedData, nil
}

// mountData is called by prepareMounts and creates files and directory according to their definition in the pod structure.
// The data parameter is an interface and it can be of type v1.ConfigMap, v1.Secret and string (for the empty dir).
// Returns a string which is a bind mount of the file/directory. Example: path/to/file/on/host:path/to/file/in/container.
// It also returns the first encountered error.
func mountData(Ctx context.Context, config commonIL.InterLinkConfig, pod v1.Pod, data interface{}, container v1.Container) ([]string, error) {
	wd, err := os.Getwd()
	if err != nil {
		log.G(Ctx).Error(err)
		return nil, err
	}

	log.G(Ctx).Info("Inside mountData ")

	//if config.ExportPodData {

	log.G(Ctx).Info("Mounting data for " + container.Name)

	for _, mountSpec := range container.VolumeMounts {

		log.G(Ctx).Info("Mounting " + mountSpec.Name + " at " + mountSpec.MountPath)

		var podVolumeSpec *v1.VolumeSource

		for _, vol := range pod.Spec.Volumes {
			if vol.Name == mountSpec.Name {
				podVolumeSpec = &vol.VolumeSource
			}

			switch mount := data.(type) {
			case v1.ConfigMap:
				var configMapNamePaths []string
				err := os.RemoveAll(config.DataRootFolder + pod.Namespace + "-" + string(pod.UID) + "/" + "configMaps/" + vol.Name)

				if err != nil {
					log.G(Ctx).Error("Unable to delete root folder")
					return nil, err
				}
				if podVolumeSpec != nil && podVolumeSpec.ConfigMap != nil {
					podConfigMapDir := filepath.Join(wd+"/"+config.DataRootFolder+pod.Namespace+"-"+string(pod.UID)+"/", "configMaps/", vol.Name)
					mode := os.FileMode(*podVolumeSpec.ConfigMap.DefaultMode)

					correctMountPath := ""
					for _, volumeMount := range container.VolumeMounts {
						if volumeMount.Name == vol.Name {
							correctMountPath = volumeMount.MountPath
						}
					}

					if mount.Data != nil {
						for key := range mount.Data {

							log.G(Ctx).Info("Key: " + key)

							path := filepath.Join(podConfigMapDir, key)
							path += (":" + correctMountPath + "/" + key + " ")

							log.G(Ctx).Info("Path: " + path)

							configMapNamePaths = append(configMapNamePaths, path)
						}
					}

					cmd := []string{"-p " + podConfigMapDir}
					shell := exec2.ExecTask{
						Command: "mkdir",
						Args:    cmd,
						Shell:   true,
					}

					execReturn, _ := shell.Execute()
					if execReturn.Stderr != "" {
						log.G(Ctx).Error(err)
						return nil, err
					} else {
						log.G(Ctx).Debug("-- Created directory " + podConfigMapDir)
					}

					log.G(Ctx).Info("-- Writing ConfigMaps files")
					for k, v := range mount.Data {
						// TODO: Ensure that these files are deleted in failure cases
						fullPath := filepath.Join(podConfigMapDir, k)
						os.WriteFile(fullPath, []byte(v), mode)
						if err != nil {
							log.G(Ctx).Errorf("Could not write ConfigMap file %s", fullPath)
							err = os.RemoveAll(fullPath)
							if err != nil {
								log.G(Ctx).Error("Unable to remove file " + fullPath)
							}
							return nil, err
						} else {
							log.G(Ctx).Debug("--- Written ConfigMap file " + fullPath)
						}
					}
					return configMapNamePaths, nil
				}

			case v1.Secret:
				var secretNamePaths []string
				err := os.RemoveAll(config.DataRootFolder + pod.Namespace + "-" + string(pod.UID) + "/" + "secrets/" + vol.Name)

				if err != nil {
					log.G(Ctx).Error("Unable to delete root folder")
					return nil, err
				}
				if podVolumeSpec != nil && podVolumeSpec.Secret != nil {
					mode := os.FileMode(*podVolumeSpec.Secret.DefaultMode)
					podSecretDir := filepath.Join(wd+"/"+config.DataRootFolder+pod.Namespace+"-"+string(pod.UID)+"/", "secrets/", vol.Name)

					if mount.Data != nil {
						for key := range mount.Data {
							path := filepath.Join(podSecretDir, key)
							path += (":" + mountSpec.MountPath + "/" + key + " ")
							secretNamePaths = append(secretNamePaths, path)
						}
					}

					cmd := []string{"-p " + podSecretDir}
					shell := exec2.ExecTask{
						Command: "mkdir",
						Args:    cmd,
						Shell:   true,
					}

					execReturn, _ := shell.Execute()
					if strings.Compare(execReturn.Stdout, "") != 0 {
						log.G(Ctx).Error(err)
						return nil, err
					}
					if execReturn.Stderr != "" {
						log.G(Ctx).Error(execReturn.Stderr)
						return nil, errors.New(execReturn.Stderr)
					} else {
						log.G(Ctx).Debug("-- Created directory " + podSecretDir)
					}

					log.G(Ctx).Info("-- Writing Secret files")
					for k, v := range mount.Data {
						// TODO: Ensure that these files are deleted in failure cases
						fullPath := filepath.Join(podSecretDir, k)
						os.WriteFile(fullPath, v, mode)
						if err != nil {
							log.G(Ctx).Errorf("Could not write Secret file %s", fullPath)
							err = os.RemoveAll(fullPath)
							if err != nil {
								log.G(Ctx).Error("Unable to remove file " + fullPath)
							}
							return nil, err
						} else {
							log.G(Ctx).Debug("--- Written Secret file " + fullPath)
						}
					}
					return secretNamePaths, nil
				}

			case string:
				if podVolumeSpec != nil && podVolumeSpec.EmptyDir != nil {
					var edPath string

					parts := strings.Split(data.(string), "/")
					emptyDirName := parts[len(parts)-1]
					if emptyDirName != vol.Name {
						log.G(Ctx).Info("Skipping " + vol.Name + " as it is not the same as " + emptyDirName)
						continue
					}

					emptyDirMountPath := ""
					isReadOnly := false
					isBidirectional := false
					//isBidirectional := false
					for _, mountSpec := range container.VolumeMounts {
						if mountSpec.Name == vol.Name {
							emptyDirMountPath = mountSpec.MountPath
							if mountSpec.ReadOnly {
								isReadOnly = true
							}
							if mountSpec.MountPropagation != nil && *mountSpec.MountPropagation == v1.MountPropagationBidirectional {
								isBidirectional = true
							}
							// if mountSpec.MountPropagation != nil && *mountSpec.MountPropagation == v1.MountPropagationBidirectional {
							// 	isBidirectional = true
							// }
							break
						}
					}

					edPath = filepath.Join(wd + "/" + config.DataRootFolder + pod.Namespace + "-" + string(pod.UID) + "/" + "emptyDirs/" + vol.Name)
					log.G(Ctx).Info("-- Creating EmptyDir in " + edPath)
					cmd := []string{"-p " + edPath}
					shell := exec2.ExecTask{
						Command: "mkdir",
						Args:    cmd,
						Shell:   true,
					}

					_, err := shell.Execute()
					if err != nil {
						log.G(Ctx).Error(err)
						return []string{""}, nil
					} else {
						log.G(Ctx).Debug("-- Created EmptyDir in " + edPath)
					}

					// if the emptyDir is read only, append :ro to the path
					if isReadOnly {
						edPath += (":" + emptyDirMountPath + "/:ro")
					} else {
						if isBidirectional {
							edPath += (":" + emptyDirMountPath + "/:shared")
						} else {
							edPath += (":" + emptyDirMountPath + "/")
						}
					}

					return []string{edPath}, nil
				}
			}
		}
	}
	//}
	return nil, err
}
