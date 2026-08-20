module github.com/maintainerd/agent

go 1.26.6

replace github.com/maintainerd/docker => ../maintainerd-docker

require (
	github.com/go-chi/chi/v5 v5.3.1
	github.com/maintainerd/core v0.0.0-00010101000000-000000000000
	github.com/maintainerd/docker v0.0.0-00010101000000-000000000000
	golang.org/x/sync v0.22.0
	google.golang.org/grpc v1.83.1
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260803160001-6ac0973c030d // indirect
)

replace github.com/maintainerd/core => ../maintainerd
