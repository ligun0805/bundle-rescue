package flashbots

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

type Client struct {
	RelayURL string
	AuthKey  *ecdsa.PrivateKey // X-Flashbots-Signature key
	http     *http.Client
}

type SimResult struct {
	OK      bool
	Error   string
	RawJSON string
}

type SendResult struct {
	OK       bool
	BundleID string
	Error    string
	RawJSON  string
}

func NewClient(relayURL, authPrivHex string) (*Client, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(authPrivHex, "0x"))
	if err != nil { return nil, fmt.Errorf("auth key: %w", err) }
	return &Client{RelayURL: relayURL, AuthKey: key, http: &http.Client{ Timeout: 12 * time.Second }}, nil
}

func (c *Client) signBody(b []byte) string {
	addr := crypto.PubkeyToAddress(c.AuthKey.PublicKey)
	sig, _ := crypto.Sign(crypto.Keccak256(b), c.AuthKey)
	return fmt.Sprintf("%s:%s", addr.Hex(), hex.EncodeToString(sig))
}

// eth_callBundle for simulation
func (c *Client) SimulateBundle(ctx context.Context, rawTxs []string, targetBlock uint64) (*SimResult, error) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_callBundle",
		"params": []any{map[string]any{
			"txs":              rawTxs,
			"blockNumber":      hexutil.EncodeUint64(targetBlock),
			"stateBlockNumber": "latest",
		}},
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.RelayURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Flashbots-Signature", c.signBody(body))
	resp, err := c.http.Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var jr struct{ Result any `json:"result"`; Error *struct{ Code int; Message string } `json:"error"` }
	_ = json.Unmarshal(rb, &jr)
	res := &SimResult{RawJSON: string(rb)}
	if jr.Error != nil { res.OK = false; res.Error = fmt.Sprintf("%d %s", jr.Error.Code, jr.Error.Message); return res, nil }
	res.OK = true
	return res, nil
}

// eth_sendBundle for sending
func (c *Client) SendBundle(ctx context.Context, rawTxs []string, targetBlock uint64) (*SendResult, error) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_sendBundle",
		"params": []any{map[string]any{
			"txs":        rawTxs,
			"blockNumber": hexutil.EncodeUint64(targetBlock),
		}},
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.RelayURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Flashbots-Signature", c.signBody(body))
	resp, err := c.http.Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var jr struct{ Result any `json:"result"`; Error *struct{ Code int; Message string } `json:"error"` }
	_ = json.Unmarshal(rb, &jr)
	res := &SendResult{RawJSON: string(rb)}
	if jr.Error != nil { res.OK = false; res.Error = fmt.Sprintf("%d %s", jr.Error.Code, jr.Error.Message); return res, nil }
	res.OK = true
	res.BundleID = fmt.Sprint(jr.Result)
	return res, nil
}
