// Package plugin starts and serves otelcov's stage plugins (decisions 67 and
// 68): separate binaries that model the stages of one distribution at one
// version, spoken to over gRPC with hashicorp/go-plugin. The protocol is in
// proto/otelcov/plugin/v1 (Go code in pluginv1).
package plugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/Anefu/otelcov/pkg/plugin/pluginv1"
)

// Handshake keeps otelcov from starting a binary that isn't an otelcov
// plugin, and a plugin from running on its own. ProtocolVersion changes
// with incompatible protocol changes.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "OTELCOV_PLUGIN",
	MagicCookieValue: "stage",
}

// MaxMessage bounds one gRPC message. Clients split batches to stay under
// it.
const MaxMessage = 256 << 20

const name = "stage"

type grpcPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	impl pluginv1.StagePluginServer
}

func (p *grpcPlugin) GRPCServer(_ *goplugin.GRPCBroker, s *grpc.Server) error {
	pluginv1.RegisterStagePluginServer(s, p.impl)
	return nil
}

func (p *grpcPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pluginv1.NewStagePluginClient(c), nil
}

// Serve runs a plugin binary's server; it returns when the core is done.
func Serve(impl pluginv1.StagePluginServer) {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         goplugin.PluginSet{name: &grpcPlugin{impl: impl}},
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			return grpc.NewServer(append(opts, grpc.MaxRecvMsgSize(MaxMessage), grpc.MaxSendMsgSize(MaxMessage))...)
		},
		Logger: hclog.New(&hclog.LoggerOptions{Level: hclog.Warn, Output: os.Stderr}),
	})
}

// Conn is a running plugin.
type Conn struct {
	pluginv1.StagePluginClient
	client *goplugin.Client
}

// Stop stops the plugin process. (Close is the protocol's call that
// releases stage instances.)
func (c *Conn) Stop() { c.client.Kill() }

// Start runs the plugin binary at path and connects to it. Its logs go to
// stderr, warnings and above.
func Start(path string) (*Conn, error) {
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  Handshake,
		Plugins:          goplugin.PluginSet{name: &grpcPlugin{}},
		Cmd:              exec.Command(path),
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		GRPCDialOptions: []grpc.DialOption{
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMessage), grpc.MaxCallSendMsgSize(MaxMessage)),
		},
		Logger: hclog.New(&hclog.LoggerOptions{Name: "plugin", Level: hclog.Warn, Output: os.Stderr}),
	})
	rpc, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("plugin %s: %w", path, err)
	}
	raw, err := rpc.Dispense(name)
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("plugin %s: %w", path, err)
	}
	return &Conn{StagePluginClient: raw.(pluginv1.StagePluginClient), client: client}, nil
}
