package inbound

import (
	"context"

	"github.com/inazumav/sing-box/adapter"
	C "github.com/inazumav/sing-box/constant"
	"github.com/inazumav/sing-box/log"
	"github.com/inazumav/sing-box/option"
)

func NewHysteria2(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.HysteriaInboundOptions) (adapter.Inbound, error) {
	return nil, C.ErrQUICNotIncluded
}

type Hysteria2 struct {
	adapter.Inbound
}

func (h *Hysteria2) Start() error {
	return C.ErrQUICNotIncluded
}

func (h *Hysteria2) AddUsers(_ []option.Hysteria2User) error {
	return C.ErrQUICNotIncluded
}

func (h *Hysteria2) DelUsers(_ []string) error {
	return C.ErrQUICNotIncluded
}
