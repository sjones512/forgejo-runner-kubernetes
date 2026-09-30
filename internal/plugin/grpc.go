package plugin

import (
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// NewGRPCServer accepts the pinned Runner v13.2.0 client's 30-second keepalive
// interval. gRPC's default five-minute minimum otherwise closes quiet streams
// with too_many_pings. The default no-active-RPC ping restriction is retained.
func NewGRPCServer() *grpc.Server {
	return grpc.NewServer(grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		MinTime:             30 * time.Second,
		PermitWithoutStream: false,
	}))
}
