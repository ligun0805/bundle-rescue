package bundlecore

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	fb "github.com/you/bundle-rescue/internal/flashbots"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

var erc20ABIJSON = `[{"inputs":[{"internalType":"address","name":"to","type":"address"},{"internalType":"uint256","name":"value","type":"uint256"}],"name":"transfer","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"nonpayable","type":"function"}]`
var erc20ABI abi.ABI
func init(){ var err error; erc20ABI, err = abi.JSON(strings.NewReader(erc20ABIJSON)); if err!=nil{ panic(err) } }

type Params struct {
	RPC          string
	ChainID      *big.Int
	Relays       []string
	AuthPrivHex  string
	Token        common.Address
	From         common.Address
	To           common.Address
	AmountWei    *big.Int
	SafePKHex    string
	FromPKHex    string
	Blocks       int
	TipGweiBase  int64
	TipMul       float64
	BaseMul      int64
	BufferPct    int64
	SimulateOnly bool
	OnSimResult func(relayURL string, rawJSON string, ok bool, err string)
	Logf         func(string, ...any)
}

type Outcome struct {
	FundTx    common.Hash
	XferTx    common.Hash
	Included  bool
	Reason    string
}

func logNop(string, ...any) {}

func Run(ctx context.Context, ec *ethclient.Client, p Params) (*Outcome, error) {
	if p.Logf == nil { p.Logf = logNop }

	data, _ := erc20ABI.Pack("transfer", p.To, p.AmountWei)
	g2, err := ec.EstimateGas(ctx, ethereum.CallMsg{ From: p.From, To: &p.Token, Data: data })
	if err != nil { return nil, fmt.Errorf("estimate gas: %w", err) }

	hdr, err := ec.HeaderByNumber(ctx, nil)
	if err != nil { return nil, err }
	base := hdr.BaseFee

	tipWei := big.NewInt(p.TipGweiBase * 1_000_000_000)
	feeCap := new(big.Int).Add(new(big.Int).Mul(base, big.NewInt(p.BaseMul)), tipWei)

	fromKey, fromAddr, err := priv(p.FromPKHex)
	if err != nil { return nil, err }
	safeKey, safeAddr, err := priv(p.SafePKHex)
	if err != nil { return nil, err }
	n2, err := ec.PendingNonceAt(ctx, fromAddr)
	if err != nil { return nil, err }
	n1, err := ec.PendingNonceAt(ctx, safeAddr)
	if err != nil { return nil, err }

	need := new(big.Int).Mul(new(big.Int).SetUint64(g2), feeCap)
	if p.BufferPct > 0 { buf := new(big.Int).Div(new(big.Int).Mul(need, big.NewInt(p.BufferPct)), big.NewInt(100)); need = new(big.Int).Add(need, buf) }

	mkTransfer := func(tip, cap *big.Int) *types.DynamicFeeTx {
		return &types.DynamicFeeTx{ ChainID: p.ChainID, Nonce: n2, GasTipCap: tip, GasFeeCap: cap, Gas: g2, To: &p.Token, Value: big.NewInt(0), Data: data }
	}
	mkFund := func(tip, cap *big.Int, value *big.Int) *types.DynamicFeeTx {
		g1 := uint64(21_000)
		return &types.DynamicFeeTx{ ChainID: p.ChainID, Nonce: n1, GasTipCap: tip, GasFeeCap: new(big.Int).Add(cap, big.NewInt(1)), Gas: g1, To: &fromAddr, Value: value }
	}

	var rels []*flashClient
	for _, u := range p.Relays {
		u = strings.TrimSpace(u); if u=="" { continue }
		cli, err := fb.NewClient(u, p.AuthPrivHex)
		if err != nil { return nil, fmt.Errorf("relay %s: %w", u, err) }
		rels = append(rels, &flashClient{u, cli})
	}
	if len(rels)==0 { return nil, fmt.Errorf("no relays provided") }

	ctxCancel, cancel := context.WithCancel(ctx)
	defer cancel()

	status := make(chan string, 1)
	go monitor(ctxCancel, ec, fromAddr, n2, status, p.Logf)

	out := &Outcome{}
	startBlk, _ := ec.BlockNumber(ctx)
	tip := new(big.Int).Set(tipWei)
	cap := new(big.Int).Set(feeCap)

	for i := 1; i <= p.Blocks; i++ {
		select { case <-ctxCancel.Done(): return out, ctx.Err(); default: }
		target := startBlk + uint64(i)

		if i > 1 {
			mul := p.TipMul
			if mul < 1.0 { mul = 1.0 }
			tip = mulBigFloat(tip, mul)
			cap = new(big.Int).Add(new(big.Int).Mul(base, big.NewInt(p.BaseMul)), tip)
		}

		stx2 := sign(types.NewTx(mkTransfer(tip, cap)), fromKey)
		stx1 := sign(types.NewTx(mkFund(tip, cap, need)), safeKey)
		out.FundTx = stx1.Hash(); out.XferTx = stx2.Hash()

		raw1 := mustRlp(stx1)
		raw2 := mustRlp(stx2)
		raws := []string{raw1, raw2}

		p.Logf("[attempt %d/%d] target=%d tip=%s cap=%s needWei=%s", i, p.Blocks, target, tip.String(), cap.String(), need.String())

		okRelays := parallelSim(ctxCancel, rels, raws, target, p.Logf, p.OnSimResult)
		if len(okRelays)==0 { p.Logf("simulation failed on all relays; continue"); continue }

		if p.SimulateOnly {
			p.Logf("simulate-only: success on %d relay(s)", len(okRelays))
			out.Reason = fmt.Sprintf("simulate ok on %d relays", len(okRelays))
			return out, nil
		}

		parallelSend(ctxCancel, okRelays, raws, target, p.Logf)

		select {
		case s := <-status:
			if s == "included" { out.Included = true; out.Reason = "included"; cancel(); return out, nil }
			if s == "competed" { out.Included = false; out.Reason = "competing nonce included"; cancel(); return out, nil }
		case <-time.After(12 * time.Second):
		}
	}

	out.Reason = "exhausted block window"
	return out, nil
}

type flashClient struct { url string; c *fb.Client }

func parallelSim(ctx context.Context, rels []*flashClient, raws []string, target uint64, logf func(string, ...any), on func(url, raw string, ok bool, err string)) []*flashClient {
	var ok []*flashClient
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, r := range rels {
		r := r; wg.Add(1)
		go func(){
			defer wg.Done()
			res, err := r.c.SimulateBundle(ctx, raws, target)
			if err != nil { if on!=nil { on(r.url, "", false, err.Error()) }; logf("sim %s err: %v", r.url, err); return }
			if !res.OK { if on!=nil { on(r.url, res.RawJSON, false, res.Error) }; logf("sim %s fail: %s", r.url, res.Error); return }
			if on!=nil { on(r.url, res.RawJSON, true, "") }
			logf("sim %s ok", r.url)
			mu.Lock(); ok = append(ok, r); mu.Unlock()
		}()
	}
	wg.Wait()
	return ok
}

func parallelSend(ctx context.Context, rels []*flashClient, raws []string, target uint64, logf func(string, ...any)) {
	var wg sync.WaitGroup
	for _, r := range rels {
		r := r; wg.Add(1)
		go func(){
			defer wg.Done()
			res, err := r.c.SendBundle(ctx, raws, target)
			if err != nil { logf("send %s err: %v", r.url, err); return }
			if !res.OK { logf("send %s fail: %s", r.url, res.Error); return }
			logf("send %s ok id=%s", r.url, res.BundleID)
		}()
	}
	wg.Wait()
}

func monitor(ctx context.Context, ec *ethclient.Client, from common.Address, nonce uint64, status chan<- string, logf func(string, ...any)) {
	for {
		select{ case <-ctx.Done(): return; default: }
		n, err := ec.NonceAt(ctx, from, nil)
		if err == nil && n > nonce { status <- "included"; return }
		time.Sleep(3 * time.Second)
	}
}

func priv(hexKey string) (*ecdsa.PrivateKey, common.Address, error) {
	k, err := crypto.HexToECDSA(strings.TrimPrefix(hexKey, "0x"))
	if err != nil { return nil, common.Address{}, err }
	return k, crypto.PubkeyToAddress(k.PublicKey), nil
}

func sign(tx *types.Transaction, key *ecdsa.PrivateKey) *types.Transaction {
	return must(types.SignTx(tx, types.LatestSignerForChainID(tx.ChainId()), key)).(*types.Transaction)
}

func must(x any, err error) any { if err != nil { panic(err) }; return x }

func mustRlp(tx *types.Transaction) string { b, _ := tx.MarshalBinary(); return hexutil.Encode(b) }

func mulBigFloat(v *big.Int, mul float64) *big.Int {
	f := new(big.Float).SetInt(v)
	f.Mul(f, big.NewFloat(mul))
	out, _ := f.Int(nil)
	return out
}
