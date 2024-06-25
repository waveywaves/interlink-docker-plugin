package docker

type DockerRunStruct struct {
	Name            string `json:"name"`
	Command         string `json:"command"`
	IsInitContainer bool   `json:"isInitContainer"`
	GpuArgs         string `json:"gpuArgs"`
}
type CreateStruct struct {
	PodUID string `json:"PodUID"`
	PodJID string `json:"PodJID"`
}
