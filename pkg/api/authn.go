package api

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/vorash/vora-cloud/api/proto"
	"github.com/vorash/vora-cloud/pkg/auth"
	"github.com/vorash/vora-cloud/pkg/database"
)

// Access is what a gRPC method requires of the caller.
type Access int

const (
	// Public needs no credentials.
	Public Access = iota + 1
	// Admin needs an API key that belongs to no user. These methods have no
	// owner: they map a name straight to a namespace.
	Admin
	// User needs a user, through a JWT or a user's API key.
	User
)

// methodAccess lists every method. A method that is not listed is refused, so
// a new RPC is closed until someone decides who may call it.
var methodAccess = map[string]Access{
	pb.ControlPlane_HealthCheck_FullMethodName:          Public,
	pb.ControlPlane_DeployApplication_FullMethodName:    Admin,
	pb.ControlPlane_DeleteApplication_FullMethodName:    Admin,
	pb.ControlPlane_GetApplicationStatus_FullMethodName: Admin,
	pb.ControlPlane_GetApplicationLogs_FullMethodName:   Admin,

	pb.Projects_CreateProject_FullMethodName:   User,
	pb.Projects_ListProjects_FullMethodName:    User,
	pb.Projects_GetProject_FullMethodName:      User,
	pb.Projects_DeleteProject_FullMethodName:   User,
	pb.Projects_ListDeployments_FullMethodName: User,
	pb.Projects_GetDeployment_FullMethodName:   User,
	pb.Projects_DeployImage_FullMethodName:     User,
	pb.Projects_Redeploy_FullMethodName:        User,
	pb.Projects_Rollback_FullMethodName:        User,
}

type tokenValidator interface {
	ValidateAccessToken(token string) (*auth.JWTClaims, error)
}

type keyValidator interface {
	ValidateKey(key string) (*database.APIKey, error)
}

type userContextKey struct{}

// UserFromContext returns the user the interceptor authenticated.
func UserFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userContextKey{}).(uuid.UUID)
	return id, ok
}

// AuthInterceptor authenticates gRPC calls the way the REST API does: a JWT in
// "authorization: Bearer <jwt>" or an API key in "x-api-key", then checks the
// method's Access.
func AuthInterceptor(tokens tokenValidator, keys keyValidator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		access, ok := methodAccess[info.FullMethod]
		if !ok {
			return nil, status.Error(codes.PermissionDenied, "method is not available")
		}
		if access == Public {
			return handler(ctx, req)
		}

		user, isKey, err := authenticate(ctx, tokens, keys)
		if err != nil {
			return nil, err
		}
		switch access {
		case Admin:
			if user != nil || !isKey {
				return nil, status.Error(codes.PermissionDenied, "this method needs an admin API key")
			}
		case User:
			if user == nil {
				return nil, status.Error(codes.PermissionDenied, "this method needs a user account")
			}
			ctx = context.WithValue(ctx, userContextKey{}, *user)
		}
		return handler(ctx, req)
	}
}

// authenticate returns the caller's user id (nil for an admin key) and whether
// the credential was an API key.
func authenticate(ctx context.Context, tokens tokenValidator, keys keyValidator) (user *uuid.UUID, isKey bool, err error) {
	md, _ := metadata.FromIncomingContext(ctx)

	if h := md.Get("authorization"); len(h) > 0 {
		token, found := strings.CutPrefix(h[0], "Bearer ")
		if !found {
			return nil, false, status.Error(codes.Unauthenticated, "authorization must be a Bearer token")
		}
		claims, err := tokens.ValidateAccessToken(token)
		if err != nil {
			return nil, false, status.Error(codes.Unauthenticated, "invalid or expired token")
		}
		id, err := uuid.Parse(claims.UserID)
		if err != nil {
			return nil, false, status.Error(codes.Unauthenticated, "invalid or expired token")
		}
		return &id, false, nil
	}

	if k := md.Get("x-api-key"); len(k) > 0 {
		info, err := keys.ValidateKey(k[0])
		if err != nil {
			return nil, false, status.Error(codes.Unauthenticated, "invalid or inactive API key")
		}
		return info.UserID, true, nil
	}

	return nil, false, status.Error(codes.Unauthenticated, "missing authentication credentials")
}
