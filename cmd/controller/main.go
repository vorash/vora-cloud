package main

import (
	"context"
	"crypto/sha256"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	pb "github.com/vorash/vora-cloud/api/proto"
	"github.com/vorash/vora-cloud/pkg/api"
	"github.com/vorash/vora-cloud/pkg/auth"
	"github.com/vorash/vora-cloud/pkg/config"
	"github.com/vorash/vora-cloud/pkg/dashboard"
	"github.com/vorash/vora-cloud/pkg/database"
	"github.com/vorash/vora-cloud/pkg/deploy"
	"github.com/vorash/vora-cloud/pkg/deploy/k3s"
	"github.com/vorash/vora-cloud/pkg/githubapp"
	"github.com/vorash/vora-cloud/pkg/project"
	"github.com/vorash/vora-cloud/pkg/rest"
	"github.com/vorash/vora-cloud/pkg/secrets"
	"google.golang.org/grpc"
)

// @title vora-cloud API
// @version 1.0
// @description API for deploying applications to regional k3s clusters
// @host localhost:5000
// @BasePath /api/v1
func main() {
	// Load configuration
	cfg := config.LoadConfig()

	// One deployer per region, each talking to that region's cluster.
	kubeconfigs, err := config.ParseRegions(cfg.Regions)
	if err != nil {
		log.Fatal(err)
	}
	deployers := make(map[string]deploy.Deployer, len(kubeconfigs))
	for region, path := range kubeconfigs {
		d, err := k3s.NewFromKubeconfig(path, k3s.Options{IngressClass: cfg.IngressClass})
		if err != nil {
			log.Fatalf("Failed to connect region %s: %v", region, err)
		}
		deployers[region] = d
	}
	regions, err := deploy.NewRegistry(deployers)
	if err != nil {
		log.Fatal(err)
	}

	if cfg.DBConnection == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	maxConns, minConns, err := config.ParsePoolSize(cfg.DBMaxConns, cfg.DBMinConns)
	if err != nil {
		log.Fatal(err)
	}
	db, err := database.New(cfg.DBConnection, maxConns, minConns)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	// Initialize repositories
	apiKeyRepo := database.NewAPIKeyRepository(db)
	userRepo := database.NewUserRepository(db)
	refreshTokenRepo := database.NewRefreshTokenRepository(db)

	// Initialize services
	apiKeySvc := auth.NewAPIKeyService(apiKeyRepo)

	// JWT secret validation
	if cfg.JWTSecret == "" {
		log.Fatal("JWT_SECRET environment variable is required for user authentication")
	}
	if len(cfg.JWTSecret) < 32 {
		log.Fatal("JWT_SECRET must be at least 32 characters long")
	}

	if cfg.SecretsKey == "" {
		log.Fatal("SECRETS_KEY environment variable is required (openssl rand -base64 32)")
	}
	secretsKey, err := secrets.ParseKey(cfg.SecretsKey)
	if err != nil {
		log.Fatalf("SECRETS_KEY: %v", err)
	}
	secretsBox, err := secrets.New(secretsKey)
	if err != nil {
		log.Fatalf("SECRETS_KEY: %v", err)
	}

	// Initialize JWT service with token durations
	jwtService := auth.NewJWTService(
		cfg.JWTSecret,
		15*time.Minute, // Access token: 15 minutes
		7*24*time.Hour, // Refresh token: 7 days
	)

	// Initialize user service
	userSvc := auth.NewUserService(userRepo, refreshTokenRepo, jwtService)

	// Bootstrap initial API key if requested
	if cfg.BootstrapKey != "" {
		count, err := apiKeySvc.KeyCount()
		if err != nil {
			log.Fatalf("Failed to check API key count: %v", err)
		}

		if count == 0 {
			plainKey, apiKey, err := apiKeySvc.CreateKey(cfg.BootstrapKey)
			if err != nil {
				log.Fatalf("Failed to create bootstrap API key: %v", err)
			}
			log.Printf("Bootstrap API key created:")
			log.Printf("  Name: %s", apiKey.Name)
			log.Printf("  ID: %s", apiKey.ID)
			log.Printf("  Key: %s", plainKey)
			log.Println("  (Save this key - it will not be shown again)")
		} else {
			log.Println("API keys already exist, skipping bootstrap")
		}
	}

	// Init gRPC service with the region registry
	appService := api.NewApplicationService(regions)

	// Projects and deployments; the reconciler settles rollouts until shutdown.
	projectSvc := project.NewService(database.NewProjectRepository(db), regions, secretsBox, cfg.AppsDomain)
	reconcileCtx, stopReconciler := context.WithCancel(context.Background())
	defer stopReconciler()
	go projectSvc.Run(reconcileCtx, 5*time.Second)

	// Create gRPC listener
	grpcListener, err := net.Listen("tcp", ":"+cfg.GRPCPort)
	if err != nil {
		log.Fatalf("Failed to listen on gRPC port: %v", err)
	}

	// Create the gRPC server
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(api.AuthInterceptor(userSvc, apiKeySvc)))
	pb.RegisterControlPlaneServer(grpcServer, appService)
	pb.RegisterProjectsServer(grpcServer, api.NewProjectsService(projectSvc))

	// One sign-in budget per account, shared by the REST and dashboard sign-in.
	loginLimiter := auth.NewAttemptLimiter(12*time.Second, 5)

	// Create REST server
	restServer := rest.NewServer(appService, projectSvc, apiKeySvc, userSvc, cfg.HttpPort,
		rest.Options{AllowRegistration: cfg.AllowRegistration, Limiter: loginLimiter})

	// The dashboard shares the REST server's listener. Its form tokens are keyed
	// from the JWT secret with a label, so the two uses never share a key.
	csrfKey := sha256.Sum256([]byte("vora-cloud dashboard csrf\x00" + cfg.JWTSecret))
	sessions := database.NewSessionRepository(db)

	// Connecting GitHub accounts needs the whole App configuration, or none. The
	// variable is assigned only when it is on: a nil *Connector in the interface
	// would not be a nil interface, and the dashboard would serve the routes.
	var githubConn dashboard.GitHub
	githubOn, err := cfg.GitHubApp()
	if err != nil {
		log.Fatal(err)
	}
	if githubOn {
		githubKey, err := os.ReadFile(cfg.GitHubPrivateKeyFile)
		if err != nil {
			log.Fatalf("GITHUB_APP_PRIVATE_KEY_FILE: %v", err)
		}
		githubApp, err := githubapp.NewApp(cfg.GitHubClientID, githubKey)
		if err != nil {
			log.Fatalf("Failed to set up the GitHub App: %v", err)
		}
		callback := "" // empty: GitHub uses the first callback URL the App has
		if cfg.PublicURL != "" {
			callback = strings.TrimRight(cfg.PublicURL, "/") + "/github/callback"
		}
		githubConn = githubapp.NewConnector(githubApp, database.NewGitHubInstallationRepository(db),
			cfg.GitHubAppSlug, cfg.GitHubClientID, cfg.GitHubClientSecret, callback)
	} else {
		log.Println("GITHUB_APP_* is not set: connecting GitHub accounts is disabled")
	}

	dash, err := dashboard.New(dashboard.Config{
		Sessions: sessions, Users: userSvc, Projects: projectSvc, Regions: regions.Regions(),
		PublicURL: cfg.PublicURL, CSRFKey: csrfKey[:], Limiter: loginLimiter, GitHub: githubConn,
	})
	if err != nil {
		log.Fatalf("Failed to set up the dashboard: %v", err)
	}
	dash.Register(restServer.Echo())
	if cfg.PublicURL == "" {
		log.Println("PUBLIC_URL is not set: the dashboard derives its origin from each request, which breaks behind a proxy that rewrites the host or scheme")
	}

	// GitHub's webhook. Without a secret the endpoint is not served at all.
	if cfg.GitHubWebhookSecret == "" {
		log.Println("GITHUB_WEBHOOK_SECRET is not set: the GitHub webhook endpoint is disabled")
	} else {
		deliveries := database.NewGitHubDeliveryRepository(db)
		hook, err := githubapp.NewWebhook(cfg.GitHubWebhookSecret, deliveries)
		if err != nil {
			log.Fatalf("Failed to set up the GitHub webhook: %v", err)
		}
		hook.Register(restServer.Echo())
		go func() {
			for {
				// GitHub redelivers for at most three days; keep a week.
				if err := deliveries.DeleteOlderThan(reconcileCtx, time.Now().Add(-7*24*time.Hour)); err != nil && reconcileCtx.Err() == nil {
					log.Printf("Failed to delete old GitHub deliveries: %v", err)
				}
				select {
				case <-reconcileCtx.Done():
					return
				case <-time.After(time.Hour):
				}
			}
		}()
	}

	go func() {
		for {
			if err := sessions.DeleteExpired(reconcileCtx); err != nil && reconcileCtx.Err() == nil {
				log.Printf("Failed to delete expired sessions: %v", err)
			}
			select {
			case <-reconcileCtx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}()

	// Start gRPC server
	go func() {
		log.Printf("Starting gRPC server on :%s (regions: %v)", cfg.GRPCPort, regions.Regions())
		if err := grpcServer.Serve(grpcListener); err != nil {
			log.Fatalf("Failed to serve gRPC: %v", err)
		}
	}()

	// Start REST server
	go func() {
		log.Printf("Starting REST server on :%s", cfg.HttpPort)
		if err := restServer.Start(); err != nil {
			log.Printf("REST server stopped: %v", err)
		}
	}()

	// Wait for interrupt
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down...")

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shutdown REST server
	if err := restServer.Shutdown(ctx); err != nil {
		log.Printf("REST server shutdown error: %v", err)
	}

	// Stop gRPC server
	grpcServer.GracefulStop()

	log.Println("Servers stopped")
}
