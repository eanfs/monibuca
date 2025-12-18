package m7s

import (
	"context"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	task "github.com/langhuihui/gotask"
	"m7s.live/v5/pb"
	. "m7s.live/v5/pkg"
	"m7s.live/v5/pkg/auth"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/db"
)

// context key type & keys
type ctxKey int

const (
	ctxKeyClaims ctxKey = iota
)

type GRPCServer struct {
	task.Task
	s       *Server
	tcpTask *config.ListenTCPWork
}

func (gRPC *GRPCServer) Dispose() {
	gRPC.s.Stop(gRPC.StopReason())
}

func (gRPC *GRPCServer) Go() (err error) {
	return gRPC.s.grpcServer.Serve(gRPC.tcpTask.Listener)
}

// ValidateToken implements auth.TokenValidator
func (s *Server) ValidateToken(tokenString string) (*auth.JWTClaims, error) {
	if !s.ServerConfig.Admin.EnableLogin {
		return &auth.JWTClaims{Username: "anonymous"}, nil
	}
	return auth.ValidateJWT(tokenString)
}

// Login implements the Login RPC method
func (s *Server) Login(ctx context.Context, req *pb.LoginRequest) (res *pb.LoginResponse, err error) {
	res = &pb.LoginResponse{}
	if !s.ServerConfig.Admin.EnableLogin {
		res.Data = &pb.LoginSuccess{
			Token: "monibuca",
			UserInfo: &pb.UserInfo{
				Username:  "anonymous",
				ExpiresAt: time.Now().Add(24 * time.Hour).Unix(),
			},
		}
		return
	}
	if s.DB == nil {
		err = ErrNoDB
		return
	}
	var user db.User
	if err = s.DB.Where("username = ?", req.Username).First(&user).Error; err != nil {
		return
	}

	if !user.CheckPassword(req.Password) {
		err = ErrInvalidCredentials
		return
	}

	// Generate JWT token
	var tokenString string
	tokenString, err = auth.GenerateToken(user.Username)
	if err != nil {
		return
	}

	// Update last login time
	s.DB.Model(&user).Update("last_login", time.Now())
	res.Data = &pb.LoginSuccess{
		Token: tokenString,
		UserInfo: &pb.UserInfo{
			Username:  user.Username,
			ExpiresAt: time.Now().Add(24 * time.Hour).Unix(),
		},
	}
	return
}

// Logout implements the Logout RPC method
func (s *Server) Logout(ctx context.Context, req *pb.LogoutRequest) (res *pb.LogoutResponse, err error) {
	// In a more complex system, you might want to maintain a blacklist of logged-out tokens
	// For now, we'll just return success as JWT tokens are stateless
	res = &pb.LogoutResponse{Code: 0, Message: "success"}
	return
}

// GetUserInfo implements the GetUserInfo RPC method
func (s *Server) GetUserInfo(ctx context.Context, req *pb.UserInfoRequest) (res *pb.UserInfoResponse, err error) {
	if !s.ServerConfig.Admin.EnableLogin {
		res = &pb.UserInfoResponse{
			Code:    0,
			Message: "success",
			Data: &pb.UserInfo{
				Username:  "anonymous",
				ExpiresAt: time.Now().Add(24 * time.Hour).Unix(),
			},
		}
		return
	}
	res = &pb.UserInfoResponse{}
	claims, err := s.ValidateToken(req.Token)
	if err != nil {
		err = ErrInvalidCredentials
		return
	}

	var user db.User
	if err = s.DB.Where("username = ?", claims.Username).First(&user).Error; err != nil {
		return
	}

	// Token is valid for 24 hours from now
	expiresAt := time.Now().Add(24 * time.Hour).Unix()

	return &pb.UserInfoResponse{
		Code:    0,
		Message: "success",
		Data: &pb.UserInfo{
			Username:  user.Username,
			ExpiresAt: expiresAt,
		},
	}, nil
}

// AuthInterceptor creates a new unary interceptor for authentication
func (s *Server) AuthInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if !s.ServerConfig.Admin.EnableLogin {
			return handler(ctx, req)
		}

		// Skip auth for login endpoint
		if info.FullMethod == "/pb.Auth/Login" {
			return handler(ctx, req)
		}

		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, errors.New("missing metadata")
		}

		authHeader := md.Get("authorization")
		if len(authHeader) == 0 {
			return nil, errors.New("missing authorization header")
		}

		tokenString := strings.TrimPrefix(authHeader[0], "Bearer ")
		claims, err := s.ValidateToken(tokenString)
		if err != nil {
			return nil, errors.New("invalid token")
		}

		// Check if token needs refresh
		shouldRefresh, err := auth.ShouldRefreshToken(tokenString)
		if err == nil && shouldRefresh {
			newToken, err := auth.RefreshToken(tokenString)
			if err == nil {
				// Add new token to response headers
				header := metadata.New(map[string]string{
					"new-token": newToken,
				})
				grpc.SetHeader(ctx, header)
			}
		}

		// Add claims to context
		newCtx := context.WithValue(ctx, ctxKeyClaims, claims)
		return handler(newCtx, req)
	}
}

