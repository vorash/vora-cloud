package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/vorash/vora-cloud/api/proto"
	"github.com/vorash/vora-cloud/pkg/auth"
	"github.com/vorash/vora-cloud/pkg/database"
)

var (
	alice    = uuid.New()
	aliceKey = uuid.New()
)

type fakeTokens struct{}

func (fakeTokens) ValidateAccessToken(token string) (*auth.JWTClaims, error) {
	if token == "alice-jwt" {
		return &auth.JWTClaims{UserID: alice.String()}, nil
	}
	return nil, errors.New("bad token")
}

type fakeKeys struct{}

func (fakeKeys) ValidateKey(key string) (*database.APIKey, error) {
	switch key {
	case "admin-key":
		return &database.APIKey{}, nil // belongs to no user
	case "alice-key":
		return &database.APIKey{UserID: &aliceKey}, nil
	}
	return nil, errors.New("bad key")
}

// call runs a method through the interceptor with the given metadata and
// returns the status code and the user the handler saw.
func call(method string, md metadata.MD) (codes.Code, uuid.UUID, bool) {
	var seen uuid.UUID
	var ok bool
	handler := func(ctx context.Context, _ any) (any, error) {
		seen, ok = UserFromContext(ctx)
		return nil, nil
	}
	ctx := metadata.NewIncomingContext(context.Background(), md)
	_, err := AuthInterceptor(fakeTokens{}, fakeKeys{})(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
	return status.Code(err), seen, ok
}

var (
	deployApp = pb.ControlPlane_DeployApplication_FullMethodName
	getProj   = pb.Projects_GetProject_FullMethodName
)

func TestHealthCheckIsPublic(t *testing.T) {
	if code, _, _ := call(pb.ControlPlane_HealthCheck_FullMethodName, nil); code != codes.OK {
		t.Errorf("got %v", code)
	}
}

// Every method of both services must be classified. A new RPC that nobody
// classified is closed to everyone, and this test names it.
func TestEveryRPCHasAnAccessRule(t *testing.T) {
	for _, svc := range []grpc.ServiceDesc{pb.ControlPlane_ServiceDesc, pb.Projects_ServiceDesc} {
		for _, m := range svc.Methods {
			full := "/" + svc.ServiceName + "/" + m.MethodName
			if _, ok := methodAccess[full]; !ok {
				t.Errorf("%s has no access rule", full)
			}
		}
	}
	if code, _, _ := call("/controlplane.ControlPlane/Unlisted", metadata.Pairs("x-api-key", "admin-key")); code != codes.PermissionDenied {
		t.Errorf("unlisted method: %v, want PermissionDenied even for an admin key", code)
	}
}

func TestProjectMethodsNeedAUser(t *testing.T) {
	for name, md := range map[string]metadata.MD{
		"no credentials": nil,
		"bad token":      metadata.Pairs("authorization", "Bearer nope"),
		"not a bearer":   metadata.Pairs("authorization", "alice-jwt"),
		"bad key":        metadata.Pairs("x-api-key", "nope"),
		"empty key":      metadata.Pairs("x-api-key", ""),
	} {
		if code, _, _ := call(getProj, md); code != codes.Unauthenticated {
			t.Errorf("%s: %v, want Unauthenticated", name, code)
		}
	}
	// An admin key belongs to no user, so it owns no projects.
	if code, _, _ := call(getProj, metadata.Pairs("x-api-key", "admin-key")); code != codes.PermissionDenied {
		t.Errorf("admin key on a project method: %v, want PermissionDenied", code)
	}
}

func TestUsersReachProjectMethodsAsThemselves(t *testing.T) {
	code, user, ok := call(getProj, metadata.Pairs("authorization", "Bearer alice-jwt"))
	if code != codes.OK || !ok || user != alice {
		t.Errorf("jwt: %v %v %v", code, user, ok)
	}
	code, user, ok = call(getProj, metadata.Pairs("x-api-key", "alice-key"))
	if code != codes.OK || !ok || user != aliceKey {
		t.Errorf("user key: %v %v %v", code, user, ok)
	}
}

// The gRPC face of "admin keys only": deploy, delete and status map a name
// straight to a namespace, so a user must not reach them.
func TestApplicationMethodsNeedAnAdminKey(t *testing.T) {
	if code, _, _ := call(deployApp, nil); code != codes.Unauthenticated {
		t.Errorf("anonymous: %v, want Unauthenticated", code)
	}
	if code, _, _ := call(deployApp, metadata.Pairs("authorization", "Bearer alice-jwt")); code != codes.PermissionDenied {
		t.Errorf("user jwt: %v, want PermissionDenied", code)
	}
	if code, _, _ := call(deployApp, metadata.Pairs("x-api-key", "alice-key")); code != codes.PermissionDenied {
		t.Errorf("user api key: %v, want PermissionDenied", code)
	}
	if code, _, _ := call(deployApp, metadata.Pairs("x-api-key", "admin-key")); code != codes.OK {
		t.Errorf("admin key: %v, want OK", code)
	}
}
