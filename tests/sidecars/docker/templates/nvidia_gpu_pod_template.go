package templates

const (
	NvidiaGpuPod = `apiVersion: v1
kind: Pod
metadata:
  name: {{.Name}}
  namespace: {{.Namespace}}
spec:
  restartPolicy: Never
  containers:
  - image: {{.Image}}
    imagePullPolicy: Always
    name: {{.ContainerName}}
    resources:
      requests:
        nvidia.com/gpu: {{.GpuRequested }} # requesting 1 GPU
      limits:
        nvidia.com/gpu: {{.GpuLimits }} # requesting 1 GPU
  dnsPolicy: ClusterFirst
  nodeSelector:
    kubernetes.io/hostname: {{.NodeSelector}}
  affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
      - matchExpressions:
        - key: nvidia.com/gpu
          operator: Gte
          values:
          - {{.GpuLimits }}
  tolerations:
  - key: virtual-node.interlink/no-schedule
    operator: Exists
  - key: node.kubernetes.io/not-ready
    operator: Exists
`
)
