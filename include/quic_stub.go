//go:build !with_quic

package include

import (
	"context"

	"github.com/inazumav/sing-box/adapter"
	"github.com/inazumav/sing-box/common/tls"
	C "github.com/inazumav/sing-box/constant"
	"github.com/inazumav/sing-box/option"
	"github.com/inazumav/sing-box/transport/v2ray"
	"github.com/sagernet/sing-dns"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

const WithQUIC = false

func init() {
	dns.RegisterTransport([]string{"quic", "h3"}, func(name string, ctx context.Context, logger logger.ContextLogger, dialer N.Dialer, link string) (dns.Transport, error) {
		return nil, C.ErrQUICNotIncluded
	})
	v2ray.RegisterQUICConstructor(
		func(ctx context.Context, options option.V2RayQUICOptions, tlsConfig tls.ServerConfig, handler adapter.V2RayServerTransportHandler) (adapter.V2RayServerTransport, error) {
			return nil, C.ErrQUICNotIncluded
		},
		func(ctx context.Context, dialer N.Dialer, serverAddr M.Socksaddr, options option.V2RayQUICOptions, tlsConfig tls.Config) (adapter.V2RayClientTransport, error) {
			return nil, C.ErrQUICNotIncluded
		},
	)
}
