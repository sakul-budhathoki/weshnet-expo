package core

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"

	"berty.tech/berty/v2/go/pkg/mdns"
	"berty.tech/berty/v2/go/pkg/osversion"
	"berty.tech/weshnet/v2"
	"berty.tech/weshnet/v2/pkg/ipfsutil"
	ipfs_mobile "berty.tech/weshnet/v2/pkg/ipfsutil/mobile"
	"berty.tech/weshnet/v2/pkg/netmanager"
	"berty.tech/weshnet/v2/pkg/protocoltypes"
	"github.com/gfanton/grpcutil/lazy"
	"github.com/gfanton/grpcutil/pipe"
	"github.com/gfanton/grpcutil/rpcmanager"
	ipfs_cfg "github.com/ipfs/kubo/config"
	"github.com/libp2p/go-libp2p"
	manet "github.com/multiformats/go-multiaddr/net"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

type BridgeConfig struct {
	RootDir            string
	NetDriver          NativeNetDriver
	MdnsLockerDriver   NativeMDNSLockerDriver
	ConnectivityDriver IConnectivityDriver
}

type Service struct {
	ctx    context.Context
	cancel context.CancelFunc

	server *grpc.Server
	client *grpc.ClientConn

	service            weshnet.Service
	close              func() error
	logger             *zap.Logger
	mdnsLocker         sync.Locker
	netmanager         *netmanager.NetManager
	connectivityDriver IConnectivityDriver
	rpcmanager         *rpcmanager.Service
	ipfsCoreAPI        ipfsutil.ExtendedCoreAPI

	ServiceClient
}

const connBufferSize = 4096

func NewBridgeConfig() *BridgeConfig {
	return &BridgeConfig{}
}

func NewService(config *BridgeConfig) (*Service, error) {
	close := func() error { return nil }

	// setup config
	if config == nil {
		return nil, fmt.Errorf("config is nil")
	}

	rootDir := config.RootDir
	if rootDir == "" {
		var err error
		rootDir, err = os.MkdirTemp("", "weshnet")
		if err != nil {
			return nil, fmt.Errorf("unable to create root dir: %w", err)
		}
		close = closeFunc(close, func() error {
			return os.RemoveAll(rootDir)
		})
	}

	// setup service
	ctx, cancel := context.WithCancel(context.Background())
	close = closeFunc(close, func() error {
		cancel()
		return nil
	})

	logger, err := zap.NewDevelopment()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("unable to create logger: %w", err)
	}

	s := &Service{
		ctx:    ctx,
		cancel: cancel,
		logger: logger,
	}

	// setup ipfs
	{
		repo, err := ipfsutil.LoadRepoFromPath(config.RootDir)
		if err != nil {
			close()
			return nil, err
		}

		mrepo := ipfs_mobile.NewRepoMobile(config.RootDir, repo)

		mopts := &ipfsutil.MobileOptions{
			IpfsConfigPatch: func(cfg *ipfs_cfg.Config) ([]libp2p.Option, error) {
				cfg.Discovery.MDNS.Enabled = false
				p2popts := []libp2p.Option{}
				return p2popts, nil
			},
		}

		mnode, err := ipfsutil.NewIPFSMobile(s.ctx, mrepo, mopts)
		if err != nil {
			close()
			return nil, err
		}

		s.ipfsCoreAPI, err = ipfsutil.NewExtendedCoreAPIFromNode(mnode.IpfsNode)
		if err != nil {
			close()
			return nil, err
		}

		close = closeFunc(close, func() error {
			return mnode.Close()
		})
	}

	// setup netdriver
	{
		if config.NetDriver != nil {
			inet := &inet{
				net:    config.NetDriver,
				logger: logger.Named("NetDriver"),
			}
			mdns.SetNetDriver(inet)
			manet.SetNetInterface(inet)
		}
	}

	// setup connectivity driver
	{
		if config.ConnectivityDriver != nil {
			s.connectivityDriver = config.ConnectivityDriver
			s.netmanager = netmanager.NewNetManager(*config.ConnectivityDriver.GetCurrentState().info)
			s.connectivityDriver.RegisterHandler(s)
		}
	}

	// setup mdnslocker
	{
		if runtime.GOOS == "android" && osversion.GetVersion().Major() >= 30 &&
			config.MdnsLockerDriver != nil {
			s.mdnsLocker = config.MdnsLockerDriver
		} else {
			s.mdnsLocker = &noopNativeMDNSLockerDriver{}
		}
	}

	// setup mDNS
	if config.MdnsLockerDriver != nil {
		mdnslogger := logger.Named("mdns")

		s.mdnsLocker.Lock()
		dh := mdns.DiscoveryHandler(ctx, mdnslogger, s.ipfsCoreAPI)
		mdnsService := mdns.NewMdnsService(mdnslogger, s.ipfsCoreAPI, mdns.MDNSServiceName, dh)

		go func() {
			mdnsNetworkManagerConfig := mdns.NetworkManagerConfig{
				Logger:     mdnslogger,
				NetManager: s.netmanager,
				Service:    mdnsService,
			}
			mdns.NetworkManagerHandler(ctx, mdnsNetworkManagerConfig)
		}()

		close = closeFunc(close, func() error {
			mdnsService.Close()
			s.mdnsLocker.Unlock()
			return nil
		})
	}

	s.service, err = weshnet.NewService(weshnet.Opts{
		DatastoreDir: rootDir,
		IpfsCoreAPI:  s.ipfsCoreAPI,
		Logger:       logger.Named("weshnet"),
	})
	if err != nil {
		close()
		return nil, fmt.Errorf("unable to start service: %w", err)
	}

	// setup grpc
	s.server = grpc.NewServer()
	listen := pipe.NewBufferPipe(connBufferSize)

	protocoltypes.RegisterProtocolServiceServer(s.server, s.service)

	managersvc := rpcmanager.NewService(&rpcmanager.Options{})
	rpcmanager.RegisterRPCManagerServer(s.server, managersvc)

	s.client, err = listen.ClientConn(ctx)
	if err != nil {
		close()
		return nil, fmt.Errorf("unable to create clien conn: %w", err)
	}

	// register account to service bridge
	for serviceName := range s.server.GetServiceInfo() {
		managersvc.RegisterService(serviceName, s.client)
	}

	go func() {
		defer cancel()
		if err := s.server.Serve(listen); err != nil {
			fmt.Printf("unable to serve listener: %s", err)
		}
	}()

	s.ServiceClient = NewServiceClient(lazy.NewClient(s.client))
	s.close = close

	return s, nil
}

func (s *Service) Close() error {
	if s.close != nil {
		s.close()
	}

	s.client.Close()
	s.server.Stop()
	return s.service.Close()
}

func (s *Service) HandleConnectivityUpdate(info *ConnectivityInfo) {
	s.logger.Info("Connectivity update", zap.Any("info", info.info.String()))

	s.netmanager.UpdateState(*info.info)
}

func Hello(name string) string {
	return fmt.Sprintf("hello %s", name)
}

func closeFunc(old func() error, new func() error) func() error {
	return func() error {
		if old != nil {
			_ = old()
		}
		return new()
	}
}
