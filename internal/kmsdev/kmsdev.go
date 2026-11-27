// Package kmsdev is a key management service for the devnet that speaks the part of the
// AWS KMS API the withdrawal signer uses, so the signer runs the same client, the AWS
// SDK's, against it as against AWS KMS. It holds secp256k1 keys (ECC_SECG_P256K1, for
// SIGN_VERIFY) that never leave it: there is no call that returns a private key. Every
// request is authenticated with AWS Signature Version 4, and each principal may only
// make the calls, on the keys, its policy names, as an IAM key policy would allow.
//
// The calls: CreateKey, CreateAlias, UpdateAlias, DescribeKey, GetPublicKey and Sign (a
// digest, ECDSA_SHA_256, answered in ASN.1 DER as AWS KMS does). Anything else is
// refused.
package kmsdev

import (
	"context"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/ethereum/go-ethereum/crypto"
)

// Principal is a caller: its credentials and what it may do.
type Principal struct {
	Name, AccessKeyID, SecretAccessKey string
	// Actions it may take, such as kms:Sign.
	Actions []string
	// Keys it may use, by alias (alias/name); empty for any.
	Keys []string
}

// Server is the service. Keys are kept in a file in Dir, readable by its owner only.
type Server struct {
	Region     string
	Account    string
	Dir        string
	Principals []Principal
	// MaxSkew is how far a request's time may be from the server's.
	MaxSkew time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Observe, when set, is told of every call: the operation, the principal ("" when
	// none was recognised) and the outcome (Outcome).
	Observe func(operation, principal, outcome string)

	mu    sync.Mutex
	state *state
}

type key struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	// D is the private scalar, hex. It is written to the key file and nowhere else.
	D string `json:"d"`
}

type state struct {
	Keys    []key             `json:"keys"`
	Aliases map[string]string `json:"aliases"`
}

const stateFile = "keys.json"

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) load() (*state, error) {
	if s.state != nil {
		return s.state, nil
	}
	info, err := os.Stat(s.Dir)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("kmsdev: %s has mode %o, want 700", s.Dir, info.Mode().Perm())
	}
	st := &state{Aliases: map[string]string{}}
	raw, err := os.ReadFile(filepath.Join(s.Dir, stateFile))
	if err == nil {
		if err := json.Unmarshal(raw, st); err != nil {
			return nil, fmt.Errorf("kmsdev: read the keys: %w", err)
		}
		if st.Aliases == nil {
			st.Aliases = map[string]string{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	s.state = st
	return st, nil
}

func (s *Server) save(st *state) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	path := filepath.Join(s.Dir, stateFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// apiError is an error as AWS KMS answers it.
type apiError struct {
	status int
	kind   string
	msg    string
}

func (e *apiError) Error() string { return e.kind + ": " + e.msg }

func fail(status int, kind, format string, args ...any) *apiError {
	return &apiError{status: status, kind: kind, msg: fmt.Sprintf(format, args...)}
}

const maxBody = 64 << 10

// The outcomes of a call, as Observe is told them.
const (
	OutcomeOK = "ok"
	// OutcomeDenied is a caller that is not who it says, or asks what its policy does
	// not allow: the calls a security team is alerted to.
	OutcomeDenied = "denied"
	// OutcomeRefused is a call refused for what it asks, such as a key that does not
	// exist.
	OutcomeRefused = "refused"
	OutcomeError   = "error"
)

// denied are the errors of a caller that is not who it says, or of a call its policy
// does not allow.
var denied = map[string]bool{"AccessDeniedException": true, "IncompleteSignatureException": true, "InvalidSignatureException": true,
	"MissingAuthenticationTokenException": true, "UnrecognizedClientException": true}

// ServeHTTP answers one call of the AWS JSON 1.1 protocol.
func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	var who string
	out, err := s.serve(r, &who)
	rw.Header().Set("Content-Type", "application/x-amz-json-1.1")
	if s.Observe != nil {
		op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "TrentService.")
		if !served[op] {
			op = "other"
		}
		var ae *apiError
		outcome := OutcomeOK
		switch {
		case errors.As(err, &ae) && denied[ae.kind]:
			outcome = OutcomeDenied
		case errors.As(err, &ae):
			outcome = OutcomeRefused
		case err != nil:
			outcome = OutcomeError
		}
		s.Observe(op, who, outcome)
	}
	var ae *apiError
	if errors.As(err, &ae) {
		rw.Header().Set("X-Amzn-ErrorType", ae.kind)
		rw.WriteHeader(ae.status)
		_ = json.NewEncoder(rw).Encode(map[string]string{"__type": ae.kind, "message": ae.msg})
		return
	}
	if err != nil {
		// The caller learns nothing of it; the service's log says what failed.
		log.Printf("kmsdev: %s: %v", r.Header.Get("X-Amz-Target"), err)
		rw.Header().Set("X-Amzn-ErrorType", "KMSInternalException")
		rw.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(rw).Encode(map[string]string{"__type": "KMSInternalException", "message": "internal error"})
		return
	}
	_ = json.NewEncoder(rw).Encode(out)
}

// Operations are the calls the service serves.
var Operations = []string{"CreateKey", "CreateAlias", "UpdateAlias", "DescribeKey", "GetPublicKey", "Sign"}

var served = map[string]bool{"CreateKey": true, "CreateAlias": true, "UpdateAlias": true, "DescribeKey": true, "GetPublicKey": true, "Sign": true}

func (s *Server) serve(r *http.Request, who *string) (any, error) {
	if r.Method != http.MethodPost {
		return nil, fail(http.StatusBadRequest, "UnknownOperationException", "POST only")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return nil, fail(http.StatusBadRequest, "ValidationException", "the request body is too large or unreadable")
	}
	p, err := s.authenticate(r, body)
	if err != nil {
		return nil, err
	}
	*who = p.Name
	target := r.Header.Get("X-Amz-Target")
	op, ok := strings.CutPrefix(target, "TrentService.")
	if !ok {
		return nil, fail(http.StatusBadRequest, "UnknownOperationException", "unknown target %q", target)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	switch op {
	case "CreateKey":
		return s.createKey(p, st, body)
	case "CreateAlias":
		return s.createAlias(p, st, body)
	case "UpdateAlias":
		return s.updateAlias(p, st, body)
	case "DescribeKey":
		return s.describeKey(p, st, body)
	case "GetPublicKey":
		return s.getPublicKey(p, st, body)
	case "Sign":
		return s.sign(p, st, body)
	}
	return nil, fail(http.StatusBadRequest, "UnknownOperationException", "%s is not served", op)
}

var credentialPattern = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=([A-Z0-9]+)/(\d{8})/([a-z0-9-]+)/kms/aws4_request, ?SignedHeaders=([a-z0-9;-]+), ?Signature=([0-9a-f]{64})$`)

// authenticate checks the request's Signature Version 4 against the principal's secret,
// by signing the same request again, and its time against the clock.
func (s *Server) authenticate(r *http.Request, body []byte) (*Principal, error) {
	m := credentialPattern.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil {
		return nil, fail(http.StatusBadRequest, "MissingAuthenticationTokenException", "the request is not signed with Signature Version 4")
	}
	akid, region, signedHeaders, signature := m[1], m[3], m[4], m[5]
	var p *Principal
	for i := range s.Principals {
		if hmac.Equal([]byte(s.Principals[i].AccessKeyID), []byte(akid)) {
			p = &s.Principals[i]
		}
	}
	if p == nil || region != s.Region {
		return nil, fail(http.StatusBadRequest, "UnrecognizedClientException", "the security token included in the request is invalid")
	}
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return nil, fail(http.StatusBadRequest, "IncompleteSignatureException", "no X-Amz-Date")
	}
	skew := s.MaxSkew
	if skew == 0 {
		skew = 5 * time.Minute
	}
	if d := s.now().Sub(at); d > skew || d < -skew {
		return nil, fail(http.StatusBadRequest, "InvalidSignatureException", "signature expired: the request time is too far from the server's")
	}
	hash := sha256.Sum256(body)
	payload := hex.EncodeToString(hash[:])
	if h := r.Header.Get("X-Amz-Content-Sha256"); h != "" && h != payload {
		return nil, fail(http.StatusBadRequest, "InvalidSignatureException", "the payload hash does not match")
	}
	again, err := http.NewRequestWithContext(r.Context(), r.Method, "http://"+r.Host+r.URL.RequestURI(), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	again.Host = r.Host
	for _, h := range strings.Split(signedHeaders, ";") {
		if h == "host" {
			continue
		}
		for _, v := range r.Header.Values(h) {
			again.Header.Add(h, v)
		}
	}
	again.Header.Del("Authorization")
	creds := aws.Credentials{AccessKeyID: p.AccessKeyID, SecretAccessKey: p.SecretAccessKey}
	if err := v4.NewSigner().SignHTTP(context.Background(), creds, again, payload, "kms", s.Region, at); err != nil {
		return nil, err
	}
	want := credentialPattern.FindStringSubmatch(again.Header.Get("Authorization"))
	if want == nil || want[4] != signedHeaders || !hmac.Equal([]byte(want[5]), []byte(signature)) {
		return nil, fail(http.StatusBadRequest, "InvalidSignatureException", "the request signature does not match")
	}
	return p, nil
}

func (s *Server) allow(p *Principal, action string, keyID string, st *state) error {
	allowed := false
	for _, a := range p.Actions {
		if a == action {
			allowed = true
		}
	}
	if allowed && keyID != "" && len(p.Keys) > 0 {
		allowed = false
		for _, alias := range p.Keys {
			if st.Aliases[alias] == keyID {
				allowed = true
			}
		}
	}
	if !allowed {
		return fail(http.StatusBadRequest, "AccessDeniedException", "%s is not authorized to perform %s on this resource", p.Name, action)
	}
	return nil
}

func (s *Server) arn(id string) string {
	return fmt.Sprintf("arn:aws:kms:%s:%s:key/%s", s.Region, s.Account, id)
}

// resolve finds a key by id, ARN or alias.
func (s *Server) resolve(st *state, keyID string) (*key, error) {
	id := keyID
	if strings.HasPrefix(id, "alias/") {
		id = st.Aliases[id]
	} else if i := strings.LastIndex(id, ":key/"); i >= 0 {
		id = id[i+len(":key/"):]
	}
	for i := range st.Keys {
		if st.Keys[i].ID == id {
			return &st.Keys[i], nil
		}
	}
	return nil, fail(http.StatusBadRequest, "NotFoundException", "key %s does not exist", keyID)
}

func (k *key) private() (*ecdsa.PrivateKey, error) {
	return crypto.HexToECDSA(k.D)
}

func (s *Server) metadata(k *key) map[string]any {
	return map[string]any{
		"AWSAccountId": s.Account, "Arn": s.arn(k.ID), "KeyId": k.ID, "CreationDate": float64(k.Created.Unix()),
		"Enabled": true, "KeyState": "Enabled", "KeyManager": "CUSTOMER", "Origin": "AWS_KMS",
		"KeySpec": "ECC_SECG_P256K1", "CustomerMasterKeySpec": "ECC_SECG_P256K1", "KeyUsage": "SIGN_VERIFY",
		"SigningAlgorithms": []string{"ECDSA_SHA_256"},
	}
}

func decode(body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return fail(http.StatusBadRequest, "SerializationException", "the request does not decode")
	}
	return nil
}

func (s *Server) createKey(p *Principal, st *state, body []byte) (any, error) {
	if err := s.allow(p, "kms:CreateKey", "", st); err != nil {
		return nil, err
	}
	var in struct{ KeySpec, CustomerMasterKeySpec, KeyUsage string }
	if err := decode(body, &in); err != nil {
		return nil, err
	}
	spec := in.KeySpec
	if spec == "" {
		spec = in.CustomerMasterKeySpec
	}
	if spec != "ECC_SECG_P256K1" || in.KeyUsage != "SIGN_VERIFY" {
		return nil, fail(http.StatusBadRequest, "UnsupportedOperationException", "only ECC_SECG_P256K1 keys for SIGN_VERIFY are served")
	}
	priv, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	h := hex.EncodeToString(id)
	k := key{ID: h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], Created: s.now().UTC(), D: hex.EncodeToString(crypto.FromECDSA(priv))}
	st.Keys = append(st.Keys, k)
	if err := s.save(st); err != nil {
		return nil, err
	}
	return map[string]any{"KeyMetadata": s.metadata(&k)}, nil
}

var aliasPattern = regexp.MustCompile(`^alias/[a-zA-Z0-9/_-]{1,250}$`)

func (s *Server) createAlias(p *Principal, st *state, body []byte) (any, error) {
	if err := s.allow(p, "kms:CreateAlias", "", st); err != nil {
		return nil, err
	}
	var in struct{ AliasName, TargetKeyId string }
	if err := decode(body, &in); err != nil {
		return nil, err
	}
	if !aliasPattern.MatchString(in.AliasName) || strings.HasPrefix(in.AliasName, "alias/aws/") {
		return nil, fail(http.StatusBadRequest, "ValidationException", "alias %q is not valid", in.AliasName)
	}
	if _, taken := st.Aliases[in.AliasName]; taken {
		return nil, fail(http.StatusBadRequest, "AlreadyExistsException", "alias %s already exists", in.AliasName)
	}
	k, err := s.resolve(st, in.TargetKeyId)
	if err != nil {
		return nil, err
	}
	st.Aliases[in.AliasName] = k.ID
	if err := s.save(st); err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// updateAlias points an alias at another key: how a key is rotated, so that its users,
// who name it by the alias, take the new key at their next start.
func (s *Server) updateAlias(p *Principal, st *state, body []byte) (any, error) {
	if err := s.allow(p, "kms:UpdateAlias", "", st); err != nil {
		return nil, err
	}
	var in struct{ AliasName, TargetKeyId string }
	if err := decode(body, &in); err != nil {
		return nil, err
	}
	if _, ok := st.Aliases[in.AliasName]; !ok {
		return nil, fail(http.StatusBadRequest, "NotFoundException", "alias %s does not exist", in.AliasName)
	}
	k, err := s.resolve(st, in.TargetKeyId)
	if err != nil {
		return nil, err
	}
	st.Aliases[in.AliasName] = k.ID
	if err := s.save(st); err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

func (s *Server) describeKey(p *Principal, st *state, body []byte) (any, error) {
	var in struct{ KeyId string }
	if err := decode(body, &in); err != nil {
		return nil, err
	}
	k, err := s.resolve(st, in.KeyId)
	if err != nil {
		return nil, err
	}
	if err := s.allow(p, "kms:DescribeKey", k.ID, st); err != nil {
		return nil, err
	}
	return map[string]any{"KeyMetadata": s.metadata(k)}, nil
}

// spki is a SubjectPublicKeyInfo of a secp256k1 key, as AWS KMS returns it.
func spki(priv *ecdsa.PrivateKey) ([]byte, error) {
	type algorithm struct {
		Algorithm  asn1.ObjectIdentifier
		Parameters asn1.ObjectIdentifier
	}
	point := crypto.FromECDSAPub(&priv.PublicKey)
	return asn1.Marshal(struct {
		Algorithm algorithm
		PublicKey asn1.BitString
	}{algorithm{asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}, asn1.ObjectIdentifier{1, 3, 132, 0, 10}}, asn1.BitString{Bytes: point, BitLength: len(point) * 8}})
}

func (s *Server) getPublicKey(p *Principal, st *state, body []byte) (any, error) {
	var in struct{ KeyId string }
	if err := decode(body, &in); err != nil {
		return nil, err
	}
	k, err := s.resolve(st, in.KeyId)
	if err != nil {
		return nil, err
	}
	if err := s.allow(p, "kms:GetPublicKey", k.ID, st); err != nil {
		return nil, err
	}
	priv, err := k.private()
	if err != nil {
		return nil, err
	}
	der, err := spki(priv)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"KeyId": s.arn(k.ID), "PublicKey": der, "KeySpec": "ECC_SECG_P256K1", "CustomerMasterKeySpec": "ECC_SECG_P256K1",
		"KeyUsage": "SIGN_VERIFY", "SigningAlgorithms": []string{"ECDSA_SHA_256"},
	}, nil
}

func (s *Server) sign(p *Principal, st *state, body []byte) (any, error) {
	var in struct {
		KeyId, MessageType, SigningAlgorithm string
		Message                              []byte
	}
	if err := decode(body, &in); err != nil {
		return nil, err
	}
	k, err := s.resolve(st, in.KeyId)
	if err != nil {
		return nil, err
	}
	if err := s.allow(p, "kms:Sign", k.ID, st); err != nil {
		return nil, err
	}
	if in.SigningAlgorithm != "ECDSA_SHA_256" || in.MessageType != "DIGEST" || len(in.Message) != 32 {
		return nil, fail(http.StatusBadRequest, "ValidationException", "only a 32-byte DIGEST signed with ECDSA_SHA_256 is served")
	}
	priv, err := k.private()
	if err != nil {
		return nil, err
	}
	// A plain ECDSA signature, as an HSM gives it: s may be in either half of the order.
	r, sv, err := ecdsaSign(priv, in.Message)
	if err != nil {
		return nil, err
	}
	der, err := asn1.Marshal(struct{ R, S *big.Int }{r, sv})
	if err != nil {
		return nil, err
	}
	return map[string]any{"KeyId": s.arn(k.ID), "Signature": der, "SigningAlgorithm": "ECDSA_SHA_256"}, nil
}

// ecdsaSign signs a digest on secp256k1 and returns r and s the way an HSM does: s in
// either half of the order, so a client must bring it to the lower half itself.
func ecdsaSign(priv *ecdsa.PrivateKey, digest []byte) (*big.Int, *big.Int, error) {
	sig, err := crypto.Sign(digest, priv)
	if err != nil {
		return nil, nil, err
	}
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:64])
	// go-ethereum always gives the lower s; half of them are flipped, as a coin decides.
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, nil, err
	}
	if b[0]&1 == 1 {
		s.Sub(crypto.S256().Params().N, s)
	}
	return r, s, nil
}
