package bundlecore

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	w3 "github.com/lmittmann/w3"
)

// Relay dialed via w3 + flashbots.
type relayClient struct {
	URL string
	C   *w3.Client
}

// buildStandardPayload returns the classic/old-style bundle payload.
func buildStandardPayload(txHexes []string, targetBlock *big.Int) map[string]any {
	return map[string]any{
		"txs":         txHexes,
		"blockNumber": fmt.Sprintf("0x%x", targetBlock),
	}
}

// buildStrategyPayload returns the strategy payload (standard base + optional fields).
// Optional fields are added only when present/meaningful.
func buildStrategyPayload(p *Params, relayURL string, txHexes []string, targetBlock *big.Int) map[string]any {
	payload := buildStandardPayload(txHexes, targetBlock)
	if p == nil {
		return payload
	}
	low := strings.ToLower(relayURL)
	isFlashbots := strings.Contains(low, "flashbots.net")
	isBeaver := strings.Contains(low, "beaverbuild.org")

	// Optional timing window
	if p.MinTimestamp > 0 {
		payload["minTimestamp"] = p.MinTimestamp
	}
	if p.MaxTimestamp > 0 {
		payload["maxTimestamp"] = p.MaxTimestamp
	}
	// Optional replacement UUID (skip for Beaver as they do not accept it)
	if p.ReplacementUUID != "" && !isBeaver {
		payload["replacementUuid"] = p.ReplacementUUID
	}
	// Optional builder allowlist (Flashbots only)
	if isFlashbots && len(p.Builders) > 0 {
		payload["builders"] = p.Builders
	}
	// Beaver-specific refund knobs
	if isBeaver {
		if p.BeaverAllowBuilderNetRefunds != nil {
			payload["allowBuilderNetRefunds"] = *p.BeaverAllowBuilderNetRefunds
		}
		if strings.TrimSpace(p.BeaverRefundRecipientHex) != "" {
			payload["builderNetRefundAddress"] = p.BeaverRefundRecipientHex
		}
	}
	return payload
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

	// Strategy switch (any of these knobs => strategy mode)
	useStrategy := false
	if p != nil {
		useStrategy = (p.MinTimestamp > 0 ||
			p.MaxTimestamp > 0 ||
			p.ReplacementUUID != "" ||
			len(p.Builders) > 0 ||
			p.BeaverAllowBuilderNetRefunds != nil ||
			strings.TrimSpace(p.BeaverRefundRecipientHex) != "")
	}

	// Helper to POST a JSON-RPC request with provided body.
	postJSON := func(body []byte) (string, error) {
		req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "bundle-rescue/1.0")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if authPriv != nil {
			addr := gethcrypto.PubkeyToAddress(authPriv.PublicKey)
			msgHash := accounts.TextHash(body)
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

	// BLXR path (Cloud API)
	if isBLXR {
		txsNo0x := make([]string, 0, len(txHexes))
		for _, h := range txHexes {
			txsNo0x = append(txsNo0x, strings.TrimPrefix(h, "0x"))
		}
		req := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "blxr_submit_bundle",
			"params": map[string]any{
				"transaction":  txsNo0x,
				"block_number": "0x" + targetBlock.Text(16),
			},
		}
		body, _ := json.Marshal(req)
		return postJSON(body)
	}

	// STANDARD mode: strictly old behavior — mev_sendBundle with minimal payload
	if !useStrategy {
		payload := map[string]any{
			"txs":         txHexes,
			"blockNumber": "0x" + targetBlock.Text(16),
		}
		body, _ := json.Marshal(rpcReq{
			Jsonrpc: "2.0",
			Method:  "mev_sendBundle",
			Params:  []any{payload},
			ID:      1,
		})
		return postJSON(body)
	}

	// STRATEGY mode: keep extended behavior (eth_sendBundle → fallback mev_sendBundle)
	payload := buildStrategyPayload(p, u, txHexes, targetBlock)
	bodyEth, _ := json.Marshal(rpcReq{
		Jsonrpc: "2.0",
		Method:  "eth_sendBundle",
		Params:  []any{payload},
		ID:      1,
	})
	res, err := postJSON(bodyEth)
	if err == nil {
		return res, nil
	}
	lowErr := strings.ToLower(err.Error())
	if strings.Contains(lowErr, "method") ||
		strings.Contains(lowErr, "not found") ||
		strings.Contains(lowErr, "unsupported") ||
		strings.Contains(lowErr, "eof") {
		bodyMev, _ := json.Marshal(rpcReq{
			Jsonrpc: "2.0",
			Method:  "mev_sendBundle",
			Params:  []any{payload},
			ID:      1,
		})
		return postJSON(bodyMev)
	}
	return "", err
}


func simulateMevBundle(ctx context.Context, p *Params, url string, headers map[string]string, authPriv *ecdsa.PrivateKey, txHexes []string, targetBlock *big.Int) (string, bool, error) {
    u := strings.TrimPrefix(url, "mev:")
    low := strings.ToLower(u)

    maybeLogBundleOnce(txHexes, targetBlock)

    // Use parent block as state base for simulation.
    // Flashbots frequently misreports "insufficient ETH for simulation" when stateBlockNumber="latest"
    // even though the first tx in the bundle funds the account. Using (targetBlock - 1) aligns the
    // sim state with the builder’s view and makes prefund-visible.
    parentBlock := new(big.Int).Sub(targetBlock, big.NewInt(1))
    if parentBlock.Sign() < 0 {
        parentBlock = big.NewInt(0)
    }

    // Decide whether strategy knobs are enabled (used below after simOnce is declared).
    useStrategy := false
    if p != nil {
        useStrategy = (p.MinTimestamp > 0 ||
            p.MaxTimestamp > 0 ||
            p.ReplacementUUID != "" ||
            len(p.Builders) > 0 ||
            p.BeaverAllowBuilderNetRefunds != nil ||
            strings.TrimSpace(p.BeaverRefundRecipientHex) != "")
    }
	// ---- bloXroute Cloud-API ----
    if strings.Contains(low, "blxrbdn.com") || strings.Contains(low, "bloxroute") {
        txNo0x := make([]string, 0, len(txHexes))
        for _, h := range txHexes {
            txNo0x = append(txNo0x, strings.TrimPrefix(h, "0x"))
        }
        params := BlxrSimulateBundleParams{
            Transaction:       txNo0x,
            BlockNumber:       "0x" + targetBlock.Text(16),
            BlockchainNetwork: "Mainnet",
        }
        body, _ := json.Marshal(map[string]any{
            "jsonrpc": "2.0",
            "id":      "1",
            "method":  "blxr_simulate_bundle",
            "params":  params,
        })

        req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
        req.Header.Set("Content-Type", "application/json")
        for k, v := range headers {
            req.Header.Set(k, v)
        }
        // No X-Flashbots-Signature for BLXR; only Authorization is required.
        resp, err := http.DefaultClient.Do(req)
        if err != nil {
            return "", false, err
        }
        defer resp.Body.Close()

        raw, _ := io.ReadAll(resp.Body)
        if resp.StatusCode != http.StatusOK {
            return string(raw), false, fmt.Errorf("http %d", resp.StatusCode)
        }
        var parsed BlxrSimulateBundleResponse
        if err := json.Unmarshal(raw, &parsed); err != nil {
            return string(raw), false, err
        }
        if parsed.Error != nil {
            // Gracefully degrade on plans/endpoints where BLXR simulation is not supported.
            // The Cloud API returns e.g. "simulation not supported on matchmaker" for
            // non-Ultra/Enterprise plans – treat this as "unsupported" so the caller can still send.
            lowMsg := strings.ToLower(parsed.Error.Message)
            if strings.Contains(lowMsg, "not supported") || strings.Contains(lowMsg, "simulation not supported") {
                return string(raw), false, nil
            }
            return string(raw), false, errors.New(parsed.Error.Message)
        }
        ok := parsed.Result != nil
        if ok {
            for _, r := range parsed.Result.Results {
                if r.Error != "" || r.Revert != "" {
                    ok = false
                    break
                }
            }
        }
        return string(raw), ok, nil
    }

    // ---- classic relays: try eth_callBundle first, then fallback to mev_simBundle ----
    simOnce := func(method string) (raw string, ok bool, err error) {

        payload := map[string]any{
            "txs":         txHexes,
            "blockNumber": "0x" + targetBlock.Text(16),
        }
        body, _ := json.Marshal(rpcReq{Jsonrpc: "2.0", Method: method, Params: []any{payload}, ID: 1})

        req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
        req.Header.Set("Content-Type", "application/json")
        for k, v := range headers {
            req.Header.Set(k, v)
        }
        if authPriv != nil {
            addr := gethcrypto.PubkeyToAddress(authPriv.PublicKey)
            msgHash := accounts.TextHash(body)
            if sigBytes, err := gethcrypto.Sign(msgHash, authPriv); err == nil {
                req.Header.Set("X-Flashbots-Signature", addr.Hex()+":"+hexutil.Encode(sigBytes))
            }
        }
        resp, err := http.DefaultClient.Do(req)
        if err != nil {
            return "", false, err
        }
        defer resp.Body.Close()

        rawBytes, _ := io.ReadAll(resp.Body)
        var out rpcResp
        if err := json.Unmarshal(rawBytes, &out); err != nil {
            return string(rawBytes), false, err
        }
        if out.Error != nil {
            return string(rawBytes), false, errors.New(out.Error.Message)
        }
        bs, _ := json.Marshal(out.Result)
        return string(bs), true, nil
    }

    if !useStrategy {
        // STANDARD: exactly old behavior — call mev_simBundle only
        return simOnce("mev_simBundle")
    }
    // (Стратегия — как у тебя было; если стратегия не используется, сюда не попадаем)
    raw, ok, err := simOnce("eth_callBundle")
    if err == nil { return raw, ok, nil }
    lowErr := strings.ToLower(err.Error())
    if strings.Contains(lowErr, "method") || strings.Contains(lowErr, "not found") || strings.Contains(lowErr, "unsupported") || strings.Contains(lowErr, "not available") {
        return simOnce("mev_simBundle")
    }
    return raw, ok, err
}

// -----------------------------------------------------------------------------
// One-shot bundle logging per (blockNumber, tx set)
// -----------------------------------------------------------------------------
var printedBundleFingerprints sync.Map

func maybeLogBundleOnce(txHexes []string, targetBlock *big.Int) {
    // Simple fingerprint: blockNumber + joined tx hexes length/first/last
    // (no heavy hashing to keep it lightweight)
    keyBuilder := strings.Builder{}
    keyBuilder.WriteString(targetBlock.Text(16))
    keyBuilder.WriteString("|")
    for _, raw := range txHexes {
        keyBuilder.WriteString(fmt.Sprintf("%d|", len(raw)))
    }
    fp := keyBuilder.String()
    if _, seen := printedBundleFingerprints.Load(fp); seen {
        return
    }
    printedBundleFingerprints.Store(fp, struct{}{})

    fmt.Printf("[bundle] block=%s txs=%d\n", targetBlock.Text(10), len(txHexes))
    for i, raw := range txHexes {
        // Try to compute tx hash from raw rlp (best-effort)
        if b, err := hexutil.Decode(raw); err == nil {
            h := hexutil.Encode(gethcrypto.Keccak256(b))
            fmt.Printf("  - tx[%d]: hash=%s, size=%d bytes\n", i, h, len(b))
        } else {
            fmt.Printf("  - tx[%d]: size=? (decode error)\n", i)
        }
    }
}