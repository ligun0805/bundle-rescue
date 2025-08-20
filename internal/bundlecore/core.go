package bundlecore

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/lmittmann/flashbots"
	w3 "github.com/lmittmann/w3"
)

type Params struct {
	RPC         string
	ChainID     *big.Int
	Relays      []string
	AuthPrivHex string
	Logf        func(string, ...any)
	OnSimResult func(relay, raw string, ok bool, err string)

	Token     common.Address
	From      common.Address
	To        common.Address
	AmountWei *big.Int

	SafePKHex string
	FromPKHex string

	Blocks       int
	TipGweiBase  int64
	TipMul       float64
	BaseMul      int64
	BufferPct    int64
	SimulateOnly bool
	SkipIfPaused bool
	Verbose      bool
}

type Result struct {
	Included bool
	Reason   string
}

func (p *Params) logf(format string, a ...any) { if p.Logf != nil { p.Logf(format, a...) } }

func hexToECDSAPriv(s string) (*ecdsa.PrivateKey, error) {
	h := strings.TrimSpace(strings.TrimPrefix(s, "0x"))
	if len(h) == 0 { return nil, errors.New("empty private key") }
	return gethcrypto.HexToECDSA(h)
}
func gweiToWei(g int64) *big.Int { x:= new(big.Int).SetInt64(g); return x.Mul(x, big.NewInt(1_000_000_000)) }
func mulBig(a *big.Int, m int64) *big.Int { if a==nil { return big.NewInt(0) }; return new(big.Int).Mul(a, big.NewInt(m)) }
func addBig(a, b *big.Int) *big.Int { if a==nil { return b }; if b==nil { return a }; return new(big.Int).Add(a, b) }

// --- pretty format helpers ---
func fmtETH(x *big.Int) string {
	if x == nil { return "0" }
	r := new(big.Rat).SetFrac(new(big.Int).Set(x), big.NewInt(1_000_000_000_000_000_000))
	return r.FloatString(6) // ETH
}
func fmtGwei(x *big.Int) string {
	if x == nil { return "0" }
	r := new(big.Rat).SetFrac(new(big.Int).Set(x), big.NewInt(1_000_000_000))
	return r.FloatString(2) // gwei
}


func encodeERC20Transfer(to common.Address, amount *big.Int) []byte {
	selector := common.FromHex("0xa9059cbb")
	arg1 := common.LeftPadBytes(to.Bytes(), 32)
	arg2 := common.LeftPadBytes(amount.Bytes(), 32)
	return append(selector, append(arg1, arg2...)...)
}

func PreflightTransfer(ctx context.Context, ec *ethclient.Client, token, from, to common.Address, amount *big.Int) (ok bool, reason string, err error) {
	data := encodeERC20Transfer(to, amount)
	msg  := ethereum.CallMsg{ From: from, To: &token, Data: data, Value: big.NewInt(0) }
	if _, e := ec.EstimateGas(ctx, msg); e == nil {
		return true, "", nil
	}
	if _, e := ec.CallContract(ctx, msg, nil); e != nil {
		return false, revertReason(e), nil
	}
	return false, "transfer would revert", nil
}

func revertReason(e error) string {
	s := e.Error()
	if i := strings.Index(s, "execution reverted"); i >= 0 {
		return s[i:]
	}
	return s
}


var pausedSigs = [][]byte{
	common.FromHex("0x5c975abb"), // paused()
	common.FromHex("0x3f4ba83a"), // isPaused()
	common.FromHex("0x51dff989"), // transfersPaused()
	common.FromHex("0x5c701d2f"), // tradingPaused()
	common.FromHex("0x8462151c"), // isTradingPaused()
	common.FromHex("0x2e1a7d4d"), // pausedTransfers()
	common.FromHex("0x0b3bafd6"), // globalPaused()
	common.FromHex("0x9c6a3b7c"), // transferEnabled()
	common.FromHex("0x75f12b21"), // isTransferEnabled()
	common.FromHex("0x4f2be91f"), // tradingEnabled()
	common.FromHex("0x0dfe1681"), // isTradingEnabled()
}

func CheckPaused(ctx context.Context, ec *ethclient.Client, token common.Address) (known, paused bool, err error) {
	for _, sig := range pausedSigs {
		res, e := ec.CallContract(ctx, ethereum.CallMsg{To:&token, Data:sig}, nil)
		if e != nil || len(res) == 0 { continue }
		b := res[len(res)-1]
		s := hex.EncodeToString(sig)
		if strings.Contains(s, "enabled") {
			return true, b == 0, nil
		}
		return true, b == 1, nil
	}
	return false, false, nil
}

var (
	blacklistAddrViewSigsStr = []string{
		"isBlacklisted(address)", "isBlackListed(address)", "blacklisted(address)", "isInBlacklist(address)",
	}
	whitelistAddrViewSigsStr = []string{
		"isWhitelisted(address)", "whitelisted(address)",
	}
	onlyWhitelistGlobalSigsStr = []string{
		"onlyWhitelisted()", "whitelistEnabled()",
	}
	transferDisabledGlobalSigsStr = []string{
		"transferDisabled()", "isTransferDisabled()", "transfersPaused()",
	}
)

func sel(sig string) []byte {
	h := gethcrypto.Keccak256([]byte(sig))
	return h[:4]
}

type TokenRestrictions struct {
	Paused           bool
	TransferDisabled bool
	OnlyWhitelisted  bool
	FromWhitelisted  *bool
	ToWhitelisted    *bool
	BlacklistedFrom  bool
	BlacklistedTo    bool
}

func (tr TokenRestrictions) Blocked() bool {
	if tr.Paused || tr.TransferDisabled || tr.BlacklistedFrom || tr.BlacklistedTo { return true }
	if tr.OnlyWhitelisted {
		if tr.FromWhitelisted != nil && !*tr.FromWhitelisted { return true }
		if tr.ToWhitelisted != nil && !*tr.ToWhitelisted { return true }
	}
	return false
}

func (tr TokenRestrictions) Summary() string {
	parts := []string{}
	if tr.Paused { parts = append(parts, "paused") }
	if tr.TransferDisabled { parts = append(parts, "transferDisabled") }
	if tr.BlacklistedFrom { parts = append(parts, "from:blacklisted") }
	if tr.BlacklistedTo { parts = append(parts, "to:blacklisted") }
	if tr.OnlyWhitelisted {
		wf := "unknown"; if tr.FromWhitelisted != nil { if *tr.FromWhitelisted { wf="yes" } else { wf="no" } }
		wt := "unknown"; if tr.ToWhitelisted != nil { if *tr.ToWhitelisted { wt="yes" } else { wt="no" } }
		parts = append(parts, fmt.Sprintf("whitelist:on (from=%s,to=%s)", wf, wt))
	}
	if len(parts)==0 { return "none" }
	return strings.Join(parts, ", ")
}

func CheckRestrictions(ctx context.Context, ec *ethclient.Client, token common.Address, from, to common.Address) (TokenRestrictions, error) {
	var out TokenRestrictions
	known, paused, _ := CheckPaused(ctx, ec, token)
	if known && paused { out.Paused = true; return out, nil }
	call := func(data []byte) (ret []byte, ok bool) {
		res, err := ec.CallContract(ctx, ethereum.CallMsg{ To:&token, Data:data }, nil)
		if err != nil || len(res) == 0 { return nil, false }
		return res, true
	}
	boolOf := func(b []byte) bool { if len(b)==0 { return false }; return b[len(b)-1]==1 }
	for _, s := range transferDisabledGlobalSigsStr {
		if ret, ok := call(sel(s)); ok && boolOf(ret) { out.TransferDisabled = true; return out, nil }
	}
	for _, s := range onlyWhitelistGlobalSigsStr {
		if ret, ok := call(sel(s)); ok && boolOf(ret) { out.OnlyWhitelisted = true; break }
	}
	whitelisted := func(addr common.Address) *bool {
		for _, s := range whitelistAddrViewSigsStr {
			data := append(sel(s), common.LeftPadBytes(addr.Bytes(), 32)...)
			if ret, ok := call(data); ok { v := boolOf(ret); return &v }
		}
		return nil
	}
	if out.OnlyWhitelisted {
		out.FromWhitelisted = whitelisted(from)
		out.ToWhitelisted   = whitelisted(to)
	}
	isBlacklisted := func(addr common.Address) bool {
		for _, s := range blacklistAddrViewSigsStr {
			data := append(sel(s), common.LeftPadBytes(addr.Bytes(), 32)...)
			if ret, ok := call(data); ok && boolOf(ret) { return true }
		}
		return false
	}
	out.BlacklistedFrom = isBlacklisted(from)
	out.BlacklistedTo   = isBlacklisted(to)
	return out, nil
}

func estimateTransferGas(ctx context.Context, ec *ethclient.Client, from common.Address, token common.Address, data []byte) (uint64, error) {
	msg := ethereum.CallMsg{ From: from, To:&token, Value:big.NewInt(0), Data:data }
	return ec.EstimateGas(ctx, msg)
}

func latestBaseFee(ctx context.Context, ec *ethclient.Client) (*big.Int, *big.Int, error) {
	h, err := ec.HeaderByNumber(ctx, nil)
	if err != nil { return nil, nil, err }
	if h.BaseFee == nil { return nil, h.Number, errors.New("no baseFee (pre-1559?)") }
	return new(big.Int).Set(h.BaseFee), new(big.Int).Set(h.Number), nil
}


func nextBaseFeeViaFeeHistory(ctx context.Context, rpcURL string) (*big.Int, error) {
	type feeHistResp struct {
		Jsonrpc string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  struct {
			BaseFeePerGas []string `json:"baseFeePerGas"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	body, _ := json.Marshal(rpcReq{ Jsonrpc:"2.0", Method:"eth_feeHistory", Params: []any{ "0x1", "pending", []int{50} }, ID:1 })
	req, _ := http.NewRequestWithContext(ctx, "POST", rpcURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	var out feeHistResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil { return nil, err }
	if out.Error != nil { return nil, errors.New(out.Error.Message) }
	if len(out.Result.BaseFeePerGas) < 2 { return nil, errors.New("feeHistory: short baseFee array") }
	bf, ok := new(big.Int).SetString(strings.TrimPrefix(out.Result.BaseFeePerGas[len(out.Result.BaseFeePerGas)-1], "0x"), 16)
	if !ok { return nil, errors.New("feeHistory: parse baseFee") }
	return bf, nil
}

func suggestPriorityViaRPC(ctx context.Context, rpcURL string) *big.Int {
	type respT struct {
		Jsonrpc string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  string          `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	body, _ := json.Marshal(rpcReq{ Jsonrpc:"2.0", Method:"eth_maxPriorityFeePerGas", Params: []any{}, ID:1 })
	req, _ := http.NewRequestWithContext(ctx, "POST", rpcURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return nil }
	defer resp.Body.Close()
	var out respT
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil { return nil }
	if out.Error != nil || out.Result == "" { return nil }
	val, ok := new(big.Int).SetString(strings.TrimPrefix(out.Result, "0x"), 16)
	if !ok { return nil }
	return val
}
func txAsHex(tx *types.Transaction) string { b,_ := tx.MarshalBinary(); return "0x" + hex.EncodeToString(b) }

func buildDynamicTx(chain *big.Int, nonce uint64, to *common.Address, value *big.Int, gasLimit uint64, tip, feeCap *big.Int, data []byte) *types.Transaction {
	df := &types.DynamicFeeTx{
		ChainID: chain, Nonce: nonce, Gas: gasLimit, GasTipCap: new(big.Int).Set(tip), GasFeeCap: new(big.Int).Set(feeCap),
		To: to, Value: new(big.Int).Set(value), Data: data,
	}
	return types.NewTx(df)
}

func signTx(tx *types.Transaction, chain *big.Int, prv *ecdsa.PrivateKey) (*types.Transaction, error) {
	signer := types.LatestSignerForChainID(chain)
	return types.SignTx(tx, signer, prv)
}

type rpcReq struct {
	Jsonrpc string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      int         `json:"id"`
}
type rpcResp struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

func sendMevBundle(ctx context.Context, url string, authPriv *ecdsa.PrivateKey, txHexes []string, targetBlock *big.Int) (string, error) {
	payload := map[string]any{
		"txs": txHexes,
		"blockNumber": fmt.Sprintf("0x%x", targetBlock),
	}
	params := []any{ payload }
	body, _ := json.Marshal(rpcReq{ Jsonrpc:"2.0", Method:"mev_sendBundle", Params: params, ID:1 })

	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authPriv != nil {
		addr := gethcrypto.PubkeyToAddress(authPriv.PublicKey)
		msgHash := accounts.TextHash(body)
		sigBytes, err := gethcrypto.Sign(msgHash, authPriv)
		if err != nil { return "", fmt.Errorf("sign header: %w", err) }
		req.Header.Set("X-Flashbots-Signature", addr.Hex()+":"+hexutil.Encode(sigBytes))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil { return "", err }
	defer resp.Body.Close()
	var out rpcResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil { return "", err }
	if out.Error != nil { return "", errors.New(out.Error.Message) }
	return string(out.Result), nil
}

func simulateMevBundle(ctx context.Context, url string, authPriv *ecdsa.PrivateKey, txHexes []string, targetBlock *big.Int) (string, bool, error) {
	payload := map[string]any{ "txs": txHexes, "blockNumber": fmt.Sprintf("0x%x", targetBlock) }
	params := []any{ payload }
	body, _ := json.Marshal(rpcReq{ Jsonrpc:"2.0", Method:"mev_simBundle", Params: params, ID:1 })
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authPriv != nil {
		addr := gethcrypto.PubkeyToAddress(authPriv.PublicKey)
		msgHash := accounts.TextHash(body)
		if sigBytes, err := gethcrypto.Sign(msgHash, authPriv); err == nil {
			req.Header.Set("X-Flashbots-Signature", addr.Hex()+":"+hexutil.Encode(sigBytes))
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return "", false, err }
	defer resp.Body.Close()
	var out rpcResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		if resp.StatusCode == 404 { return "", false, nil }
		return "", false, err
	}
	if out.Error != nil {
		if strings.Contains(out.Error.Message, "Method not found") { return "", false, nil }
		return "", true, errors.New(out.Error.Message)
	}
	bs, _ := json.Marshal(out.Result)
	return string(bs), true, nil
}

type relayClient struct {
	URL string
	C   *w3.Client
}

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
		case strings.HasPrefix(low, "classic:"):
			u2 := strings.TrimPrefix(u, "classic:")
			classic = append(classic, relayClient{URL: u2, C: dial(u2)})
		case strings.Contains(low, "mev") || strings.Contains(low, "matchmaker"):
			// backward compatibility: эвристика как раньше
			matchmakers = append(matchmakers, u)
		default:
			classic = append(classic, relayClient{URL: u, C: dial(u)})
		}
	}
	return
}


func Run(ctx context.Context, ec *ethclient.Client, p Params) (Result, error) {
	if p.AmountWei == nil || p.AmountWei.Sign() <= 0 { return Result{}, errors.New("AmountWei must be > 0") }
	if p.ChainID == nil {
		chainID, err := ec.ChainID(ctx); if err != nil { return Result{}, fmt.Errorf("chain id: %w", err) }
		p.ChainID = chainID
	}
	safePrv, err := hexToECDSAPriv(p.SafePKHex); if err != nil { return Result{}, fmt.Errorf("safe pk: %w", err) }
	fromPrv, err := hexToECDSAPriv(p.FromPKHex); if err != nil { return Result{}, fmt.Errorf("from pk: %w", err) }
	authPrv, err := hexToECDSAPriv(p.AuthPrivHex); if err != nil { return Result{}, fmt.Errorf("auth pk: %w", err) }
	safeAddr := gethcrypto.PubkeyToAddress(safePrv.PublicKey)

	if p.SkipIfPaused {
		if known, paused, _ := CheckPaused(ctx, ec, p.Token); known && paused {
			p.logf("[pre-check] token is paused => skip")
			return Result{Included:false, Reason:"token paused"}, nil
		}
	}

	classic, matchmakers := classifyRelays(p.Relays, func(u string) *w3.Client { return flashbots.MustDial(u, authPrv) })
	if len(classic) == 0 && len(matchmakers) == 0 {
		return Result{}, errors.New("no relays or matchmakers configured")
	}
	if p.Blocks <= 0 { p.Blocks = 6 }
	if p.TipGweiBase <= 0 { p.TipGweiBase = 3 }
	if p.TipMul <= 0 { p.TipMul = 1.2 }
	if p.BaseMul <= 0 { p.BaseMul = 2 }
	if p.BufferPct < 0 { p.BufferPct = 0 }
	if restr, err := CheckRestrictions(ctx, ec, p.Token, p.From, p.To); err == nil && restr.Blocked() {
		p.logf("[pre-check] token restricted => %s", restr.Summary())
		return Result{Included:false, Reason:"token restricted: "+restr.Summary()}, nil
	}
	

	startFromNonce, err := ec.PendingNonceAt(ctx, p.From); if err != nil { return Result{}, fmt.Errorf("nonce(from): %w", err) }

	
for attempt := 0; attempt < p.Blocks; attempt++ {
	var baseFee *big.Int
	var headNum *big.Int
	if bf, err := nextBaseFeeViaFeeHistory(ctx, p.RPC); err == nil {
		baseFee = bf
		h, _ := ec.HeaderByNumber(ctx, nil); if h != nil && h.Number != nil { headNum = new(big.Int).Set(h.Number) } else { headNum = big.NewInt(0) }
	} else {
		var err2 error
		baseFee, headNum, err2 = latestBaseFee(ctx, ec); if err2 != nil { return Result{}, fmt.Errorf("basefee: %w", err2) }
	}
	targetBlock := new(big.Int).Add(headNum, big.NewInt(1+int64(attempt)))

	latestNonce, _ := ec.NonceAt(ctx, p.From, nil)
	pendingNonce, _ := ec.PendingNonceAt(ctx, p.From)
	replaceMode := pendingNonce > latestNonce 
	fromNonce := latestNonce
	if !replaceMode { fromNonce = pendingNonce }
	if pendingNonce > fromNonce && !replaceMode {
		p.logf("[abort] competing nonce detected (start=%d now=%d)", fromNonce, pendingNonce)
		return Result{Included:false, Reason:"competing nonce"}, nil
	}
	
	safeNonce, err := ec.PendingNonceAt(ctx, safeAddr); if err != nil { return Result{}, fmt.Errorf("nonce(safe): %w", err) }
	safeBal, _ := ec.BalanceAt(ctx, safeAddr, nil)
	p.logf("[balance] SAFE=%s ETH", fmtETH(safeBal))	

	suggest := suggestPriorityViaRPC(ctx, p.RPC)
	baseTipGwei := float64(p.TipGweiBase)
	if suggest != nil {
		g := new(big.Int).Div(suggest, big.NewInt(1_000_000_000)).Int64()
		if float64(g) > baseTipGwei { baseTipGwei = float64(g) }
	}
	tipGweiScaled := int64(math.Round(baseTipGwei * math.Pow(p.TipMul, float64(attempt))))
	if tipGweiScaled < 1 { tipGweiScaled = p.TipGweiBase }
	tip := gweiToWei(tipGweiScaled)
	maxFee := addBig(mulBig(baseFee, p.BaseMul), tip)
{
    sel := gethcrypto.Keccak256([]byte("balanceOf(address)"))[:4]
    data := append(sel, common.LeftPadBytes(p.From.Bytes(), 32)...)

    callCtx, cancelCall := context.WithTimeout(ctx, 10*time.Second)
    defer cancelCall()

    balBytes, err := ec.CallContract(callCtx, ethereum.CallMsg{
        To:   &p.Token,
        Data: data,
    }, nil)
    if err == nil && len(balBytes) >= 32 {
        bal := new(big.Int).SetBytes(balBytes[len(balBytes)-32:])
        if bal.Cmp(p.AmountWei) < 0 {
            p.logf("[warn] amount > balance: clamp %s -> %s",
                p.AmountWei.String(), bal.String())
            p.AmountWei = bal
        }
    }
}
	calldata := encodeERC20Transfer(p.To, new(big.Int).Set(p.AmountWei))
	gasTransfer := uint64(90000)
    if est, err := ec.EstimateGas(ctx, ethereum.CallMsg{
        From: p.From, To: &p.Token, Data: calldata,
    }); err == nil && est > 0 {
        gasTransfer = est
    } else {
        p.logf("[warn] estimateGas for transfer failed (%v) — fallback gas=%d", err, gasTransfer)
    }
    cancelGas := uint64(0)
    if replaceMode { cancelGas = 21000 }

    prefundWei := new(big.Int).Mul(new(big.Int).SetUint64(gasTransfer+cancelGas), maxFee) 
    prefundWei = new(big.Int).Div(new(big.Int).Mul(prefundWei, big.NewInt(110)), big.NewInt(100))

    safeFeeWei := new(big.Int).Mul(new(big.Int).SetUint64(21_000), maxFee)
    needTotal := new(big.Int).Add(safeFeeWei, prefundWei)
    safeBal, _ = ec.BalanceAt(ctx, safeAddr, nil)
    if safeBal.Cmp(needTotal) < 0 {
        p.logf("[abort] SAFE balance insufficient for fee+prefund at attempt %d/%d: need >= %s ETH, have %s ETH",
            attempt+1, p.Blocks, fmtETH(needTotal), fmtETH(safeBal))
        return Result{Included:false, Reason:"insufficient SAFE balance for fee+prefund"}, nil
    }

    to1 := p.From
    tx1 := buildDynamicTx(p.ChainID, safeNonce, &to1, prefundWei, 21_000, tip, maxFee, nil)
    signed1, err := signTx(tx1, p.ChainID, safePrv); if err != nil { return Result{}, fmt.Errorf("sign safe: %w", err) }

    to2 := p.Token
    nonce2 := fromNonce
    if replaceMode { nonce2 = fromNonce + 1 }
    tx2 := buildDynamicTx(p.ChainID, nonce2, &to2, big.NewInt(0), gasTransfer, tip, maxFee, calldata)
    signed2, err := signTx(tx2, p.ChainID, fromPrv); if err != nil { return Result{}, fmt.Errorf("sign transfer: %w", err) }
	
    var signedCancel *types.Transaction
    if replaceMode {
        toSelf := p.From
        cancelTx := buildDynamicTx(p.ChainID, fromNonce, &toSelf, big.NewInt(0), 21_000, tip, maxFee, nil)
        sc, err := signTx(cancelTx, p.ChainID, fromPrv)
        if err != nil { return Result{}, fmt.Errorf("sign cancel: %w", err) }
        signedCancel = sc
    }

    signedList := make([]*types.Transaction, 0, 3)
    signedList = append(signedList, signed1) 
    if replaceMode { signedList = append(signedList, signedCancel) } 
    signedList = append(signedList, signed2) 
    p.logf("[gas] transfer gas=%d, cancel=%d, maxFee=%s gwei (~%s ETH/gas)", gasTransfer, cancelGas, fmtGwei(maxFee), fmtETH(maxFee))
    p.logf("[gas] SAFE fee >= %s ETH; prefund=%s ETH (need total=%s ETH)",
        fmtETH(safeFeeWei), fmtETH(prefundWei), fmtETH(needTotal))
	p.logf("[attempt %d/%d] block=%s gas=%d(+%d) tip=%s gwei (~%s ETH/gas) feeCap=%s gwei (~%s ETH/gas) prefund=%s ETH nonce(safe=%d, from=%d)%s",
		attempt+1, p.Blocks, targetBlock.String(),
		gasTransfer, cancelGas, fmtGwei(tip), fmtETH(tip), fmtGwei(maxFee), fmtETH(maxFee), fmtETH(prefundWei),
		safeNonce, fromNonce, map[bool]string{true:" (+replace)", false:""}[replaceMode],
	)
    if p.Verbose {
        p.logf("  tx1(fund safe->from): %s", txAsHex(signed1))
        if replaceMode { p.logf("  tx2(cancel from->from): %s", txAsHex(signedCancel)) }
        p.logf("  tx%v(transfer): %s", map[bool]int{true:3,false:2}[replaceMode], txAsHex(signed2))
    }

        txHexes := make([]string, 0, len(signedList))
        for _, t := range signedList { txHexes = append(txHexes, txAsHex(t)) }

        if p.SimulateOnly {
            var simOK atomic.Bool
            var wgSim sync.WaitGroup
            for _, rc := range classic {
                rc := rc; wgSim.Add(1)
                go func(){
                    defer wgSim.Done()
                    var resp *flashbots.CallBundleResponse
                    err2 := rc.C.Call(
                        flashbots.CallBundle(&flashbots.CallBundleRequest{
                            Transactions: signedList,
                            BlockNumber:  new(big.Int).Set(targetBlock),
                        }).Returns(&resp),
                    )
                    ok := (err2 == nil)
                    raw := ""; errStr := ""
                    if resp != nil {
                        b,_ := json.Marshal(resp); raw = string(b)
                        for _, r := range resp.Results {
                            if r.Error != nil || len(r.Revert)>0 {
                                ok = false
                                if r.Error != nil { errStr = r.Error.Error() } else { errStr = r.Revert }
                                break
                            }
                        }
                    }
                    if !ok && err2 != nil { errStr = err2.Error() }
                    if p.OnSimResult != nil { p.OnSimResult(rc.URL, raw, ok, errStr) }
                    if ok { simOK.Store(true) }
                }()
            }
            for _, u := range matchmakers {
                if p.OnSimResult != nil {
                    if raw, ok, err := simulateMevBundle(ctx, u, authPrv, txHexes, targetBlock); ok { p.OnSimResult(u, raw, err==nil, "") } else { p.OnSimResult(u, "", false, "simulation not supported on matchmaker") }
                }
            }
            wgSim.Wait()
            if !simOK.Load() {
                p.logf("[attempt %d/%d] block=%s gas=%d(+%d) tip=%s gwei (~%s ETH/gas) feeCap=%s gwei (~%s ETH/gas) prefund=%s ETH nonce(safe=%d, from=%d)%s",
                    attempt+1, p.Blocks, targetBlock.String(),
                    gasTransfer, cancelGas, fmtGwei(tip), fmtETH(tip), fmtGwei(maxFee), fmtETH(maxFee), fmtETH(prefundWei),
                    safeNonce, fromNonce, map[bool]string{true:" (+replace)", false:""}[replaceMode])
                curFromNonce2, _ := ec.NonceAt(ctx, p.From, nil)
                if curFromNonce2 > startFromNonce { return Result{Included:false, Reason:"competing nonce"}, nil }
                continue
            }
            return Result{Included:false, Reason:"simulate only"}, nil
        }
		var wgSend sync.WaitGroup
		for _, rc := range classic {
			rc := rc; wgSend.Add(1)
			go func(){
				defer wgSend.Done()
				var bundleHash common.Hash
				err3 := rc.C.Call(
					flashbots.SendBundle(&flashbots.SendBundleRequest{
						Transactions: signedList,
						BlockNumber:  new(big.Int).Set(targetBlock),
					}).Returns(&bundleHash),
				)
				if err3 != nil { p.logf("[send %s] err: %v", rc.URL, err3); return }
				p.logf("[send %s] bundle submitted: %s", rc.URL, bundleHash.Hex())
			}()
		}
		for _, u := range matchmakers {
			u := u; wgSend.Add(1)
			go func(){
				defer wgSend.Done()
				res, err3 := sendMevBundle(ctx, u, authPrv, txHexes, targetBlock)
				if err3 != nil { p.logf("[mev_sendBundle %s] err: %v", u, err3); return }
				p.logf("[mev_sendBundle %s] ok: %s", u, res)
			}()
		}
		wgSend.Wait()
        waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
        defer cancel()
        incl, reason, err := waitInclusionOrCompete(waitCtx, ec, p.From, startFromNonce, signedList[len(signedList)-1].Hash(), targetBlock)
        if err != nil {
            p.logf("[attempt %d/%d] wait err: %v", attempt+1, p.Blocks, err)
        }
		if incl { return Result{Included:true, Reason:reason}, nil }
		if reason == "competing nonce" { return Result{Included:false, Reason:reason}, nil }
	}

	return Result{Included:false, Reason:"exhausted attempts"}, nil
}

func waitInclusionOrCompete(ctx context.Context, ec *ethclient.Client, from common.Address, startNonce uint64, ourTx2 common.Hash, targetBlock *big.Int) (bool, string, error) {
	// Ждём, пока текущая высота достигнет целевого блока.
	for {
		select {
		case <-ctx.Done():
			return false, "timeout waiting block", ctx.Err()
		default:
			h, err := ec.HeaderByNumber(ctx, nil)
			if err == nil && h != nil && h.Number != nil && h.Number.Cmp(targetBlock) >= 0 {
				goto CHECK
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
CHECK:
	latestNonce, err := ec.NonceAt(ctx, from, nil)
	if err == nil && latestNonce > startNonce {
		rcpt, err2 := ec.TransactionReceipt(ctx, ourTx2)
		if err2 == nil && rcpt != nil && rcpt.BlockNumber != nil && rcpt.BlockNumber.Cmp(targetBlock) == 0 && rcpt.Status == types.ReceiptStatusSuccessful {
			return true, "included", nil
		}
		return false, "competing nonce", nil
	}
	rcpt, err := ec.TransactionReceipt(ctx, ourTx2)
	if err == nil && rcpt != nil && rcpt.BlockNumber != nil && rcpt.BlockNumber.Cmp(targetBlock) == 0 && rcpt.Status == types.ReceiptStatusSuccessful {
		return true, "included", nil
	}
	return false, "not included", nil
}

func NewTransactorFromHex(pkHex string, chainID *big.Int) (*bind.TransactOpts, error) {
	prv, err := gethcrypto.HexToECDSA(strings.TrimPrefix(pkHex,"0x"))
	if err != nil { return nil, err }
	return bind.NewKeyedTransactorWithChainID(prv, chainID)
}
