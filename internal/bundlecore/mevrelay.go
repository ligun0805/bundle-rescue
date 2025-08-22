package bundlecore

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/common/hexutil"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	w3 "github.com/lmittmann/w3"
)

// Relay dialed via w3 + flashbots.
type relayClient struct {
	URL string
	C   *w3.Client
}

// classifyRelays splits relay URLs into classic (flashbots-compatible) and matchmakers (mev: / mm: / bloxroute etc.)
func classifyRelays(relays []string, dial func(url string) *w3.Client) (classic []relayClient, matchmakers []string) {
	for _, r := range relays {
		u := strings.TrimSpace(r)
		if u == "" {
			continue
		}
		low := strings.ToLower(u)
		switch {
		case strings.HasPrefix(low, "mm:"):
			matchmakers = append(matchmakers, strings.TrimPrefix(u, "mm:"))
		case strings.Contains(low, "blxrbdn.com") || strings.Contains(low, "bloxroute"):
			// bloXroute Cloud-API is not flashbots-RPC compatible — treat as matchmaker path
			matchmakers = append(matchmakers, u)
			continue
		case strings.HasPrefix(low, "mev:"):
			// explicit "mev:" prefix — treat as matchmaker and strip prefix
			u2 := strings.TrimPrefix(u, "mev:")
			matchmakers = append(matchmakers, u2)
		case strings.HasPrefix(low, "classic:"):
			u2 := strings.TrimPrefix(u, "classic:")
			classic = append(classic, relayClient{URL: u2, C: dial(u2)})
		case strings.Contains(low, "mev") || strings.Contains(low, "matchmaker"):
			// backward compatibility: old heuristic
			matchmakers = append(matchmakers, u)
		default:
			classic = append(classic, relayClient{URL: u, C: dial(u)})
		}
	}
	return
}

// sendMevBundle handles flashbots-like and bloxroute APIs with reasonable fallbacks.
func sendMevBundle(ctx context.Context, p *Params, url string, headers map[string]string, authPriv *ecdsa.PrivateKey, txHexes []string, targetBlock *big.Int) (string, error) {
	u := strings.TrimPrefix(url, "mev:")
	isBLXR := strings.Contains(strings.ToLower(u), "blxrbdn.com")

	doOnce := func(method string) (string, error) {
		var body []byte
		if isBLXR {
			// bloXroute Cloud-API: blxr_submit_bundle (non-standard, but accepts JSON-RPC envelope)
			txsNo0x := make([]string, 0, len(txHexes))
			for _, h := range txHexes {
				txsNo0x = append(txsNo0x, strings.TrimPrefix(h, "0x"))
			}
			params := map[string]any{
				"transaction":  txsNo0x,
				"block_number": "0x" + targetBlock.Text(16),
			}
			req := map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"method":  "blxr_submit_bundle",
				"params":  params,
			}
			var err error
			body, err = json.Marshal(req)
			if err != nil {
				return "", err
			}
		} else {
			payload := map[string]any{
				"txs":         txHexes,
				"blockNumber": "0x" + targetBlock.Text(16),
			}
			if p != nil && p.MinTimestamp > 0 {
				payload["minTimestamp"] = p.MinTimestamp
			}
			if p != nil && p.MaxTimestamp > 0 {
				payload["maxTimestamp"] = p.MaxTimestamp
			}
			low := strings.ToLower(u)
			isFlashbots := strings.Contains(low, "flashbots.net")
			isBeaver := strings.Contains(low, "beaverbuild.org")
			if p != nil && p.ReplacementUUID != "" && !isBeaver {
				payload["replacementUuid"] = p.ReplacementUUID
			}
			if isFlashbots && p != nil && len(p.Builders) > 0 {
				payload["builders"] = p.Builders
			}
			if isBeaver && p != nil {
				if p.BeaverAllowBuilderNetRefunds != nil {
					payload["allowBuilderNetRefunds"] = *p.BeaverAllowBuilderNetRefunds
				}
				if strings.TrimSpace(p.BeaverRefundRecipientHex) != "" {
					payload["builderNetRefundAddress"] = p.BeaverRefundRecipientHex
				}
			}
			body, _ = json.Marshal(rpcReq{
				Jsonrpc: "2.0", Method: method, Params: []any{payload}, ID: 1,
			})
		}

		req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "bundle-rescue/1.0")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if authPriv != nil {
			addr := gethcrypto.PubkeyToAddress(authPriv.PublicKey)
			msgHash := gethcrypto.Keccak256(body)
			sigBytes, err := gethcrypto.Sign(msgHash, authPriv)
			if err != nil {
				return "", err
			}
			req.Header.Set("X-Flashbots-Signature", addr.Hex()+":"+hexutil.Encode(sigBytes))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		var out rpcResp
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", err
		}
		if out.Error != nil {
			return "", errors.New(out.Error.Message)
		}
		return string(out.Result), nil
	}

	if isBLXR {
		return doOnce("blxr_submit_bundle")
	}
	// try eth_sendBundle first
	res, err := doOnce("eth_sendBundle")
	if err == nil {
		return res, nil
	}
	// fallback to mev_sendBundle for Eden/compatible
	lowErr := strings.ToLower(err.Error())
	if strings.Contains(lowErr, "method") ||
		strings.Contains(lowErr, "not found") ||
		strings.Contains(lowErr, "unsupported") ||
		strings.Contains(lowErr, "eof") {
		return doOnce("mev_sendBundle")
	}
	return "", err
}

// simulateMevBundle tries mev_simBundle on matchmakers (best-effort).
func simulateMevBundle(ctx context.Context, url string, headers map[string]string, authPriv *ecdsa.PrivateKey, txHexes []string, targetBlock *big.Int) (string, bool, error) {
	u := strings.TrimPrefix(url, "mev:")
	payload := map[string]any{"txs": txHexes, "blockNumber": "0x" + targetBlock.Text(16)}
	params := []any{payload}
	body, _ := json.Marshal(rpcReq{Jsonrpc: "2.0", Method: "mev_simBundle", Params: params, ID: 1})

	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if authPriv != nil {
		addr := gethcrypto.PubkeyToAddress(authPriv.PublicKey)
		msgHash := gethcrypto.Keccak256(body)
		if sigBytes, err := gethcrypto.Sign(msgHash, authPriv); err == nil {
			req.Header.Set("X-Flashbots-Signature", addr.Hex()+":"+hexutil.Encode(sigBytes))
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()

	var out rpcResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		if resp.StatusCode == 404 {
			return "", false, nil
		}
		return "", false, err
	}
	if out.Error != nil {
		if strings.Contains(out.Error.Message, "Method not found") {
			return "", false, nil
		}
		return "", true, errors.New(out.Error.Message)
	}
	bs, _ := json.Marshal(out.Result)
	return string(bs), true, nil
}
