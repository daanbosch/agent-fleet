// dev_idp_keys.go — the development OIDC IdP's signing keys (ADR 0116 decision 4).
//
// One RS256 key signs; every key that has not been dropped yet is published in the JWKS, so a
// token signed just before a rotation still verifies. The private half exists in three places
// only: the sealed column of dev_idp_key, this process's memory, and the signature it makes.
// It is never logged, returned, or written to an audit row — errors from this file carry the
// operation, not the key.
package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

const (
	// devIdPKeyRotateAfter is how old the signing key gets before a new one replaces it.
	devIdPKeyRotateAfter = 30 * 24 * time.Hour
	// devIdPKeyRetainFor is how long a retired key stays in the JWKS: far longer than any token
	// lives (devIdPTokenTTL), so rotation never invalidates a token that is still valid.
	devIdPKeyRetainFor = 24 * time.Hour
	// devIdPKeyRefresh bounds how stale this process's view of the key table may be. Another
	// Control Plane may have rotated; its tokens verify through the JWKS either way.
	devIdPKeyRefresh = time.Minute
	// devIdPKeyRef is the custodian key the signing keys are sealed under. They belong to the
	// deployment, not to a tenant (the same reason as engineHfTokenKeyRef).
	devIdPKeyRef = "deployment"
)

var devIdPB64 = base64.RawURLEncoding

// devIdPSigner is the key currently used to sign.
type devIdPSigner struct {
	kid string
	key *rsa.PrivateKey
}

// devIdPSealer is the part of the manager the keyring needs.
type devIdPSealer interface {
	sealTenantSecret(ctx context.Context, keyRef, secret string) (enc, ref string, err error)
	openTenantSecret(ctx context.Context, enc, keyRef string) (string, error)
}

type devIdPKeyring struct {
	mu       sync.Mutex
	store    store.DevIdPStore
	sealer   devIdPSealer
	now      func() time.Time
	cached   *devIdPSigner
	cachedAt time.Time
	createdA time.Time // creation time of the cached key
}

func newDevIdPKeyring(st store.DevIdPStore, sealer devIdPSealer, now func() time.Time) *devIdPKeyring {
	return &devIdPKeyring{store: st, sealer: sealer, now: now}
}

// signer returns the active key, creating the first one and rotating an old one on demand.
func (k *devIdPKeyring) signer(ctx context.Context) (devIdPSigner, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	if k.cached != nil && now.Sub(k.cachedAt) < devIdPKeyRefresh && now.Sub(k.createdA) < devIdPKeyRotateAfter {
		return *k.cached, nil
	}
	keys, err := k.store.ListDevIdPKeys(ctx)
	if err != nil {
		return devIdPSigner{}, fmt.Errorf("dev idp: list keys: %w", err)
	}
	var active *store.DevIdPKey
	for i := range keys {
		if keys[i].State == store.DevIdPKeyActive {
			active = &keys[i] // newest first
			break
		}
	}
	if active == nil || now.Sub(time.Unix(active.CreatedAt, 0)) >= devIdPKeyRotateAfter {
		fresh, err := k.mint(ctx, now)
		if err != nil {
			return devIdPSigner{}, err
		}
		active = &fresh
		if err := k.store.RetireDevIdPKeys(ctx, fresh.KID, now.Unix()); err != nil {
			return devIdPSigner{}, fmt.Errorf("dev idp: retire keys: %w", err)
		}
	}
	// Drop what has been retired for long enough that no token it signed can still be live.
	if err := k.store.DeleteRetiredDevIdPKeys(ctx, now.Add(-devIdPKeyRetainFor).Unix()); err != nil {
		return devIdPSigner{}, fmt.Errorf("dev idp: drop retired keys: %w", err)
	}
	priv, err := k.open(ctx, *active)
	if err != nil {
		return devIdPSigner{}, err
	}
	k.cached = &devIdPSigner{kid: active.KID, key: priv}
	k.cachedAt = now
	k.createdA = time.Unix(active.CreatedAt, 0)
	return *k.cached, nil
}

func (k *devIdPKeyring) mint(ctx context.Context, now time.Time) (store.DevIdPKey, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return store.DevIdPKey{}, fmt.Errorf("dev idp: generate key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return store.DevIdPKey{}, fmt.Errorf("dev idp: encode key: %w", err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return store.DevIdPKey{}, fmt.Errorf("dev idp: encode public key: %w", err)
	}
	enc, ref, err := k.sealer.sealTenantSecret(ctx, devIdPKeyRef, base64.StdEncoding.EncodeToString(der))
	if err != nil {
		return store.DevIdPKey{}, fmt.Errorf("dev idp: seal key: %w", err)
	}
	// The kid is the thumbprint of the public key, so it names the key and nothing else.
	sum := sha256.Sum256(pub)
	row := store.DevIdPKey{
		KID: devIdPB64.EncodeToString(sum[:12]), PrivateEnc: enc, KeyRef: ref,
		PublicKey: base64.StdEncoding.EncodeToString(pub),
		State:     store.DevIdPKeyActive, CreatedAt: now.Unix(),
	}
	if err := k.store.InsertDevIdPKey(ctx, row); err != nil {
		return store.DevIdPKey{}, fmt.Errorf("dev idp: store key: %w", err)
	}
	return row, nil
}

func (k *devIdPKeyring) open(ctx context.Context, row store.DevIdPKey) (*rsa.PrivateKey, error) {
	plain, err := k.sealer.openTenantSecret(ctx, row.PrivateEnc, row.KeyRef)
	if err != nil {
		return nil, fmt.Errorf("dev idp: unseal key %s: %w", row.KID, err)
	}
	der, err := base64.StdEncoding.DecodeString(plain)
	if err != nil {
		return nil, fmt.Errorf("dev idp: key %s is not decodable", row.KID)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("dev idp: key %s is not parseable", row.KID)
	}
	priv, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("dev idp: stored key is not RSA")
	}
	return priv, nil
}

// sign makes a compact RS256 JWT over claims.
func (s devIdPSigner) sign(claims any) (string, error) {
	hdr, err := json.Marshal(devIdPJWTHeader{Alg: "RS256", Typ: "JWT", Kid: s.kid})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := devIdPB64.EncodeToString(hdr) + "." + devIdPB64.EncodeToString(body)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + devIdPB64.EncodeToString(sig), nil
}

type devIdPJWTHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// devIdPJWK / devIdPJWKS are the published key set (RFC 7517).
type devIdPJWK struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type devIdPJWKS struct {
	Keys []devIdPJWK `json:"keys"`
}

// jwks is the public half of every key not yet dropped.
func (k *devIdPKeyring) jwks(ctx context.Context) (devIdPJWKS, error) {
	// A first request may find no key at all; make sure one exists so a verifier that fetches
	// the JWKS before any token was issued does not cache an empty set.
	if _, err := k.signer(ctx); err != nil {
		return devIdPJWKS{}, err
	}
	rows, err := k.store.ListDevIdPKeys(ctx)
	if err != nil {
		return devIdPJWKS{}, err
	}
	keys := make([]devIdPJWK, 0, len(rows))
	for _, row := range rows {
		der, err := base64.StdEncoding.DecodeString(row.PublicKey)
		if err != nil {
			continue
		}
		parsed, err := x509.ParsePKIXPublicKey(der)
		pub, ok := parsed.(*rsa.PublicKey)
		if err != nil || !ok {
			continue
		}
		keys = append(keys, devIdPJWK{
			Kty: "RSA", Use: "sig", Alg: "RS256", Kid: row.KID,
			N: devIdPB64.EncodeToString(pub.N.Bytes()),
			E: devIdPB64.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		})
	}
	return devIdPJWKS{Keys: keys}, nil
}
