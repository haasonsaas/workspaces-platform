package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type jobIdentity struct {
	Namespace string
	Name      string
}

type jobTokenClaims struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	jwt.RegisteredClaims
}

func (s *server) mintJobToken(namespace, name string, ttl time.Duration) (string, error) {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(name) == "" {
		return "", errors.New("namespace and name are required")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	now := time.Now()
	claims := jobTokenClaims{
		Namespace: strings.TrimSpace(namespace),
		Name:      strings.TrimSpace(name),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("agentjob:%s/%s", strings.TrimSpace(namespace), strings.TrimSpace(name)),
			IssuedAt:  jwt.NewNumericDate(now.Add(-30 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Audience:  jwt.ClaimStrings{"capability-broker"},
			ID:        randomSlug(),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(s.jobJWTSecret)
}

func (s *server) parseJobToken(r *http.Request) (*jobIdentity, error) {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(raw), "bearer ") {
		raw = strings.TrimSpace(raw[7:])
	} else {
		raw = ""
	}
	if raw == "" {
		raw = strings.TrimSpace(r.Header.Get("X-Workspaces-Job-Token"))
	}
	if raw == "" {
		return nil, errors.New("missing token")
	}

	claims := &jobTokenClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if t.Method == nil || t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, errors.New("unexpected signing method")
		}
		return s.jobJWTSecret, nil
	}, jwt.WithAudience("capability-broker"), jwt.WithLeeway(30*time.Second))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(claims.Namespace) == "" || strings.TrimSpace(claims.Name) == "" {
		return nil, errors.New("invalid token claims")
	}
	return &jobIdentity{Namespace: strings.TrimSpace(claims.Namespace), Name: strings.TrimSpace(claims.Name)}, nil
}

func (s *server) requireJobOrAdmin(r *http.Request, requiredJobName string) error {
	// Admin token is a superset.
	if s.adminToken != "" {
		if strings.TrimSpace(r.Header.Get("X-Broker-Admin-Token")) == s.adminToken {
			return nil
		}
	}

	id, err := s.parseJobToken(r)
	if err != nil {
		return err
	}
	if strings.TrimSpace(id.Namespace) != "agents" {
		return errors.New("job token namespace not allowed")
	}
	if strings.TrimSpace(requiredJobName) != "" && id.Name != strings.TrimSpace(requiredJobName) {
		return errors.New("job token does not match agent job")
	}
	return nil
}
