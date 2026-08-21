module github.com/maintainerd/agent

go 1.26.6

require (
	github.com/containerd/errdefs v1.0.0
	github.com/docker/docker v28.5.1+incompatible
	github.com/docker/go-connections v0.8.1
	github.com/go-chi/chi/v5 v5.3.1
	github.com/maintainerd/core v0.0.0-00010101000000-000000000000
	github.com/maintainerd/kit v0.0.0-00010101000000-000000000000
	github.com/maintainerd/sdk v0.0.0-00010101000000-000000000000
	github.com/opencontainers/image-spec v1.1.1
	golang.org/x/sync v0.22.0
	google.golang.org/grpc v1.83.1
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/MicahParks/jwkset v0.11.1 // indirect
	github.com/MicahParks/keyfunc/v3 v3.8.1 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/containerd/log v0.1.0 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/maintainerd/secret v0.0.0-00010101000000-000000000000 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/sys/atomicwriter v0.1.0 // indirect
	github.com/moby/term v0.5.2 // indirect
	github.com/morikuni/aec v1.1.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.45.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260803160001-6ac0973c030d // indirect
	gotest.tools/v3 v3.5.2 // indirect
)

replace github.com/maintainerd/core => ../maintainerd

replace github.com/maintainerd/kit => ../maintainerd-kit

replace github.com/maintainerd/sdk => ../maintainerd-sdk

replace github.com/maintainerd/secret => ../maintainerd-secret
