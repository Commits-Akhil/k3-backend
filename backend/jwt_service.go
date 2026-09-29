package main

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	jwtIssuer             = "cyberlab"
	jwtAudience           = "cyberlab-api"
	defaultAccessTokenTTL = 15 * time.Minute
)

var ErrInvalidToken = errors.New("invalid access token")

type AuthenticatedIdentity struct {
	UserID uuid.UUID
}

type JWTService struct {
	secret         []byte
	issuer         string
	audience       string
	accessTokenTTL time.Duration
	now            func() time.Time
}

func NewJWTService(config JWTConfig) (*JWTService, error) {
	if len(config.SigningSecret) < minimumJWTSecretBytes {
		return nil, errors.New("JWT signing secret is invalid")
	}
	return &JWTService{
		secret:         []byte(config.SigningSecret),
		issuer:         jwtIssuer,
		audience:       jwtAudience,
		accessTokenTTL: defaultAccessTokenTTL,
		now:            time.Now,
	}, nil
}

func (service *JWTService) IssueAccessToken(userID uuid.UUID) (string, error) {
	if service == nil || userID == uuid.Nil {
		return "", ErrInvalidInput
	}

	issuedAt := service.now().UTC()
	claims := jwt.RegisteredClaims{
		Subject:   userID.String(),
		Issuer:    service.issuer,
		Audience:  jwt.ClaimStrings{service.audience},
		IssuedAt:  jwt.NewNumericDate(issuedAt),
		ExpiresAt: jwt.NewNumericDate(issuedAt.Add(service.accessTokenTTL)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	serialized, err := token.SignedString(service.secret)
	if err != nil {
		return "", ErrInvalidToken
	}
	return serialized, nil
}

func (service *JWTService) AccessTokenLifetime() time.Duration {
	if service == nil {
		return 0
	}
	return service.accessTokenTTL
}

func (service *JWTService) ValidateAccessToken(tokenString string) (AuthenticatedIdentity, error) {
	if service == nil || tokenString == "" {
		return AuthenticatedIdentity{}, ErrInvalidToken
	}

	claims := &jwt.RegisteredClaims{}
	token, err := jwt.ParseWithClaims(
		tokenString,
		claims,
		func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, ErrInvalidToken
			}
			return service.secret, nil
		},
		jwt.WithIssuer(service.issuer),
		jwt.WithAudience(service.audience),
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithTimeFunc(service.now),
	)
	if err != nil || token == nil || !token.Valid {
		return AuthenticatedIdentity{}, ErrInvalidToken
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil || userID == uuid.Nil {
		return AuthenticatedIdentity{}, ErrInvalidToken
	}
	return AuthenticatedIdentity{UserID: userID}, nil
}
