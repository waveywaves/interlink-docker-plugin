all: sidecars

sidecars:
	CGO_ENABLED=0 GOOS=linux go build -o bin/docker-sd cmd/main.go

