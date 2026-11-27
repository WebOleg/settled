package signer

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/WebOleg/settled/internal/withdrawals"
)

// The signer's API, over TLS with a client certificate (mTLS):
//
//	GET  /v1/address  {"address": "0x…"}
//	POST /v1/sign     {"tx": "0x…"} the unsigned transaction, encoded as on the wire
//	                  200 {"tx": "0x…"} signed
//	                  403 {"rule": "…", "reason": "…"} refused under the policy
//
// Only a client whose certificate the signer's CA issued reaches it, and only one whose
// common name is allowed may have anything signed.

type signRequest struct {
	Tx hexutil.Bytes `json:"tx"`
}

type signResponse struct {
	Tx     hexutil.Bytes `json:"tx,omitempty"`
	Rule   string        `json:"rule,omitempty"`
	Reason string        `json:"reason,omitempty"`
}

// maxRequest is the largest request read: a withdrawal's transaction is a few hundred
// bytes.
const maxRequest = 16 << 10

// Handler serves the API. allowed are the client common names that may sign.
func Handler(s *Service, allowed map[string]bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/address", func(rw http.ResponseWriter, _ *http.Request) {
		writeJSON(rw, http.StatusOK, map[string]string{"address": s.Key.Address().Hex()})
	})
	mux.HandleFunc("POST /v1/sign", func(rw http.ResponseWriter, r *http.Request) {
		client := ""
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			client = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		var req signRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, maxRequest)).Decode(&req); err != nil {
			writeJSON(rw, http.StatusBadRequest, signResponse{Reason: "the request is not {\"tx\": \"0x…\"}"})
			return
		}
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(req.Tx); err != nil {
			writeJSON(rw, http.StatusBadRequest, signResponse{Reason: "the transaction does not decode"})
			return
		}
		var signed *types.Transaction
		var err error
		if !allowed[client] {
			// Recorded like any refusal, so a client that should not be there is seen.
			err = s.refuse(r.Context(), client, tx, "client", fmt.Sprintf("client %q may not sign", client))
		} else {
			signed, err = s.Sign(r.Context(), client, tx)
		}
		var refused *withdrawals.Refused
		switch {
		case errors.As(err, &refused):
			writeJSON(rw, http.StatusForbidden, signResponse{Rule: refused.Rule, Reason: refused.Reason})
		case err != nil:
			log.Printf("withdrawal-signer: %v", err)
			writeJSON(rw, http.StatusInternalServerError, signResponse{Reason: "the signer failed; nothing was signed"})
		default:
			raw, err := signed.MarshalBinary()
			if err != nil {
				writeJSON(rw, http.StatusInternalServerError, signResponse{Reason: "the signer failed"})
				return
			}
			writeJSON(rw, http.StatusOK, signResponse{Tx: raw})
		}
	})
	return mux
}

// refuse records a refusal decided before the policy, and returns it.
func (s *Service) refuse(ctx context.Context, client string, tx *types.Transaction, rule, reason string) error {
	db, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Rollback(context.WithoutCancel(ctx)) }()
	if err := s.write(ctx, db, record{client: client, tx: tx, decision: "refused", rule: rule, reason: reason}); err != nil {
		return err
	}
	if err := db.Commit(ctx); err != nil {
		return err
	}
	s.observe("refused", rule)
	return &withdrawals.Refused{Rule: rule, Reason: reason}
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

// ServerTLS is the server's TLS configuration: its certificate, and client
// certificates required, issued by the CA.
func ServerTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	pool, err := certPool(caFile)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("signer: load the server certificate: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}, nil
}

// ClientTLS is a client's TLS configuration: its certificate, and the signer's
// certificate checked against the CA.
func ClientTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	pool, err := certPool(caFile)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("signer: load the client certificate: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: pool}, nil
}

func certPool(caFile string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("signer: read the CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, errors.New("signer: the CA file holds no certificate")
	}
	return pool, nil
}

// Remote is the payer's side: a withdrawals.Signer that asks the signer over mTLS.
type Remote struct {
	url     string
	client  *http.Client
	address common.Address
}

// NewRemote connects to the signer at url and reads the account it signs for.
func NewRemote(ctx context.Context, url string, config *tls.Config) (*Remote, error) {
	r := &Remote{url: url, client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: config}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/address", nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("signer: reach %s: %w", url, err)
	}
	defer resp.Body.Close()
	var body struct {
		Address common.Address `json:"address"`
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("signer: %s/v1/address: %s", url, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRequest)).Decode(&body); err != nil || body.Address == (common.Address{}) {
		return nil, fmt.Errorf("signer: %s/v1/address: no address", url)
	}
	r.address = body.Address
	return r, nil
}

func (r *Remote) Address() common.Address { return r.address }

func (r *Remote) Sign(ctx context.Context, tx *types.Transaction) (*types.Transaction, error) {
	raw, err := tx.MarshalBinary()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(signRequest{Tx: raw})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url+"/v1/sign", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("signer: %w", err)
	}
	defer resp.Body.Close()
	var out signResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRequest)).Decode(&out); err != nil {
		return nil, fmt.Errorf("signer: %s: the answer does not decode", resp.Status)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		t := new(types.Transaction)
		if err := t.UnmarshalBinary(out.Tx); err != nil {
			return nil, fmt.Errorf("signer: the signed transaction does not decode: %w", err)
		}
		return t, nil
	case http.StatusForbidden:
		return nil, &withdrawals.Refused{Rule: out.Rule, Reason: out.Reason}
	default:
		return nil, fmt.Errorf("signer: %s: %s", resp.Status, out.Reason)
	}
}

// Local is a withdrawals.Signer that asks a Service in the same process, as client: for
// the end-to-end runs, where the policy is under test without the network between.
type Local struct {
	Service *Service
	Client  string
}

func (l Local) Address() common.Address { return l.Service.Key.Address() }

func (l Local) Sign(ctx context.Context, tx *types.Transaction) (*types.Transaction, error) {
	return l.Service.Sign(ctx, l.Client, tx)
}
