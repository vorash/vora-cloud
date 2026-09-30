package rest

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"time"

	"os"

	"github.com/go-logr/stdr"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.opentelemetry.io/contrib/instrumentation/github.com/labstack/echo/otelecho"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	stdout "go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	echoSwagger "github.com/swaggo/echo-swagger"
	_ "github.com/vorash/vora-cloud/docs"
	"github.com/vorash/vora-cloud/pkg/api"
	"github.com/vorash/vora-cloud/pkg/auth"
	"github.com/vorash/vora-cloud/pkg/database"
	"github.com/vorash/vora-cloud/pkg/project"
)

// Options are the settings of the REST server that are not services.
type Options struct {
	// AllowRegistration opens POST /auth/register to anyone.
	AllowRegistration bool
	// Limiter limits sign-in attempts per email. Share it with the dashboard, so
	// guesses over either count against one budget. Nil gets a private default.
	Limiter LoginLimiter
}

// LoginLimiter spends one sign-in attempt for an account.
type LoginLimiter interface {
	Allow(key string) bool
}

// logins signs users in.
type logins interface {
	LoginUser(ctx context.Context, email, password string) (accessToken, refreshToken string, user *database.User, err error)
}

// accountCreator creates user accounts.
type accountCreator interface {
	RegisterUser(ctx context.Context, name, email, password string) (*database.User, error)
}

// Server the REST API server
type Server struct {
	echo           *echo.Echo
	appService     *api.ApplicationService
	projects       *project.Service
	authSvc        *auth.APIKeyService
	userSvc        *auth.UserService
	accounts       accountCreator
	logins         logins
	opts           Options
	port           string
	tracerProvider *sdktrace.TracerProvider
}

// NewServer creates a new REST API server
func NewServer(appService *api.ApplicationService, projects *project.Service, authSvc *auth.APIKeyService, userSvc *auth.UserService, port string, opts Options) *Server {
	tp := initTracer()
	if opts.Limiter == nil {
		opts.Limiter = auth.NewAttemptLimiter(12*time.Second, 5)
	}

	e := echo.New()
	e.HideBanner = true

	e.Use(otelecho.Middleware("vorash-control-plane"))
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())

	server := &Server{
		echo:           e,
		appService:     appService,
		projects:       projects,
		authSvc:        authSvc,
		userSvc:        userSvc,
		accounts:       userSvc,
		logins:         userSvc,
		opts:           opts,
		port:           port,
		tracerProvider: tp,
	}

	server.setupRoutes()

	return server
}

// setupRoutes configures all API routes
func (s *Server) setupRoutes() {
	s.echo.GET("/api/v1/health", s.healthCheck)
	s.echo.GET("/swagger/*", echoSwagger.WrapHandler)

	// Authentication routes
	auth := s.echo.Group("/api/v1/auth")
	auth.POST("/register", s.register)
	auth.POST("/login", s.login)
	auth.POST("/refresh", s.refreshToken)
	auth.POST("/logout", s.logout)

	// Protected auth routes
	auth.GET("/me", s.getCurrentUser, s.jwtOnlyMiddleware)

	// API v1 routes (accept both JWT and API key)
	v1 := s.echo.Group("/api/v1")
	v1.Use(s.authMiddleware)

	s.registerDeployRoutes(v1)

	// User API key management (require JWT only)
	userKeys := v1.Group("/user/keys")
	userKeys.Use(s.jwtOnlyMiddleware)
	userKeys.POST("", s.createUserAPIKey)
	userKeys.GET("", s.listUserAPIKeys)
	userKeys.DELETE("/:id", s.deleteUserAPIKey)

}

// Echo returns the underlying router, so the dashboard can add its pages.
func (s *Server) Echo() *echo.Echo { return s.echo }

// registerDeployRoutes adds the routes that deploy apps and mint admin keys,
// behind whatever authentication v1 already has.
func (s *Server) registerDeployRoutes(v1 *echo.Group) {
	// Application operations. They have no owner, so only API keys without a
	// user (admin) may use them; users deploy through projects.
	apps := v1.Group("/applications", s.adminKeyOnlyMiddleware)
	apps.POST("", s.deployApplication)
	apps.DELETE("/:id", s.deleteApplication)
	apps.GET("/:id/status", s.getApplicationStatus)

	// Projects and deployments (a user's JWT or API key)
	projects := v1.Group("/projects", s.userOnlyMiddleware)
	projects.POST("", s.createProject)
	projects.GET("", s.listProjects)
	projects.GET("/:id", s.getProject)
	projects.DELETE("/:id", s.deleteProject)
	projects.GET("/:id/deployments", s.listDeployments)
	projects.POST("/:id/deployments", s.deployImage)
	projects.POST("/:id/redeploy", s.redeploy)
	projects.POST("/:id/rollback", s.rollback)
	projects.GET("/:id/env", s.listEnv)
	projects.PUT("/:id/env/:name", s.setEnv)
	projects.DELETE("/:id/env/:name", s.deleteEnv)
	v1.GET("/deployments/:id", s.getDeployment, s.userOnlyMiddleware)

	// Admin operations: only an API key that belongs to no user. Otherwise any
	// user could mint an ownerless key and use it on the ownerless endpoints.
	admin := v1.Group("/admin", s.adminKeyOnlyMiddleware)
	admin.POST("/users", s.adminCreateUser)
	admin.POST("/keys", s.createAPIKey)
	admin.GET("/keys", s.listAPIKeys)
	admin.DELETE("/keys/:id", s.deleteAPIKey)
}

// Start starts the REST API server
func (s *Server) Start() error {
	addr := fmt.Sprintf(":%s", s.port)
	return s.echo.Start(addr)
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.tracerProvider.Shutdown(ctx); err != nil {
		log.Printf("Error shutting down tracer provider: %v", err)
	}
	return s.echo.Shutdown(ctx)
}

// healthCheck handles health check requests
// @Summary Health check
// @Description Check if the server is healthy
// @Tags system
// @Accept json
// @Produce json
// @Success 200 {object} map[string]string
// @Router /health [get]
func (s *Server) healthCheck(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{
		"status": "healthy",
	})
}

// initTracer initializes the OpenTelemetry tracer
func initTracer() *sdktrace.TracerProvider {
	otel.SetLogger(stdr.New(log.New(os.Stderr, "", log.LstdFlags|log.Lshortfile)))

	ctx := context.Background()
	otlpExporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithTLSClientConfig(&tls.Config{InsecureSkipVerify: true}),
	)
	if err != nil {
		log.Fatal(err)
	}

	stdoutExporter, err := stdout.New(stdout.WithPrettyPrint())
	if err != nil {
		log.Fatal(err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(otlpExporter),
		sdktrace.WithBatcher(stdoutExporter),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp
}
