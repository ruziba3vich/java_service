package main

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/ruziba3vich/java_service/genprotos/genprotos/compiler_service"
	"github.com/ruziba3vich/java_service/internal/service"
	"github.com/ruziba3vich/java_service/pkg/config"
	logger "github.com/ruziba3vich/prodonik_lgger"
	"go.uber.org/fx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
)

func NewNetListener(lc fx.Lifecycle, cfg *config.Config) (net.Listener, error) {
	lis, err := net.Listen("tcp", ":"+cfg.AppPort)
	if err != nil {
		return nil, err
	}

	log.Printf("gRPC server will listen on port: %s", cfg.AppPort)

	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			return lis.Close()
		},
	})
	return lis, nil
}

func NewLogger(cfg *config.Config) (*logger.Logger, error) {
	return logger.NewLogger(cfg.LogPath)
}

func NewGRPCServer() *grpc.Server {
	return grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     15 * time.Second,
			MaxConnectionAge:      30 * time.Second,
			MaxConnectionAgeGrace: 5 * time.Second,
			Time:                  5 * time.Second,
			Timeout:               1 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	)
}

func main() {
	fx.New(
		fx.Provide(
			config.NewConfig,
			NewNetListener,
			NewLogger,
			NewGRPCServer,
			service.NewJavaExecutorServer,
		),
		fx.Invoke(func(lc fx.Lifecycle, lis net.Listener, s *grpc.Server, server *service.JavaExecutorServer, cfg *config.Config) {
			compiler_service.RegisterCodeExecutorServer(s, server)
			reflection.Register(s)

			lc.Append(fx.Hook{
				OnStart: func(ctx context.Context) error {
					log.Printf("gRPC server starting on port %s...", cfg.AppPort)
					go func() {
						if err := s.Serve(lis); err != nil {
							log.Printf("gRPC server failed: %v", err)
						}
					}()
					return nil
				},
				OnStop: func(ctx context.Context) error {
					log.Println("Stopping gRPC server...")
					s.GracefulStop()
					return nil
				},
			})
		}),
	).Run()
}
