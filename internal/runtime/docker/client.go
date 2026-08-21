package docker

import (
	"context"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// dockerClient is the narrow slice of the Docker SDK the driver actually
// calls. Keeping it private and minimal serves two purposes: unit tests fake
// the engine without a daemon, and the driver's blast radius stays auditable —
// anything the driver could ever do to the host is enumerated right here.
type dockerClient interface {
	Ping(ctx context.Context) (types.Ping, error)

	ImagePull(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error)
	ImageInspect(ctx context.Context, imageID string, opts ...client.ImageInspectOption) (image.InspectResponse, error)

	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error)

	NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error)
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error

	VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
}

// Compile-time check that the real SDK client satisfies the narrow interface.
var _ dockerClient = (*client.Client)(nil)
