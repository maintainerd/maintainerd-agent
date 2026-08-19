// Package runtimeclient is the agent's client for maintainerd-docker's
// RuntimeService. The agent is a *consumer* of the runtime contract that the
// docker repo owns; it imports docker's generated stubs directly.
package runtimeclient

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	runtimev1 "github.com/maintainerd/docker/gen/maintainerd/runtime/v1"
)

// Client wraps a gRPC connection to a runtime provider (maintainerd-docker).
type Client struct {
	conn *grpc.ClientConn
	rt   runtimev1.RuntimeServiceClient
}

// Dial creates a lazy client to the runtime provider at addr. The connection is
// established on first use, so this never blocks on a down runtime.
func Dial(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, rt: runtimev1.NewRuntimeServiceClient(conn)}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.rt.Ping(ctx, &runtimev1.PingRequest{})
	return err
}

func (c *Client) Pull(ctx context.Context, image string) error {
	_, err := c.rt.Pull(ctx, &runtimev1.PullRequest{Image: image})
	return err
}

func (c *Client) Run(ctx context.Context, spec *runtimev1.WorkloadSpec) (*runtimev1.WorkloadHandle, error) {
	resp, err := c.rt.Run(ctx, &runtimev1.RunRequest{Spec: spec})
	if err != nil {
		return nil, err
	}
	return resp.GetHandle(), nil
}

func (c *Client) Stop(ctx context.Context, id string) error {
	_, err := c.rt.Stop(ctx, &runtimev1.StopRequest{Handle: &runtimev1.WorkloadHandle{Id: id}})
	return err
}

func (c *Client) Remove(ctx context.Context, id string) error {
	_, err := c.rt.Remove(ctx, &runtimev1.RemoveRequest{Handle: &runtimev1.WorkloadHandle{Id: id}})
	return err
}

func (c *Client) Status(ctx context.Context, id string) (*runtimev1.WorkloadStatus, error) {
	resp, err := c.rt.Status(ctx, &runtimev1.StatusRequest{Handle: &runtimev1.WorkloadHandle{Id: id}})
	if err != nil {
		return nil, err
	}
	return resp.GetStatus(), nil
}

func (c *Client) List(ctx context.Context) ([]*runtimev1.WorkloadStatus, error) {
	resp, err := c.rt.List(ctx, &runtimev1.ListRequest{})
	if err != nil {
		return nil, err
	}
	return resp.GetWorkloads(), nil
}
