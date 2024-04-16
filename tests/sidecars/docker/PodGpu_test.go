package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"io/ioutil"

	"github.com/intertwin-eu/interlink-docker-plugin/tests/sidecars/docker/templates" // replace with the actual module path
	v1core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

type PodTemplateData struct {
	Name          string
	Namespace     string
	Image         string
	ContainerName string
	NodeSelector  string
	GpuRequested  string
	GpuLimits     string
}

// create a function that take as parameter the clientset and configure it
func CreateClientSet(kubeconfig string) (*kubernetes.Clientset, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("Error building kubeconfig: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("Error creating Kubernetes client: %v", err)
	}

	return clientset, nil
}

func CreatePodDependencies(namespace, name, image, containerName, nodeSelector, gpuRequested, gpuLimits string) (string, string, error) {

	podData := new(PodTemplateData)
	podData.ContainerName = containerName
	podData.Image = image
	podData.Name = name
	podData.Namespace = namespace
	podData.NodeSelector = nodeSelector
	podData.GpuRequested = gpuRequested
	podData.GpuLimits = gpuLimits

	startingPodTemplate, err := template.New("output_" + name + ".yaml").Parse(templates.NvidiaGpuPod)
	if err != nil {
		fmt.Printf("Error parsing template: %v\n", err)
		return "", "", err
	}

	folder, err := os.MkdirTemp(".", "output")
	if err != nil {
		fmt.Printf("Error creating temp folder: %v\n", err)
		return "", "", err
	}

	f, err := os.Create(folder + "/output_" + name + ".yaml")
	if err != nil {
		fmt.Printf("Error creating file: %v\n", err)
		return "", "", err
	}
	err = startingPodTemplate.Execute(f, podData)
	if err != nil {
		fmt.Printf("Error executing template: %v\n", err)
		return "", "", err
	}
	f.Close()

	return folder, folder + "/output_" + name + ".yaml", nil
}

func watchPodStatus(clientset *kubernetes.Clientset, namespace string, createdPod *v1core.Pod, t *testing.T) (error, v1core.PodPhase, string) {
	timeout := time.After(1 * time.Minute)

	watch, err := clientset.CoreV1().Pods(namespace).Watch(context.TODO(), metav1.ListOptions{
		FieldSelector: "metadata.name=" + createdPod.GetName(),
	})
	if err != nil {
		return fmt.Errorf("Error watching pod: %v", err), "", ""
	}

	for {
		select {
		case event, ok := <-watch.ResultChan():
			if !ok {
				return fmt.Errorf("pod watch channel closed before timeout"), "", ""
			}

			pod, ok := event.Object.(*v1core.Pod)
			if !ok {
				fmt.Printf("Unexpected type\n")
				continue
			}

			t.Logf("Pod %s status: %s\n", pod.Name, pod.Status.Phase)

			if pod.Status.Phase == v1core.PodFailed || pod.Status.Phase == v1core.PodSucceeded {
				watch.Stop()

				if pod.Status.Phase == v1core.PodFailed {
					return nil, pod.Status.Phase, ""
				}
				// Get the logs of the pod
				podLogOpts := v1core.PodLogOptions{}
				req := clientset.CoreV1().Pods(namespace).GetLogs(pod.Name, &podLogOpts)
				podLogs, err := req.Stream(context.TODO())
				if err != nil {
					return fmt.Errorf("error in opening stream: %v", err), "", ""
				}
				defer podLogs.Close()

				buf := new(bytes.Buffer)
				_, err = io.Copy(buf, podLogs)
				if err != nil {
					return fmt.Errorf("error in copy information from podLogs to buf: %v", err), "", ""
				}
				str := buf.String()
				return nil, pod.Status.Phase, str
			}
		case <-timeout:
			watch.Stop()
			return fmt.Errorf("pod did not reach 'Succeeded' or 'Failed' status within 1 minute"), "", ""
		}
	}
}

func CreatePod(yamlFilePath string, clientset *kubernetes.Clientset, namespace string) (*v1core.Pod, error) {
	// Read the YAML file
	yamlFile, err := ioutil.ReadFile(yamlFilePath)
	if err != nil {
		return nil, fmt.Errorf("Error reading YAML file: %v", err)
	}

	// Unmarshal the YAML into a Pod object
	var podTemplate v1core.Pod
	err = yaml.Unmarshal(yamlFile, &podTemplate)
	if err != nil {
		return nil, fmt.Errorf("Error unmarshalling YAML: %v", err)
	}

	// Create the Pod using the template
	createdPod, err := clientset.CoreV1().Pods(namespace).Create(context.TODO(), &podTemplate, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("Error creating pod: %v", err)
	}

	return createdPod, nil
}

func AvoidTestPodFailure(t *testing.T) {

	name := "cuda-sample-fail"
	containerName := "cuda-sample-container-fail"
	namespace := "vk"
	image := "nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda10.2"
	node := "vkgpu"
	kubeconfig := "/home/ubuntu/kubeconfig/kubeconfig.yaml"
	gpuLimits := "3"    // requesting 3 GPUs should fail because the VK node has only 2 GPU
	gpuRequested := "3" // requesting 3 GPUs should fail because the VK node has only 2 GPU

	// call createClientSet function to create the clientset
	clientset, err := CreateClientSet(kubeconfig)
	if err != nil {
		t.Fatalf("Error creating Kubernetes client: %v\n", err)
		return
	}

	// Create the Pod dependencies
	folder, yamlFilePath, err := CreatePodDependencies(namespace, name, image, containerName, node, gpuRequested, gpuLimits)
	if err != nil {
		t.Fatalf("Error creating pod dependencies: %v\n", err)
		return
	}

	// Ensure that the temporary folder is removed
	defer func() {
		err := os.RemoveAll(folder)
		if err != nil {
			fmt.Printf("Error removing temp folder: %v\n", err)
		}
	}()

	createdPod, err := CreatePod(yamlFilePath, clientset, namespace)
	if err != nil {
		t.Fatalf("Error creating pod: %v\n", err)
		return
	}

	defer func() {
		err := clientset.CoreV1().Pods(namespace).Delete(context.TODO(), createdPod.GetName(), metav1.DeleteOptions{})
		if err != nil {
			fmt.Printf("Error deleting pod: %v\n", err)
		}
	}()

	var podPhase v1core.PodPhase
	var expectedFinalPodPhase v1core.PodPhase = v1core.PodFailed

	err, podPhase, _ = watchPodStatus(clientset, namespace, createdPod, t)
	if err != nil {
		t.Fatalf("Error watching pod: %v\n", err)
		return
	}

	if podPhase != expectedFinalPodPhase {
		t.Fatalf("Pod %s did not reach the expected phase: %s\n", createdPod.GetName(), podPhase)
		return
	}

	t.Logf("Pod %s reached the expected phase: %s\n", createdPod.GetName(), podPhase)

}

/*
* The following test creates a new client and create n cuda test pods in parallel. This test is to check
* if the Virtual Kubelet node can handle multiple pods requesting one gpu at the same time.
 */
func TestParallelMultiplePodOneGpuConcurrent(t *testing.T) {
	name := "cuda-sample-"
	containerName := "cuda-sample-container-"
	namespace := "vk"
	image := "nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda10.2"
	node := "vkgpu"
	kubeconfig := "/home/ubuntu/kubeconfig/kubeconfig.yaml"
	gpuLimits := "1"
	gpuRequested := "1"

	var nPodToRun int = 1 // number of pods to run

	// call createClientSet function to create the clientset
	clientset, err := CreateClientSet(kubeconfig)
	if err != nil {
		t.Fatalf("Error creating Kubernetes client: %v\n", err)
		return
	}

	// initialize a list of created pods
	createdPods := make([]*v1core.Pod, nPodToRun)

	for i := 0; i < nPodToRun; i++ {

		name := name + fmt.Sprint(i)
		containerName := containerName + fmt.Sprint(i)
		// Create the Pod dependencies
		folder, yamlFilePath, err := CreatePodDependencies(namespace, name, image, containerName, node, gpuRequested, gpuLimits)
		if err != nil {
			t.Fatalf("Error creating pod dependencies: %v\n", err)
			return
		}

		// Ensure that the temporary folder is removed
		defer func() {
			err := os.RemoveAll(folder)
			if err != nil {
				fmt.Printf("Error removing temp folder: %v\n", err)
			}
		}()

		createdPod, err := CreatePod(yamlFilePath, clientset, namespace)
		createdPods[i] = createdPod

		if err != nil {
			t.Fatalf("Error creating pod: %v\n", err)
			return
		}

	}

	if err != nil {
		t.Fatalf("Error converting gpuRequested to int: %v\n", err)
		return
	}

	// initialize a list of podPhases
	podPhases := make([]v1core.PodPhase, nPodToRun)

	// check the status of the pods
	var wg sync.WaitGroup
	for i := 0; i < nPodToRun; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			// Watch for the pod to reach 'Succeeded' status
			watch, err := clientset.CoreV1().Pods(namespace).Watch(context.TODO(), metav1.ListOptions{
				FieldSelector: "metadata.name=" + createdPods[i].GetName(),
			})
			if err != nil {
				t.Errorf("Error watching pod: %v\n", err)
				return
			}

			timeout := time.After(time.Duration((i+1)*60) * time.Second)
			done := make(chan bool)

			go func() {
				for event := range watch.ResultChan() {
					pod, ok := event.Object.(*v1core.Pod)
					if !ok {
						t.Errorf("Unexpected type\n")
						return
					}

					if pod.Status.Phase == v1core.PodSucceeded || pod.Status.Phase == v1core.PodFailed {
						podPhases[i] = pod.Status.Phase
						done <- true
						return
					}
				}
			}()

			// Wait for done or timeout
			select {
			case <-timeout:
				t.Errorf("Timeout waiting for pod %s to reach 'Succeeded' or 'Failed' status\n", createdPods[i].GetName())
				podPhases[i] = v1core.PodFailed
			case <-done:
			}
		}(i)
	}

	wg.Wait()

	// delete the pods
	for i := 0; i < nPodToRun; i++ {
		err := clientset.CoreV1().Pods(namespace).Delete(context.TODO(), createdPods[i].GetName(), metav1.DeleteOptions{})
		if err != nil {
			fmt.Printf("Error deleting pod: %v\n", err)
		}
	}

	// expect all pods to be succeded
	for i := 0; i < nPodToRun; i++ {
		t.Logf("Pod %s status: %s\n", createdPods[i].GetName(), podPhases[i])
	}

	// expect all pods to be succeded
	for i := 0; i < nPodToRun; i++ {
		if podPhases[i] != v1core.PodSucceeded {
			t.Fatalf("Pod %s did not reach the expected phase: %s\n", createdPods[i].GetName(), podPhases[i])
			return
		}
	}
}

/*
* The following test is a sequential version of the previous test.
* It creates a new client and create a cuda test pod sequentially and waits for the pod to reach the Succeeded status (or Failed status).
* Then it checks the logs of the pod to see if the test passed (it should contain the string "Test PASSED").
* At the end of the test, the pod is deleted.
* This tests checks the log of the container while the other test checks only the status of the pod. The reason is that
* the pod can reach the Succeeded status even if the test failed. In this way we can check if the test is truly passed or not.
 */

func TestSequentialMultiplePodOneGpu(t *testing.T) {

	name := "cuda-sample-"
	containerName := "cuda-sample-container-"
	namespace := "vk"
	image := "nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda10.2"
	node := "vkgpu"
	kubeconfig := "/home/ubuntu/kubeconfig/kubeconfig.yaml"
	gpuRequested := "1"
	gpuLimit := "1"

	var nPodToRun int = 1

	// call createClientSet function to create the clientset
	clientset, err := CreateClientSet(kubeconfig)
	if err != nil {
		t.Fatalf("Error creating Kubernetes client: %v\n", err)
		return
	}

	for i := 0; i < nPodToRun; i++ {

		name := name + fmt.Sprint(i)
		containerName := containerName + fmt.Sprint(i)
		// Create the Pod dependencies
		folder, yamlFilePath, err := CreatePodDependencies(namespace, name, image, containerName, node, gpuRequested, gpuLimit)
		if err != nil {
			t.Fatalf("Error creating pod dependencies: %v\n", err)
			return
		}
		// Ensure that the temporary folder is removed
		defer func() {
			err := os.RemoveAll(folder)
			if err != nil {
				fmt.Printf("Error removing temp folder: %v\n", err)
			}
		}()

		createdPod, err := CreatePod(yamlFilePath, clientset, namespace)

		if err != nil {
			t.Fatalf("Error creating pod: %v\n", err)
			return
		}

		// Ensure to delete the pod at the end of the test
		defer func() {
			err := clientset.CoreV1().Pods(namespace).Delete(context.TODO(), createdPod.GetName(), metav1.DeleteOptions{})
			if err != nil {
				fmt.Printf("Error deleting pod: %v\n", err)
			}
		}()

		var podPhase v1core.PodPhase
		var expectedFinalPodPhase v1core.PodPhase = v1core.PodSucceeded
		var podLogs string

		err, podPhase, podLogs = watchPodStatus(clientset, namespace, createdPod, t)
		if err != nil {
			t.Fatalf("Error watching pod: %v\n", err)
			return
		}

		if podPhase != expectedFinalPodPhase {
			t.Fatalf("Pod %s did not reach the expected phase: %s\n", createdPod.GetName(), podPhase)
			return
		}

		// check if podLogs contains the string "Test PASSED"
		if !strings.Contains(podLogs, "Test PASSED") {
			t.Fatalf("Pod %s did not pass the test\n", createdPod.GetName())
			return
		}

		t.Log("Pod " + createdPod.GetName() + " passed the test")
	}

}

func TestDeletePossibleOutputFolders(t *testing.T) {

	folders, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("Error reading directories: %v\n", err)
		return
	}

	for _, folder := range folders {
		if folder.IsDir() && strings.HasPrefix(folder.Name(), "output") {
			err := os.RemoveAll(folder.Name())
			if err != nil {
				t.Fatalf("Error removing folder: %v\n", err)
				return
			}
		}
	}
}
