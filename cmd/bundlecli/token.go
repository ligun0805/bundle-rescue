package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
  "os"
	"strings"
  "strconv"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// --- RPC concurrency gate (limits parallel eth_call to protect the RPC) ---
var rpcConcurrencyGate chan struct{}

func init() {
	n := 16
	if v := strings.TrimSpace(os.Getenv("GUI_RPC_MAX_CONCURRENCY")); v != "" {
		if i, err := strconv.Atoi(v); err == nil && i > 0 && i <= 256 { n = i }
	}
	rpcConcurrencyGate = make(chan struct{}, n)
}


type Address = common.Address

func mustAddrFromPK(pkHex string) Address {
	h := strings.TrimPrefix(strings.TrimSpace(pkHex), "0x")
	prv, err := crypto.HexToECDSA(h)
	must(err, "bad private key")
	return crypto.PubkeyToAddress(prv.PublicKey)
}

// callContractWithRetry is a tiny wrapper around eth_call with exponential backoff
// to survive provider rate limits (429 / -32005). Keep attempts small to avoid long waits.
func callContractWithRetry(ctx context.Context, ec *ethclient.Client, msg ethereum.CallMsg) ([]byte, error) {
	// Concurrency limiter
	rpcConcurrencyGate <- struct{}{}
	defer func(){ <-rpcConcurrencyGate }()

	const maxAttempts = 3
	backoff := 200 * time.Millisecond
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, err := ec.CallContract(ctx, msg, nil)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if attempt < maxAttempts {
			time.Sleep(backoff)
			s := err.Error()
			if strings.Contains(s, "Too Many Requests") || strings.Contains(s, "-32005") {
				backoff *= 2
			}
		}
	}
	return nil, lastErr
}

// classifyCallError returns a concise, user-facing reason for common eth_call failures.
func classifyCallError(ctx context.Context, ec *ethclient.Client, target common.Address, err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if strings.Contains(s, "Too Many Requests") || strings.Contains(s, "-32005") {
		return "[RATE_LIMIT] provider throttled the request"
	}
	if strings.Contains(s, "execution reverted") {
		if idx := strings.Index(s, ":"); idx >= 0 && idx+1 < len(s) {
			if r := strings.TrimSpace(s[idx+1:]); r != "" {
				return "[REVERT] " + r
			}
		}
		return "[REVERT] execution reverted"
	}
	if code, e := ec.CodeAt(ctx, target, nil); e == nil && len(code) == 0 {
		return "[NOT_CONTRACT] no bytecode at address"
	}
	if strings.Contains(s, "unsupported") || strings.Contains(s, "abi") {
		return "[UNSUPPORTED] ABI/return type mismatch"
	}
	return "[RPC] " + s
}

// fetchTokenDecimals returns decimals or error (caller may default to 18)
func fetchTokenDecimals(ctx context.Context, ec *ethclient.Client, token Address) (int, error) {
	decimalsSelector := common.FromHex("0x313ce567")
	res, err := callContractWithRetry(ctx, ec, ethereum.CallMsg{To: &token, Data: decimalsSelector})
	if err != nil {
		// Print precise reason for diagnostics; caller decides flow.
		fmt.Println("  [!] decimals():", classifyCallError(ctx, ec, token, err))
		return 0, err
	}
	if len(res) == 0 {
		return 18, nil
	}
	return int(res[len(res)-1]), nil
}

func fetchTokenBalance(ctx context.Context, ec *ethclient.Client, token, owner Address) (*big.Int, error) {
	data := append(common.FromHex("0x70a08231"), common.LeftPadBytes(owner.Bytes(), 32)...)
	res, err := callContractWithRetry(ctx, ec, ethereum.CallMsg{To: &token, Data: data})
	if err != nil {
		fmt.Println("  [!] balanceOf():", classifyCallError(ctx, ec, token, err))
		return nil, err
	}
	if len(res) == 0 {
		return big.NewInt(0), nil
	}
	return new(big.Int).SetBytes(res), nil
}

func toWeiFromTokens(amount string, decimals int) (*big.Int, error) {
	amount = strings.TrimSpace(amount)
	if amount == "" {
		return nil, fmt.Errorf("empty amount")
	}
	if decimals < 0 {
		decimals = 18
	}
	parts := strings.SplitN(amount, ".", 2)
	intPart := parts[0]
	fracPart := ""
	if len(parts) == 2 {
		fracPart = parts[1]
	}
	if len(fracPart) > decimals {
		return nil, fmt.Errorf("too many fractional digits for %d decimals", decimals)
	}
	fracPart = fracPart + strings.Repeat("0", decimals-len(fracPart))
	clean := strings.TrimLeft(intPart+fracPart, "0")
	if clean == "" {
		return big.NewInt(0), nil
	}
	v, ok := new(big.Int).SetString(clean, 10)
	if !ok {
		return nil, fmt.Errorf("bad amount")
	}
	return v, nil
}

func formatTokensFromWei(v *big.Int, decimals int) string {
	if v == nil {
		return "0"
	}
	if decimals <= 0 {
		return v.String()
	}
	s := new(big.Int).Abs(v).String()
	neg := v.Sign() < 0
	if len(s) <= decimals {
		frac := strings.Repeat("0", decimals-len(s)) + s
		out := "0." + strings.TrimRight(frac, "0")
		if out == "0." {
			out = "0"
		}
		if neg {
			return "-" + out
		}
		return out
	}
	intPart := s[:len(s)-decimals]
	frac := strings.TrimRight(s[len(s)-decimals:], "0")
	out := intPart
	if frac != "" {
		out = intPart + "." + frac
	}
	if neg {
		return "-" + out
	}
	return out
}

func tryReadBPSAndTS(ctx context.Context, ec *ethclient.Client, token Address) (ok bool, maxTxBps, maxWalletBps uint64, totalSupply *big.Int) {
	readUint := func(sig string) (*big.Int, error) {
		sel := crypto.Keccak256([]byte(sig))[:4]
		res, err := ec.CallContract(ctx, ethereum.CallMsg{To: &token, Data: sel}, nil)
		if err != nil || len(res) < 32 {
			return nil, err
		}
		return new(big.Int).SetBytes(res[len(res)-32:]), nil
	}
	ts, errTS := readUint("totalSupply()")
	mt, errTx := readUint("maxTxBPS()")
	mw, errW := readUint("maxWalletBPS()")
	if errTS == nil && ts != nil && errTx == nil && mt != nil && errW == nil && mw != nil {
		return true, mt.Uint64(), mw.Uint64(), ts
	}
	return false, 0, 0, nil
}

// printPendingStateForAddress prints latest/pending nonces and up to 10 pending txs from txpool (if supported).
// It degrades gracefully when the RPC does not expose txpool_* methods.
func printPendingStateForAddress(rpcURL, fromHexLower string) error {
	// tiny JSON-RPC helpers
	type rpcReq struct {
		Jsonrpc string        `json:"jsonrpc"`
		ID      int           `json:"id"`
		Method  string        `json:"method"`
		Params  []interface{} `json:"params"`
	}
	type rpcResp struct {
		Result interface{}                 `json:"result"`
		Error  *struct{ Message string `json:"message"` } `json:"error"`
	}
	call := func(method string, params []interface{}) (rpcResp, error) {
		body, _ := json.Marshal(rpcReq{Jsonrpc: "2.0", ID: 1, Method: method, Params: params})
		req, _ := http.NewRequest("POST", rpcURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return rpcResp{}, err
		}
		defer resp.Body.Close()
		var out rpcResp
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return rpcResp{}, err
		}
		if out.Error != nil {
			return out, fmt.Errorf(out.Error.Message)
		}
		return out, nil
	}
	latest, _ := call("eth_getTransactionCount", []interface{}{fromHexLower, "latest"})
	pending, _ := call("eth_getTransactionCount", []interface{}{fromHexLower, "pending"})
	fmt.Printf("  Nonce(latest/pending): %v / %v\n", latest.Result, pending.Result)

	// Try txpool_content (often blocked by hosted providers — silently skip)
	pool, err := call("txpool_content", nil)
	if err != nil {
		return nil
	}
	m, ok := pool.Result.(map[string]interface{})
	if !ok {
		return nil
	}
	pend, _ := m["pending"].(map[string]interface{})
	if pend == nil {
		return nil
	}
	addrMap, _ := pend[fromHexLower].(map[string]interface{})
	if addrMap == nil {
		return nil
	}
	fmt.Println("  Pending txs in txpool for FROM:")
	shown := 0
	for nonceKey, arr := range addrMap {
		list, _ := arr.(map[string]interface{})
		for _, v := range list {
			txObj, _ := v.(map[string]interface{})
			hash, _ := txObj["hash"].(string)
			to, _ := txObj["to"].(string)
			gas, _ := txObj["gas"].(string)
			feeCap, _ := txObj["maxFeePerGas"].(string)
			fmt.Printf("    • nonce=%s hash=%s to=%s gas=%s feeCap=%s\n", nonceKey, hash, to, gas, feeCap)
			shown++
			if shown >= 10 {
				fmt.Println("    … truncated")
				return nil
			}
		}
	}
	return nil
}

// fetchTokenSymbol returns ERC-20 symbol; supports both dynamic string and bytes32.
func fetchTokenSymbol(ctx context.Context, ec *ethclient.Client, token Address) (string, error) {
	data := common.FromHex("0x95d89b41") // symbol()
	out, err := callContractWithRetry(ctx, ec, ethereum.CallMsg{To: &token, Data: data})
	if err != nil || len(out) == 0 {
		if err != nil {
			fmt.Println("  [!] symbol():", classifyCallError(ctx, ec, token, err))
		}
		return "", err
	}
	if len(out) >= 64 {
		l := new(big.Int).SetBytes(out[32:64]).Int64()
		if l > 0 && 64+int(l) <= len(out) {
			return string(out[64 : 64+int(l)]), nil
		}
	}
	return strings.TrimRight(string(out), "\x00"), nil
}
