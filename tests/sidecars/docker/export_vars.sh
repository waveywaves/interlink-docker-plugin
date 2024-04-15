#!/bin/bash

export TEST_POD_NAMESPACE="vk"
export TEST_POD_NAME="test-pod-vector-add"
export TEST_POD_IMAGE="nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda10.2"
export TEST_POD_CONTAINER_NAME="gpu-vect-add"
export TEST_POD_NODE_SELECTOR="vkgpu"
export TEST_KUBECONFIG_FILEPATH="/home/ubuntu/kubeconfig/kubeconfig.yaml"