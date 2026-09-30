package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/vorash/vora-cloud/pkg/database"
)

const (
	bcryptCost = 12
	// maxConcurrentChecks bounds simultaneous password checks.
	maxConcurrentChecks = 4
)

// UserService user authentication operations
type UserService struct {
	userRepo         *database.UserRepository
	refreshTokenRepo *database.RefreshTokenRepository
	jwtService       *JWTService
	// hashSlots caps concurrent bcrypt checks: each takes a core for a while,
	// and sign-in is reachable without credentials.
	hashSlots chan struct{}
}

func NewUserService(
	userRepo *database.UserRepository,
	refreshTokenRepo *database.RefreshTokenRepository,
	jwtService *JWTService,
) *UserService {
	return &UserService{
		userRepo:         userRepo,
		refreshTokenRepo: refreshTokenRepo,
		jwtService:       jwtService,
		hashSlots:        make(chan struct{}, maxConcurrentChecks),
	}
}

// RegisterUser registers a new user with email and password
func (s *UserService) RegisterUser(ctx context.Context, name, email, password string) (*database.User, error) {
	existingUser, err := s.userRepo.GetByEmail(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("failed to check existing user: %w", err)
	}
	if existingUser != nil {
		return nil, fmt.Errorf("user with email %s already exists", email)
	}

	passwordHash, err := hashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	user, err := s.userRepo.Create(ctx, name, email, passwordHash)
	if err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	return user, nil
}

// ErrInvalidCredentials is the one answer for an unknown email, a wrong
// password and an inactive account, so a sign-in form does not reveal which
// accounts exist.
var ErrInvalidCredentials = errors.New("invalid email or password")

// dummyHash is compared against when the email is unknown, so an unknown
// email costs the same time as a wrong password.
var dummyHash = func() string {
	h, err := hashPassword("no such user")
	if err != nil {
		panic(err)
	}
	return h
}()

// AuthenticateUser checks an email and password and returns the user. Unlike
// LoginUser it issues no tokens: browser sessions are kept by the dashboard.
func (s *UserService) AuthenticateUser(ctx context.Context, email, password string) (*database.User, error) {
	user, err := s.userRepo.GetByEmail(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	hash := dummyHash
	if user != nil {
		hash = user.PasswordHash
	}
	select {
	case s.hashSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	matches := verifyPassword(password, hash)
	<-s.hashSlots
	if user == nil || !user.IsActive || !matches {
		return nil, ErrInvalidCredentials
	}
	if err := s.userRepo.UpdateLastLogin(ctx, user.ID); err != nil {
		log.Printf("failed to update last login for user %s: %v", user.ID, err)
	}
	return user, nil
}

// LoginUser authenticates a user and returns access and refresh tokens. A bad
// email, a wrong password and an inactive account all return
// ErrInvalidCredentials, so the API does not reveal which accounts exist.
func (s *UserService) LoginUser(ctx context.Context, email, password string) (accessToken, refreshToken string, user *database.User, err error) {
	user, err = s.AuthenticateUser(ctx, email, password)
	if err != nil {
		return "", "", nil, err
	}

	accessToken, err = s.jwtService.GenerateAccessToken(user.ID, user.Email, user.Name)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to generate access token: %w", err)
	}

	refreshToken, err = s.jwtService.GenerateRefreshToken()
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to generate refresh token: %w", err)
	}

	tokenHash := hashRefreshToken(refreshToken)
	expiresAt := s.jwtService.GetRefreshTokenExpiry()

	_, err = s.refreshTokenRepo.Create(ctx, user.ID, tokenHash, expiresAt)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to store refresh token: %w", err)
	}

	return accessToken, refreshToken, user, nil
}

// RefreshAccessToken generates a new access token using a refresh token
func (s *UserService) RefreshAccessToken(ctx context.Context, refreshToken string) (accessToken string, newRefreshToken string, err error) {
	tokenHash := hashRefreshToken(refreshToken)

	storedToken, err := s.refreshTokenRepo.GetByTokenHash(ctx, tokenHash)
	if err != nil {
		return "", "", fmt.Errorf("failed to get refresh token: %w", err)
	}
	if storedToken == nil {
		return "", "", fmt.Errorf("invalid refresh token")
	}

	if time.Now().After(storedToken.ExpiresAt) {
		return "", "", fmt.Errorf("refresh token expired")
	}

	if storedToken.IsRevoked {
		return "", "", fmt.Errorf("refresh token has been revoked")
	}

	user, err := s.userRepo.GetByID(ctx, storedToken.UserID)
	if err != nil {
		return "", "", fmt.Errorf("failed to get user: %w", err)
	}
	if user == nil {
		return "", "", fmt.Errorf("user not found")
	}

	if !user.IsActive {
		return "", "", fmt.Errorf("user account is inactive")
	}

	accessToken, err = s.jwtService.GenerateAccessToken(user.ID, user.Email, user.Name)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate access token: %w", err)
	}

	newRefreshToken, err = s.jwtService.GenerateRefreshToken()
	if err != nil {
		return "", "", fmt.Errorf("failed to generate refresh token: %w", err)
	}

	if err := s.refreshTokenRepo.Revoke(ctx, tokenHash); err != nil {
		return "", "", fmt.Errorf("failed to revoke old refresh token: %w", err)
	}

	newTokenHash := hashRefreshToken(newRefreshToken)
	expiresAt := s.jwtService.GetRefreshTokenExpiry()
	_, err = s.refreshTokenRepo.Create(ctx, user.ID, newTokenHash, expiresAt)
	if err != nil {
		return "", "", fmt.Errorf("failed to store new refresh token: %w", err)
	}

	return accessToken, newRefreshToken, nil
}

// LogoutUser revokes a user's refresh token
func (s *UserService) LogoutUser(ctx context.Context, refreshToken string) error {
	tokenHash := hashRefreshToken(refreshToken)
	if err := s.refreshTokenRepo.Revoke(ctx, tokenHash); err != nil {
		return fmt.Errorf("failed to revoke refresh token: %w", err)
	}
	return nil
}

// LogoutAllSessions revokes all refresh tokens for a user
func (s *UserService) LogoutAllSessions(ctx context.Context, userID uuid.UUID) error {
	if err := s.refreshTokenRepo.RevokeAllByUserID(ctx, userID); err != nil {
		return fmt.Errorf("failed to revoke all tokens: %w", err)
	}
	return nil
}

// ValidateAccessToken validates a JWT access token and returns the claims
func (s *UserService) ValidateAccessToken(tokenString string) (*JWTClaims, error) {
	return s.jwtService.ValidateToken(tokenString)
}

// GetUserByID retrieves a user by ID
func (s *UserService) GetUserByID(ctx context.Context, userID uuid.UUID) (*database.User, error) {
	return s.userRepo.GetByID(ctx, userID)
}

// ChangePassword changes a user's password
func (s *UserService) ChangePassword(ctx context.Context, userID uuid.UUID, oldPassword, newPassword string) error {
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}
	if user == nil {
		return fmt.Errorf("user not found")
	}

	if !verifyPassword(oldPassword, user.PasswordHash) {
		return fmt.Errorf("invalid old password")
	}

	newPasswordHash, err := hashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("failed to hash new password: %w", err)
	}

	if err := s.userRepo.UpdatePassword(ctx, userID, newPasswordHash); err != nil {
		return fmt.Errorf("failed to update password: %w", err)
	}

	if err := s.refreshTokenRepo.RevokeAllByUserID(ctx, userID); err != nil {
		fmt.Printf("Warning: failed to revoke tokens for user %s: %v\n", userID, err)
	}

	return nil
}

// VerifyEmail marks a user's email as verified
func (s *UserService) VerifyEmail(ctx context.Context, userID uuid.UUID) error {
	return s.userRepo.UpdateEmailVerified(ctx, userID, true)
}

// DeactivateUser deactivates a user account
func (s *UserService) DeactivateUser(ctx context.Context, userID uuid.UUID) error {
	if err := s.userRepo.UpdateActive(ctx, userID, false); err != nil {
		return fmt.Errorf("failed to deactivate user: %w", err)
	}

	if err := s.refreshTokenRepo.RevokeAllByUserID(ctx, userID); err != nil {
		return fmt.Errorf("failed to revoke tokens: %w", err)
	}

	return nil
}

// hashPassword hashes a password using bcrypt
func hashPassword(password string) (string, error) {
	hashedBytes, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hashedBytes), nil
}

// verifyPassword verifies a password against a bcrypt hash
func verifyPassword(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// hashRefreshToken hashes a refresh token for storage
func hashRefreshToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}
