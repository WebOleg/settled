package vaultwatch

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

// Client is a minimal Ethereum JSON-RPC client: the few calls the watcher needs, over
// plain HTTP, so the watcher carries no dependency beyond the standard library.
type Client struct {
	URL  string
	HTTP *http.Client
	id   atomic.Uint64
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) call(ctx context.Context, method string, result any, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: c.id.Add(1), Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("vaultwatch: %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vaultwatch: %s: HTTP %d", method, resp.StatusCode)
	}
	var r rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("vaultwatch: %s: decode: %w", method, err)
	}
	if r.Error != nil {
		return fmt.Errorf("vaultwatch: %s: %d %s", method, r.Error.Code, r.Error.Message)
	}
	if err := json.Unmarshal(r.Result, result); err != nil {
		return fmt.Errorf("vaultwatch: %s: result: %w", method, err)
	}
	return nil
}

// Header is the part of a block header the watcher reads.
type Header struct {
	Number uint64
	Hash   string
}

// HeaderByNumber returns the header of block n, or of the latest block when latest is set.
func (c *Client) HeaderByNumber(ctx context.Context, n uint64, latest bool) (Header, error) {
	tag := "latest"
	if !latest {
		tag = quantity(n)
	}
	var raw *struct {
		Number string `json:"number"`
		Hash   string `json:"hash"`
	}
	if err := c.call(ctx, "eth_getBlockByNumber", &raw, tag, false); err != nil {
		return Header{}, err
	}
	if raw == nil {
		return Header{}, fmt.Errorf("vaultwatch: no block %s", tag)
	}
	num, err := parseQuantity(raw.Number)
	if err != nil {
		return Header{}, err
	}
	return Header{Number: num, Hash: strings.ToLower(raw.Hash)}, nil
}

// Log is one event log.
type Log struct {
	Address     string   `json:"address"`
	Topics      []string `json:"topics"`
	Data        string   `json:"data"`
	BlockNumber string   `json:"blockNumber"`
	LogIndex    string   `json:"logIndex"`
	Removed     bool     `json:"removed"`
}

// Logs returns the logs of address in blocks from..to whose first topic is one of topics.
func (c *Client) Logs(ctx context.Context, address string, from, to uint64, topics []string) ([]Log, error) {
	filter := map[string]any{
		"address":   address,
		"fromBlock": quantity(from),
		"toBlock":   quantity(to),
		"topics":    []any{topics},
	}
	var logs []Log
	if err := c.call(ctx, "eth_getLogs", &logs, filter); err != nil {
		return nil, err
	}
	return logs, nil
}

// Call runs a read-only call of data to address in block n and returns what it returns.
func (c *Client) Call(ctx context.Context, address, data string, n uint64) ([]byte, error) {
	var out string
	if err := c.call(ctx, "eth_call", &out, map[string]string{"to": address, "data": data}, quantity(n)); err != nil {
		return nil, err
	}
	return decodeHex(out)
}

// Balance returns the ETH balance of address in block n.
func (c *Client) Balance(ctx context.Context, address string, n uint64) (*big.Int, error) {
	var out string
	if err := c.call(ctx, "eth_getBalance", &out, address, quantity(n)); err != nil {
		return nil, err
	}
	v, ok := new(big.Int).SetString(strings.TrimPrefix(out, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("vaultwatch: balance %q is not a number", out)
	}
	return v, nil
}

func quantity(n uint64) string {
	return "0x" + strconv.FormatUint(n, 16)
}

func parseQuantity(s string) (uint64, error) {
	if !strings.HasPrefix(s, "0x") {
		return 0, fmt.Errorf("vaultwatch: quantity %q has no 0x", s)
	}
	return strconv.ParseUint(s[2:], 16, 64)
}

func decodeHex(s string) ([]byte, error) {
	if !strings.HasPrefix(s, "0x") {
		return nil, errors.New("vaultwatch: hex without 0x")
	}
	return hex.DecodeString(s[2:])
}
