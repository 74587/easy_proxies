package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"easy_proxies/internal/builder"
	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/outbound/pool"

	"github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
)

// Run builds the runtime components from config and blocks until shutdown.
func Run(ctx context.Context, cfg *config.Config) error {
	monitorCfg := monitor.Config{
		Enabled:     cfg.ManagementEnabled(),
		Listen:      cfg.Management.Listen,
		ProbeTarget: cfg.Management.ProbeTarget,
	}
	monitorMgr, err := monitor.NewManager(monitorCfg)
	if err != nil {
		return fmt.Errorf("init monitor: %w", err)
	}

	buildResult, err := builder.Build(cfg)
	if err != nil {
		return err
	}

	inboundRegistry := include.InboundRegistry()
	outboundRegistry := include.OutboundRegistry()
	pool.Register(outboundRegistry)
	endpointRegistry := include.EndpointRegistry()
	dnsRegistry := include.DNSTransportRegistry()
	serviceRegistry := include.ServiceRegistry()

	ctx = box.Context(ctx, inboundRegistry, outboundRegistry, endpointRegistry, dnsRegistry, serviceRegistry)
	ctx = monitor.ContextWith(ctx, monitorMgr)

	instance, err := box.New(box.Options{Context: ctx, Options: buildResult})
	if err != nil {
		return fmt.Errorf("create sing-box instance: %w", err)
	}
	if err := instance.Start(); err != nil {
		return fmt.Errorf("start sing-box: %w", err)
	}

	var monitorServer *monitor.Server
	if monitorCfg.Enabled {
		monitorServer = monitor.NewServer(monitorCfg, monitorMgr, log.Default())
		monitorServer.Start(ctx)
		defer monitorServer.Shutdown(context.Background())
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-ctx.Done():
	case sig := <-sigCh:
		fmt.Printf("received %s, shutting down\n", sig)
	}
	return instance.Close()
}
