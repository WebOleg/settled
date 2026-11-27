package signer

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// The payer reads the signer's account at each step: when the payer's key is rotated it
// follows, and a signer that answers no account is an error, not the zero account.
func TestRemote_followsTheAccountWhenTheKeyIsRotated(t *testing.T) {
	var account atomic.Value
	account.Store("0x00000000000000000000000000000000000000a1")
	ts := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/address" {
			http.NotFound(rw, r)
			return
		}
		_, _ = rw.Write([]byte(`{"address":"` + account.Load().(string) + `"}`))
	}))
	defer ts.Close()
	config := ts.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	config.MinVersion = tls.VersionTLS12
	ctx := context.Background()
	r, err := NewRemote(ctx, ts.URL, config)
	if err != nil {
		t.Fatal(err)
	}
	if r.Address() != common.HexToAddress("0xa1") {
		t.Fatalf("account %s", r.Address())
	}
	if changed, err := r.Refresh(ctx); err != nil || changed {
		t.Fatalf("the same account: changed %t, %v", changed, err)
	}
	account.Store("0x00000000000000000000000000000000000000b2")
	if changed, err := r.Refresh(ctx); err != nil || !changed || r.Address() != common.HexToAddress("0xb2") {
		t.Fatalf("rotated: changed %t, %s, %v; want the new account", changed, r.Address(), err)
	}
	account.Store("0x0000000000000000000000000000000000000000")
	if _, err := r.Refresh(ctx); err == nil {
		t.Fatal("the zero account: accepted")
	}
	if r.Address() != common.HexToAddress("0xb2") {
		t.Fatalf("after a bad answer the account is %s, want the last good one", r.Address())
	}
}
