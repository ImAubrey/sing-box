//go:build with_quic

package inbound

import (
	"context"
	"github.com/sagernet/quic-go"
	"sync"

	"github.com/inazumav/sing-box/adapter"
	"github.com/inazumav/sing-box/common/qtls"
	"github.com/inazumav/sing-box/common/tls"
	C "github.com/inazumav/sing-box/constant"
	"github.com/inazumav/sing-box/log"
	"github.com/inazumav/sing-box/option"
	"github.com/inazumav/sing-box/transport/hysteria"
	"github.com/sagernet/quic-go/congestion"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/auth"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

var _ adapter.Inbound = (*Hysteria)(nil)

type Hysteria struct {
	myInboundAdapter
	quicConfig   *quic.Config
	tlsConfig    tls.ServerConfig
	users        map[string]string // map[password]name
	xplusKey     []byte
	sendBPS      uint64
	recvBPS      uint64
	listener     qtls.QUICListener
	udpAccess    sync.RWMutex
	udpSessionId uint32
	udpSessions  map[uint32]chan *hysteria.UDPMessage
	udpDefragger hysteria.Defragger
}

func NewHysteria(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.HysteriaInboundOptions) (*Hysteria, error) {
	options.UDPFragmentDefault = true
	quicConfig := &quic.Config{
		InitialStreamReceiveWindow:     options.ReceiveWindowConn,
		MaxStreamReceiveWindow:         options.ReceiveWindowConn,
		InitialConnectionReceiveWindow: options.ReceiveWindowClient,
		MaxConnectionReceiveWindow:     options.ReceiveWindowClient,
		MaxIncomingStreams:             int64(options.MaxConnClient),
		KeepAlivePeriod:                hysteria.KeepAlivePeriod,
		DisablePathMTUDiscovery:        options.DisableMTUDiscovery || !(C.IsLinux || C.IsWindows),
		EnableDatagrams:                true,
	}
	if options.ReceiveWindowConn == 0 {
		quicConfig.InitialStreamReceiveWindow = hysteria.DefaultStreamReceiveWindow
		quicConfig.MaxStreamReceiveWindow = hysteria.DefaultStreamReceiveWindow
	}
	if options.ReceiveWindowClient == 0 {
		quicConfig.InitialConnectionReceiveWindow = hysteria.DefaultConnectionReceiveWindow
		quicConfig.MaxConnectionReceiveWindow = hysteria.DefaultConnectionReceiveWindow
	}
	if quicConfig.MaxIncomingStreams == 0 {
		quicConfig.MaxIncomingStreams = hysteria.DefaultMaxIncomingStreams
	}
	users := make(map[string]string, len(options.Users))
	for _, u := range options.Users {
		if len(u.Auth) > 0 {
			users[string(u.Auth)] = u.Name
		} else {
			users[u.AuthString] = u.Name
		}
	}
	var xplus []byte
	if options.Obfs != "" {
		xplus = []byte(options.Obfs)
	}
	var up, down uint64
	if len(options.Up) > 0 {
		up = hysteria.StringToBps(options.Up)
		if up == 0 {
			return nil, E.New("invalid up speed format: ", options.Up)
		}
	} else {
		up = uint64(options.UpMbps) * hysteria.MbpsToBps
	}
	if len(options.Down) > 0 {
		down = hysteria.StringToBps(options.Down)
		if down == 0 {
			return nil, E.New("invalid down speed format: ", options.Down)
		}
	} else {
		down = uint64(options.DownMbps) * hysteria.MbpsToBps
	}
	if up < hysteria.MinSpeedBPS {
		return nil, E.New("invalid up speed")
	}
	if down < hysteria.MinSpeedBPS {
		return nil, E.New("invalid down speed")
	}
	inbound := &Hysteria{
		myInboundAdapter: myInboundAdapter{
			protocol:      C.TypeHysteria,
			network:       []string{N.NetworkUDP},
			ctx:           ctx,
			router:        router,
			logger:        logger,
			tag:           tag,
			listenOptions: options.ListenOptions,
		},
		quicConfig:  quicConfig,
		users:       users,
		xplusKey:    xplus,
		sendBPS:     up,
		recvBPS:     down,
		udpSessions: make(map[uint32]chan *hysteria.UDPMessage),
	}
	if options.TLS == nil || !options.TLS.Enabled {
		return nil, C.ErrTLSRequired
	}
	if len(options.TLS.ALPN) == 0 {
		options.TLS.ALPN = []string{hysteria.DefaultALPN}
	}
	tlsConfig, err := tls.NewServer(ctx, logger, common.PtrValueOrDefault(options.TLS))
	if err != nil {
		return nil, err
	}
	inbound.tlsConfig = tlsConfig
	return inbound, nil
}

func (h *Hysteria) Start() error {
	packetConn, err := h.myInboundAdapter.ListenUDP()
	if err != nil {
		return err
	}
	if len(h.xplusKey) > 0 {
		packetConn = hysteria.NewXPlusPacketConn(packetConn, h.xplusKey)
		packetConn = &hysteria.PacketConnWrapper{PacketConn: packetConn}
	}
	err = h.tlsConfig.Start()
	if err != nil {
		return err
	}
	listener, err := qtls.Listen(packetConn, h.tlsConfig, h.quicConfig)
	if err != nil {
		return err
	}
	h.listener = listener
	h.logger.Info("udp server started at ", listener.Addr())
	go h.acceptLoop()
	return nil
}

func (h *Hysteria) acceptLoop() {
	for {
		ctx := log.ContextWithNewID(h.ctx)
		conn, err := h.listener.Accept(ctx)
		if err != nil {
			return
		}
		go func() {
			hErr := h.accept(ctx, conn)
			if hErr != nil {
				conn.CloseWithError(0, "")
				NewError(h.logger, ctx, E.Cause(hErr, "process connection from ", conn.RemoteAddr()))
			}
		}()
	}
}

func (h *Hysteria) accept(ctx context.Context, conn quic.Connection) error {
	controlStream, err := conn.AcceptStream(ctx)
	if err != nil {
		return err
	}
	clientHello, err := hysteria.ReadClientHello(controlStream)
	if err != nil {
		return err
	}
	if len(h.users) > 0 {
		var user string
		if u, ok := h.users[string(clientHello.Auth)]; ok {
			user = u
			if user == "" {
				user = string(clientHello.Auth)
			}
		} else {
			err = hysteria.WriteServerHello(controlStream, hysteria.ServerHello{
				Message: "wrong password",
			})
			return E.Errors(E.New("wrong password: ", string(clientHello.Auth)), err)
		}
		ctx = auth.ContextWithUser(ctx, user)
		h.logger.InfoContext(ctx, "[", user, "] inbound connection from ", conn.RemoteAddr())
	} else {
		h.logger.InfoContext(ctx, "inbound connection from ", conn.RemoteAddr())
	}
	h.logger.DebugContext(ctx, "peer send speed: ", clientHello.SendBPS/1024/1024, " MBps, peer recv speed: ", clientHello.RecvBPS/1024/1024, " MBps")
	if clientHello.SendBPS == 0 || clientHello.RecvBPS == 0 {
		return E.New("invalid rate from client")
	}
	serverSendBPS, serverRecvBPS := clientHello.RecvBPS, clientHello.SendBPS
	if h.sendBPS > 0 && serverSendBPS > h.sendBPS {
		serverSendBPS = h.sendBPS
	}
	if h.recvBPS > 0 && serverRecvBPS > h.recvBPS {
		serverRecvBPS = h.recvBPS
	}
	err = hysteria.WriteServerHello(controlStream, hysteria.ServerHello{
		OK:      true,
		SendBPS: serverSendBPS,
		RecvBPS: serverRecvBPS,
	})
	if err != nil {
		return err
	}
	conn.SetCongestionControl(hysteria.NewBrutalSender(congestion.ByteCount(serverSendBPS)))
	go h.udpRecvLoop(conn)
	for {
		var stream quic.Stream
		stream, err = conn.AcceptStream(ctx)
		if err != nil {
			return err
		}
		go func() {
			hErr := h.acceptStream(ctx, conn /*&hysteria.StreamWrapper{Stream: stream}*/, stream)
			if hErr != nil {
				stream.Close()
				NewError(h.logger, ctx, E.Cause(hErr, "process stream from ", conn.RemoteAddr()))
			}
		}()
	}
}

func (h *Hysteria) udpRecvLoop(conn quic.Connection) {
	for {
		packet, err := conn.ReceiveMessage(h.ctx)
		if err != nil {
			return
		}
		message, err := hysteria.ParseUDPMessage(packet)
		if err != nil {
			h.logger.Error("parse udp message: ", err)
			continue
		}
		dfMsg := h.udpDefragger.Feed(message)
		if dfMsg == nil {
			continue
		}
		h.udpAccess.RLock()
		ch, ok := h.udpSessions[dfMsg.SessionID]
		if ok {
			select {
			case ch <- dfMsg:
				// OK
			default:
				// Silently drop the message when the channel is full
			}
		}
		h.udpAccess.RUnlock()
	}
}

func (h *Hysteria) acceptStream(ctx context.Context, conn quic.Connection, stream quic.Stream) error {
	request, err := hysteria.ReadClientRequest(stream)
	if err != nil {
		return err
	}
	var metadata adapter.InboundContext
	metadata.Inbound = h.tag
	metadata.InboundType = C.TypeHysteria
	metadata.InboundOptions = h.listenOptions.InboundOptions
	metadata.Source = M.SocksaddrFromNet(conn.RemoteAddr()).Unwrap()
	metadata.OriginDestination = M.SocksaddrFromNet(conn.LocalAddr()).Unwrap()
	metadata.Destination = M.ParseSocksaddrHostPort(request.Host, request.Port).Unwrap()
	metadata.User, _ = auth.UserFromContext[string](ctx)

	if !request.UDP {
		err = hysteria.WriteServerResponse(stream, hysteria.ServerResponse{
			OK: true,
		})
		if err != nil {
			return err
		}
		h.logger.InfoContext(ctx, "inbound connection to ", metadata.Destination)
		return h.router.RouteConnection(ctx, hysteria.NewConn(stream, metadata.Destination, false), metadata)
	} else {
		h.logger.InfoContext(ctx, "inbound packet connection to ", metadata.Destination)
		var id uint32
		h.udpAccess.Lock()
		id = h.udpSessionId
		nCh := make(chan *hysteria.UDPMessage, 1024)
		h.udpSessions[id] = nCh
		h.udpSessionId += 1
		h.udpAccess.Unlock()
		err = hysteria.WriteServerResponse(stream, hysteria.ServerResponse{
			OK:           true,
			UDPSessionID: id,
		})
		if err != nil {
			return err
		}
		packetConn := hysteria.NewPacketConn(conn, stream, id, metadata.Destination, nCh, common.Closer(func() error {
			h.udpAccess.Lock()
			if ch, ok := h.udpSessions[id]; ok {
				close(ch)
				delete(h.udpSessions, id)
			}
			h.udpAccess.Unlock()
			return nil
		}))
		go packetConn.Hold()
		return h.router.RoutePacketConnection(ctx, packetConn, metadata)
	}
}

func (h *Hysteria) AddUsers(users []option.HysteriaUser) error {
	for _, u := range users {
		if len(u.Auth) > 0 {
			h.users[string(u.Auth)] = u.Name
		} else {
			h.users[u.AuthString] = u.Name
		}
	}
	return nil
}

func (h *Hysteria) DelUsers(names []string) error {
	for _, n := range names {
		delete(h.users, n)
	}
	return nil
}

func (h *Hysteria) Close() error {
	h.udpAccess.Lock()
	for _, session := range h.udpSessions {
		close(session)
	}
	h.udpSessions = make(map[uint32]chan *hysteria.UDPMessage)
	h.udpAccess.Unlock()
	return common.Close(
		&h.myInboundAdapter,
		h.listener,
		h.tlsConfig,
	)
}
