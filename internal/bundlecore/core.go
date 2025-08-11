package bundlecore

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ---------- Public API ----------

type Params struct {
	RPC         string
	ChainID     *big.Int
	Relays      []string
	AuthPrivHex string

	Token     common.Address
	From      common.Address
	To        common.Address
	AmountWei *big.Int

	SafePKHex string // funding wallet (sends ETH to From)
	FromPKHex string // compromised wallet

	Blocks      int
	TipGweiBase int64
	TipMul      float64
	BaseMul     int64
	BufferPct   int64

	SimulateOnly bool

	Logf        func(format string, a ...any)
	OnSimResult func(relayURL string, rawJSON string, ok bool, err string)
}

type Output struct {
	Reason   string
	Included bool
}

// ---------- Flashbots client ----------

type flashClient struct {
	url     string
	authPK  *ecdsa.PrivateKey
	address common.Address
	httpc   *http.Client
}

func newFlashClient(url, authPrivHex string) (*flashClient, error) {
	h := strings.TrimPrefix(strings.TrimSpace(authPrivHex), "0x")
	if h == "" {
		return nil, fmt.Errorf("auth private key is empty")
	}
	pk, err := crypto.HexToECDSA(h)
	if err != nil {
		return nil, fmt.Errorf("auth pk parse: %w", err)
	}
	addr := crypto.PubkeyToAddress(pk.PublicKey)
	return &flashClient{
		url:     strings.TrimSpace(url),
		authPK:  pk,
		address: addr,
		httpc:   &http.Client{Timeout: 12 * time.Second},
	}, nil
}

func (c *flashClient) signBody(b []byte) string {
	hash := crypto.Keccak256(b)
	sig, err := crypto.Sign(hash, c.authPK)
	if err != nil {
		return ""
	}
	return c.address.Hex() + ":" + "0x" + hex.EncodeToString(sig)
}

func (c *flashClient) rpc(ctx context.Context, method string, params any) (status int, body []byte, err error) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Flashbots-Signature", c.signBody(b))
	resp, err := c.httpc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	all := new(bytes.Buffer)
	_, _ = all.ReadFrom(resp.Body)
	return resp.StatusCode, all.Bytes(), nil
}

type simResponse struct {
	OK      bool
	RawJSON string
	Error   string
}

func (c *flashClient) callBundle(ctx context.Context, txs []string, target uint64, stateTag string) (*simResponse, error) {
	params := []any{
		map[string]any{
			"txs":              txs,
			"blockNumber":      hexutil.EncodeUint64(target),
			"stateBlockNumber": stateTag, // "latest"
		},
	}
	code, body, err := c.rpc(ctx, "eth_callBundle", params)
	raw := string(body)
	if err != nil {
		return &simResponse{OK: false, RawJSON: raw, Error: err.Error()}, err
	}
	if code != 200 {
		return &simResponse{OK: false, RawJSON: raw, Error: fmt.Sprintf("http %d", code)}, fmt.Errorf("http %d", code)
	}
	// If RPC error field present -> fail
	var wrap struct {
		Error any `json:"error"`
	}
	_ = json.Unmarshal(body, &wrap)
	if wrap.Error != nil {
		return &simResponse{OK: false, RawJSON: raw, Error: "rpc error"}, nil
	}
	return &simResponse{OK: true, RawJSON: raw, Error: ""}, nil
}

func (c *flashClient) sendBundle(ctx context.Context, txs []string, target uint64) (int, []byte, error) {
	params := []any{
		map[string]any{
			"txs":         txs,
			"blockNumber": hexutil.EncodeUint64(target),
		},
	}
	return c.rpc(ctx, "eth_sendBundle", params)
}

// ---------- Builder ----------

var erc20ABI abi.ABI

func init() {
	const erc20 = `[{"inputs":[{"internalType":"address","name":"recipient","type":"address"},{"internalType":"uint256","name":"amount","type":"uint256"}],"name":"transfer","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"nonpayable","type":"function"}]`
	ab, _ := abi.JSON(strings.NewReader(erc20))
	erc20ABI = ab
}

func powf(x float64, y int) float64 { return math.Exp(math.Log(x) * float64(y)) }

// Run builds a (fund + token) bundle, simulates and optionally sends it, with retries over blocks.
func Run(ctx context.Context, ec *ethclient.Client, p Params) (*Output, error) {
	logf := func(s string, a ...any) {
		if p.Logf != nil {
			p.Logf(s, a...)
		}
	}

	if p.AmountWei == nil || p.AmountWei.Sign() <= 0 {
		return nil, fmt.Errorf("amount is zero")
	}
	if len(p.Relays) == 0 {
		return nil, fmt.Errorf("no relays configured")
	}
	if p.ChainID == nil {
		return nil, fmt.Errorf("chainID is nil")
	}

	if ec == nil {
		var err error
		ec, err = ethclient.Dial(p.RPC)
		if err != nil {
			return nil, fmt.Errorf("dial rpc: %w", err)
		}
	}

	logf("prepare: token=%s from=%s to=%s amountWei=%s", p.Token.Hex(), p.From.Hex(), p.To.Hex(), p.AmountWei.String())

	// Keys
	safePK, err := crypto.HexToECDSA(strings.TrimPrefix(p.SafePKHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("safe pk: %w", err)
	}
	fromPK, err := crypto.HexToECDSA(strings.TrimPrefix(p.FromPKHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("from pk: %w", err)
	}
	safeAddr := crypto.PubkeyToAddress(safePK.PublicKey)

	// Nonces
	safeNonce, err := ec.PendingNonceAt(ctx, safeAddr)
	if err != nil {
		return nil, fmt.Errorf("nonce(safe): %w", err)
	}
	fromNonce, err := ec.PendingNonceAt(ctx, p.From)
	if err != nil {
		return nil, fmt.Errorf("nonce(from): %w", err)
	}

	// Head / target
	logf("fetch head...")
	head, err := ec.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("head: %w", err)
	}
	baseFee := head.BaseFee
	if baseFee == nil {
		baseFee = big.NewInt(0)
	}
	logf("head=%d baseFee=%s", head.Number.Uint64(), baseFee.String())
	target0 := new(big.Int).Add(head.Number, big.NewInt(1)).Uint64()

	// Data for token transfer (from -> to)
	data, err := erc20ABI.Pack("transfer", p.To, p.AmountWei)
	if err != nil {
		return nil, fmt.Errorf("erc20 pack: %w", err)
	}

	// Estimate gas for token tx
	logf("estimating gas for ERC20.transfer...")
	est, err := ec.EstimateGas(ctx, ethereum.CallMsg{
		From: p.From,
		To:   &p.Token,
		Data: data,
	})
	if err != nil {
		// fallback constant
		logf("estimateGas failed (%v), fallback to 70000", err)
		est = 70000
	}
	// add buffer
	logf("gasEstimate=%d bufferPct=%d", est, p.BufferPct)
	estPlus := uint64(float64(est) * (1.0 + float64(p.BufferPct)/100.0))

	// Strategy
	baseMul := big.NewInt(p.BaseMul)
	blocksForward := p.Blocks
	if blocksForward <= 0 {
		blocksForward = 5
	}
	tipBase := p.TipGweiBase
	if tipBase <= 0 {
		tipBase = 2
	}
	tipMul := p.TipMul
	if tipMul < 1.0 {
		tipMul = 1.25
	}
	logf("strategy: blocks=%d tipBaseGwei=%d tipMul=%.2f baseMul=%d", blocksForward, tipBase, tipMul, p.BaseMul)

	// Helper to build signed txs & raw
	makeBundle := func(tipGwei int64) (fundTx *types.Transaction, tokenTx *types.Transaction, fundRaw, tokenRaw string, err error) {
		// dynamic fees
		tip := new(big.Int).Mul(big.NewInt(tipGwei), big.NewInt(1_000_000_000))
		feeCap := new(big.Int).Mul(baseFee, baseMul)
		feeCap.Add(feeCap, tip)

		// FUND: safe -> From (value enough for token gas)
		logf("fees: tipGwei=%d feeCap=%s tip=%s", tipGwei, feeCap.String(), tip.String())
		needWei := new(big.Int).Mul(new(big.Int).SetUint64(estPlus), feeCap)
		needWei.Add(needWei, new(big.Int).Mul(new(big.Int).SetUint64(estPlus), tip))
		needWei.Add(needWei, new(big.Int).Mul(big.NewInt(21_000), feeCap)) // safety top-up
		logf("estPlus=%d fundValueWei=%s", estPlus, needWei.String())

		fund := &types.DynamicFeeTx{
			ChainID:   p.ChainID,
			Nonce:     safeNonce,
			GasTipCap: tip,
			GasFeeCap: feeCap,
			Gas:       21000,
			To:        &p.From,
			Value:     needWei,
			Data:      nil,
		}
		token := &types.DynamicFeeTx{
			ChainID:   p.ChainID,
			Nonce:     fromNonce,
			GasTipCap: tip,
			GasFeeCap: feeCap,
			Gas:       estPlus,
			To:        &p.Token,
			Value:     big.NewInt(0),
			Data:      data,
		}
		stx1, err := types.SignNewTx(safePK, types.LatestSignerForChainID(p.ChainID), fund)
		if err != nil {
			return nil, nil, "", "", err
		}
		stx2, err := types.SignNewTx(fromPK, types.LatestSignerForChainID(p.ChainID), token)
		if err != nil {
			return nil, nil, "", "", err
		}
		br1, _ := stx1.MarshalBinary()
		br2, _ := stx2.MarshalBinary()
		return stx1, stx2, "0x" + hex.EncodeToString(br1), "0x" + hex.EncodeToString(br2), nil
	}

	// Init relays
	var rels []*flashClient
	for _, u := range p.Relays {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		cl, err := newFlashClient(u, p.AuthPrivHex)
		if err != nil {
			logf("relay %s init error: %v", u, err)
			continue
		}
		rels = append(rels, cl)
	}
	if len(rels) == 0 {
		return nil, fmt.Errorf("no valid relays after init")
	}

	// First bundle build
	_, _, raw1, raw2, err := makeBundle(tipBase)
	if err != nil {
		return nil, err
	}
	txs := []string{raw1, raw2}

	// Simulation only path
	if p.SimulateOnly {
		okAny := false
		for _, r := range rels {
			logf("pre-simulate on relay %s target=%d", r.url, target0)
			res, err := r.callBundle(ctx, txs, target0, "latest")
			if p.OnSimResult != nil {
				if err != nil {
					p.OnSimResult(r.url, res.RawJSON, false, err.Error())
				} else {
					p.OnSimResult(r.url, res.RawJSON, res.OK, res.Error)
				}
			}
			if err == nil && res.OK {
				okAny = true
			}
		}
		if okAny {
			return &Output{Reason: "simulate ok on at least one relay", Included: false}, nil
		}
		return &Output{Reason: "simulate failed on all relays", Included: false}, nil
	}

	// Send loop across blocks
	for blk := 0; blk < blocksForward; blk++ {
		target := target0 + uint64(blk)
		tipForBlk := int64(float64(tipBase) * powf(tipMul, blk))
		logf("block attempt #%d target=%d tipGwei=%d", blk+1, target, tipForBlk)

		_, _, raw1, raw2, err = makeBundle(tipForBlk)
		if err != nil {
			return nil, err
		}
		txs = []string{raw1, raw2}

		// pre-simulation to filter relays
		okRelays := make([]*flashClient, 0, len(rels))
		for _, r := range rels {
			logf("pre-simulate on relay %s", r.url)
			res, err := r.callBundle(ctx, txs, target, "latest")
			if p.OnSimResult != nil {
				if err != nil {
					p.OnSimResult(r.url, res.RawJSON, false, err.Error())
				} else {
					p.OnSimResult(r.url, res.RawJSON, res.OK, res.Error)
				}
			}
			if err == nil && res.OK {
				okRelays = append(okRelays, r)
			}
		}
		logf("relays passed simulation: %d/%d", len(okRelays), len(rels))
		if len(okRelays) == 0 {
			continue
		}

		// parallel send to relays
		type sent struct {
			ok   bool
			url  string
			body string
			err  error
		}
		ch := make(chan sent, len(okRelays))
		for _, r := range okRelays {
			go func(rc *flashClient) {
				code, body, err := rc.sendBundle(ctx, txs, target)
				if err != nil {
					ch <- sent{false, rc.url, string(body), err}
					return
				}
				if code != 200 {
					ch <- sent{false, rc.url, string(body), fmt.Errorf("http %d", code)}
					return
				}
				ch <- sent{true, rc.url, string(body), nil}
			}(r)
		}

		timeout := time.NewTimer(1200 * time.Millisecond)
		success := false
		for i := 0; i < len(okRelays); i++ {
			select {
			case s := <-ch:
				if s.ok {
					logf("bundle sent via %s (target %d)", s.url, target)
					success = true
				} else {
					logf("relay %s send error: %v", s.url, s.err)
				}
			case <-timeout.C:
				// don't block forever; we still gather what arrives later
			}
		}
		if success {
			return &Output{Reason: fmt.Sprintf("bundle sent for block %d (tip %d gwei)", target, tipForBlk), Included: true}, nil
		}
	}

	return &Output{Reason: "exhausted all blocks without inclusion", Included: false}, nil
}
