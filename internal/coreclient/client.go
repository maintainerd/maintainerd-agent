// Package coreclient is the agent's client for maintainerd-core's AgentGateway
// (core.v1). The agent pulls work from Core and reports observed status back —
// the pull-model seam. The agent imports core's generated stubs.
package coreclient

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	corev1 "github.com/maintainerd/core/gen/maintainerd/core/v1"
)

// Client wraps a gRPC connection to Core's AgentGateway.
type Client struct {
	conn *grpc.ClientConn
	gw   corev1.AgentGatewayServiceClient
}

// Dial creates a lazy client to Core at addr.
func Dial(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, gw: corev1.NewAgentGatewayServiceClient(conn)}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) Register(ctx context.Context, agentUUID, version string, capabilities []string) error {
	_, err := c.gw.Register(ctx, &corev1.RegisterRequest{
		AgentUuid:    agentUUID,
		Version:      version,
		Capabilities: capabilities,
	})
	return err
}

func (c *Client) Heartbeat(ctx context.Context, agentUUID string) error {
	_, err := c.gw.Heartbeat(ctx, &corev1.HeartbeatRequest{AgentUuid: agentUUID})
	return err
}

func (c *Client) PullWork(ctx context.Context, agentUUID string, max int32) ([]*corev1.WorkItem, error) {
	resp, err := c.gw.PullWork(ctx, &corev1.PullWorkRequest{AgentUuid: agentUUID, MaxItems: max})
	if err != nil {
		return nil, err
	}
	return resp.GetItems(), nil
}

func (c *Client) ReportStatus(ctx context.Context, agentUUID string, reports []*corev1.StatusReport) (int32, error) {
	resp, err := c.gw.ReportStatus(ctx, &corev1.ReportStatusRequest{AgentUuid: agentUUID, Reports: reports})
	if err != nil {
		return 0, err
	}
	return resp.GetAccepted(), nil
}
