package bundlecore

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/lmittmann/flashbots"
	w3 "github.com/lmittmann/w3"
)

// Run builds bundle (optional bribe + prefund + cancel + transfer) and races relays for inclusion.
func Run(ctx context.Context, ec *ethclient.Client, p Params) (Result, error) {
	if p.AmountWei == nil || p.AmountWei.Sign() <= 0 {
		return Result{}, errors.New("AmountWei must be > 0")
	}
	if p.ChainID == nil {
		chainID, err := ec.ChainID(ctx)
		if err != nil {
			return Result{}, err
		}
		p.ChainID = chainID
	}

	safePrv, err := hexToECDSAPriv(p.SafePKHex)
	if err != nil {
		return Result{}, err
	}
	fromPrv, err := hexToECDSAPriv(p.FromPKHex)
	if err != nil {
		return Result{}, err
	}
	authPrv, err := hexToECDSAPriv(p.AuthPrivHex)
	if err != nil {
		return Result{}, err
	}
	safeAddr := gethcrypto.PubkeyToAddress(safePrv.PublicKey)

	if p.SkipIfPaused {
		if known, paused, _ := CheckPaused(ctx, ec, p.Token); known && paused {
			p.logf("[pre-check] token is paused => skip")
			return Result{Included: false, Reason: "token paused"}, nil
		}
	}

	classic, matchmakers := classifyRelays(p.Relays, func(u string) *w3.Client { return flashbots.MustDial(u, authPrv) })
	if len(classic) == 0 && len(matchmakers) == 0 {
		return Result{}, errors.New("no relays or matchmakers configured")
	}
	if p.Blocks <= 0 {
		p.Blocks = 6
	}
	if p.TipGweiBase <= 0 {
		p.TipGweiBase = 3
	}
	if p.TipMul <= 0 {
		p.TipMul = 1.2
	}
	if p.BaseMul <= 0 {
		p.BaseMul = 2
	}
	if p.BufferPct < 0 {
		p.BufferPct = 0
	}
	if restr, err := CheckRestrictions(ctx, ec, p.Token, p.From, p.To); err == nil && restr.Blocked() {
		p.logf("[pre-check] token restricted => %s", restr.Summary())
		return Result{Included: false, Reason: "token restricted: " + restr.Summary()}, nil
	}

	startFromNonce, err := ec.PendingNonceAt(ctx, p.From)
	if err != nil {
		return Result{}, err
	}

	for attempt := 0; attempt < p.Blocks; attempt++ {
		var baseFee *big.Int
		var headNum *big.Int
		if bf, err := nextBaseFeeViaFeeHistory(ctx, p.RPC); err == nil {
			baseFee = bf
			if h, _ := ec.HeaderByNumber(ctx, nil); h != nil && h.Number != nil {
				headNum = new(big.Int).Set(h.Number)
			} else {
				headNum = big.NewInt(0)
			}
		} else {
			var err2 error
			baseFee, headNum, err2 = latestBaseFee(ctx, ec)
			if err2 != nil {
				return Result{}, err2
			}
		}
		targetBlock := new(big.Int).Add(headNum, big.NewInt(1+int64(attempt)))

		latestNonce, _ := ec.NonceAt(ctx, p.From, nil)
		pendingNonce, _ := ec.PendingNonceAt(ctx, p.From)
		replaceMode := pendingNonce > latestNonce
		fromNonce := latestNonce
		if !replaceMode {
			fromNonce = pendingNonce
		}
		if pendingNonce > fromNonce && !replaceMode {
			p.logf("[abort] competing nonce detected (start=%d now=%d)", fromNonce, pendingNonce)
			return Result{Included: false, Reason: "competing nonce"}, nil
		}

		var tip *big.Int
		if strings.ToLower(p.TipMode) == "feehist" {
			t, err := TipFromFeeHistory(ctx, p.RPC, p.TipWindow, p.TipPercentile)
			if err == nil && t != nil && t.Sign() > 0 {
				if p.TipMul > 0 && p.TipMul != 1 {
					mult := math.Pow(p.TipMul, float64(attempt))
					fv := new(big.Float).Mul(new(big.Float).SetInt(t), big.NewFloat(mult))
					fv.Int(t)
				}
				tip = t
			} else {
				// fallback — old fixed logic with escalation
				suggest := suggestPriorityViaRPC(ctx, p.RPC)
				baseTipGwei := float64(p.TipGweiBase)
				if suggest != nil {
					g := new(big.Int).Div(suggest, big.NewInt(1_000_000_000)).Int64()
					if float64(g) > baseTipGwei {
						baseTipGwei = float64(g)
					}
				}
				tipGweiScaled := int64(math.Round(baseTipGwei * math.Pow(p.TipMul, float64(attempt))))
				if tipGweiScaled < 1 {
					tipGweiScaled = p.TipGweiBase
				}
				tip = gweiToWei(tipGweiScaled)
			}
		} else {
			// old fixed logic with escalation
			suggest := suggestPriorityViaRPC(ctx, p.RPC)
			baseTipGwei := float64(p.TipGweiBase)
			if suggest != nil {
				g := new(big.Int).Div(suggest, big.NewInt(1_000_000_000)).Int64()
				if float64(g) > baseTipGwei {
					baseTipGwei = float64(g)
				}
			}
			tipGweiScaled := int64(math.Round(baseTipGwei * math.Pow(p.TipMul, float64(attempt))))
			if tipGweiScaled < 1 {
				tipGweiScaled = p.TipGweiBase
			}
			tip = gweiToWei(tipGweiScaled)
		}
		maxFee := addBig(mulBig(baseFee, p.BaseMul), tip)

		// SAFE runtime values
		safeNonce, _ := ec.PendingNonceAt(ctx, safeAddr)

		// Clamp amount to current token balance(from)
		sel := gethcrypto.Keccak256([]byte("balanceOf(address)"))[:4]
		data := append(sel, common.LeftPadBytes(p.From.Bytes(), 32)...)
		callCtx, cancelCall := context.WithTimeout(ctx, 10*time.Second)
		defer cancelCall()
		if balBytes, err := ec.CallContract(callCtx, ethereum.CallMsg{To: &p.Token, Data: data}, nil); err == nil && len(balBytes) >= 32 {
			bal := new(big.Int).SetBytes(balBytes[len(balBytes)-32:])
			if bal.Cmp(p.AmountWei) < 0 {
				p.logf("[warn] amount > balance: clamp %s -> %s", p.AmountWei.String(), bal.String())
				p.AmountWei = bal
			}
		}

		calldata := EncodeERC20Transfer(p.To, new(big.Int).Set(p.AmountWei))
		gasTransfer := uint64(90_000)
		if est, err := ec.EstimateGas(ctx, ethereum.CallMsg{From: p.From, To: &p.Token, Data: calldata}); err == nil && est > 0 {
			gasTransfer = est
		} else {
			p.logf("[warn] estimateGas for transfer failed (%v) — fallback gas=%d", err, gasTransfer)
		}
		cancelGas := uint64(0)
		if replaceMode {
			cancelGas = 21_000
		}

		prefundWei := new(big.Int).Mul(new(big.Int).SetUint64(gasTransfer+cancelGas), maxFee)
		prefundWei = new(big.Int).Div(new(big.Int).Mul(prefundWei, big.NewInt(110)), big.NewInt(100))

		bribeWei := big.NewInt(0)
		bribeGas := uint64(0)
		if p.BribeWei != nil && p.BribeWei.Sign() > 0 {
			bribeWei = new(big.Int).Set(p.BribeWei)
			if p.BribeGasLimit == 0 {
				bribeGas = 60_000
			} else {
				bribeGas = p.BribeGasLimit
			}
		}
		safeFeeWei := new(big.Int).Mul(new(big.Int).SetUint64(21_000+bribeGas), maxFee)
		needTotal := new(big.Int).Add(new(big.Int).Add(safeFeeWei, prefundWei), bribeWei)
		safeBal, _ := ec.BalanceAt(ctx, safeAddr, nil)
		if safeBal.Cmp(needTotal) < 0 {
			p.logf("[abort] SAFE balance insufficient for fee+prefund at attempt %d/%d: need >= %s ETH, have %s ETH",
				attempt+1, p.Blocks, fmtETH(needTotal), fmtETH(safeBal))
			return Result{Included: false, Reason: "insufficient SAFE balance for fee+prefund"}, nil
		}

		// 0) optional bribe tx (contract creation with {0x41,0xff})
		var signedBribe *types.Transaction
		if p.BribeWei != nil && p.BribeWei.Sign() > 0 {
			gasBribe := uint64(60_000)
			if p.BribeGasLimit > 0 {
				gasBribe = p.BribeGasLimit
			}
			bribeInit := []byte{0x41, 0xff}
			tx0 := buildDynamicTx(p.ChainID, safeNonce, nil, new(big.Int).Set(p.BribeWei), gasBribe, tip, maxFee, bribeInit)
			sb, err := signTx(tx0, p.ChainID, safePrv)
			if err != nil {
				return Result{}, err
			}
			signedBribe = sb
			safeNonce++
		}

		// 1) SAFE funds "from" for maxFee * gas (transfer + optional cancel)
		to1 := p.From
		tx1 := buildDynamicTx(p.ChainID, safeNonce, &to1, prefundWei, 21_000, tip, maxFee, nil)
		signed1, err := signTx(tx1, p.ChainID, safePrv)
		if err != nil {
			return Result{}, err
		}

		// 2) main transfer
		to2 := p.Token
		nonce2 := fromNonce
		if replaceMode {
			nonce2 = fromNonce + 1
		}
		tx2 := buildDynamicTx(p.ChainID, nonce2, &to2, big.NewInt(0), gasTransfer, tip, maxFee, calldata)
		signed2, err := signTx(tx2, p.ChainID, fromPrv)
		if err != nil {
			return Result{}, err
		}

		// optional cancel (nonce=fromNonce) if replace mode
		var signedCancel *types.Transaction
		if replaceMode {
			toSelf := p.From
			cancelTx := buildDynamicTx(p.ChainID, fromNonce, &toSelf, big.NewInt(0), 21_000, tip, maxFee, nil)
			sc, err := signTx(cancelTx, p.ChainID, fromPrv)
			if err != nil {
				return Result{}, err
			}
			signedCancel = sc
		}

		signedList := make([]*types.Transaction, 0, 4)
		if signedBribe != nil {
			signedList = append(signedList, signedBribe)
		}
		signedList = append(signedList, signed1)
		if replaceMode {
			signedList = append(signedList, signedCancel)
		}
		signedList = append(signedList, signed2)

		p.logf("[gas] transfer gas=%d, cancel=%d, maxFee=%s gwei (~%s ETH/gas)", gasTransfer, cancelGas, fmtGwei(maxFee), fmtETH(maxFee))
		p.logf("[gas] SAFE fee >= %s ETH; prefund=%s ETH (need total=%s ETH)", fmtETH(safeFeeWei), fmtETH(prefundWei), fmtETH(needTotal))
		p.logf("[attempt %d/%d] block=%s gas=%d(+%d) tip=%s gwei (~%s ETH/gas) feeCap=%s gwei (~%s ETH/gas) prefund=%s ETH nonce(safe=%d, from=%d)%s",
			attempt+1, p.Blocks, targetBlock.String(),
			gasTransfer, cancelGas, fmtGwei(tip), fmtETH(tip), fmtGwei(maxFee), fmtETH(maxFee), fmtETH(prefundWei),
			safeNonce, fromNonce, map[bool]string{true: " (+replace)", false: ""}[replaceMode],
		)
		if p.Verbose {
			p.logf("  tx1(fund safe->from): %s", txAsHex(signed1))
			if replaceMode {
				p.logf("  tx2(cancel from->from): %s", txAsHex(signedCancel))
			}
			p.logf("  tx%v(transfer): %s", map[bool]int{true: 3, false: 2}[replaceMode], txAsHex(signed2))
		}

		// prepare hex list
		txHexes := make([]string, 0, len(signedList))
		for _, t := range signedList {
			txHexes = append(txHexes, txAsHex(t))
		}

		// === PREFLIGHT SIMULATION (always log) ===
		{
			var simOK atomic.Bool
			var wgSim sync.WaitGroup
			// classic
			for _, rc := range classic {
				rc := rc
				wgSim.Add(1)
				go func() {
					defer wgSim.Done()
					var resp *flashbots.CallBundleResponse
					err2 := rc.C.Call(
						flashbots.CallBundle(&flashbots.CallBundleRequest{
							Transactions: signedList,
							BlockNumber:  new(big.Int).Set(targetBlock),
						}).Returns(&resp),
					)
					ok := (err2 == nil)
					raw := ""
					errStr := ""
					if resp != nil {
						b, _ := json.Marshal(resp)
						raw = string(b)
						for _, r := range resp.Results {
							if r.Error != nil || len(r.Revert) > 0 {
								ok = false
								if r.Error != nil {
									errStr = r.Error.Error()
								} else {
									errStr = r.Revert
								}
								break
							}
						}
					}
					if !ok && err2 != nil {
						errStr = err2.Error()
					}
					if p.OnSimResult != nil {
						p.OnSimResult(rc.URL, raw, ok, errStr)
					}
					if ok {
						simOK.Store(true)
					}
				}()
			}
			// matchmakers
			for _, u := range matchmakers {
				u := u
				wgSim.Add(1)
				go func() {
					defer wgSim.Done()
					raw, ok, err := simulateMevBundle(ctx, u, p.headerFor(u), authPrv, txHexes, targetBlock)
					if p.OnSimResult != nil {
						if ok {
							p.OnSimResult(u, raw, err == nil, "")
						} else {
							p.OnSimResult(u, "", false, "simulation not supported on matchmaker")
						}
					}
					if ok && err == nil {
						simOK.Store(true)
					}
				}()
			}
			wgSim.Wait()
		}

		if p.SimulateOnly {
			var simOK atomic.Bool
			var wgSim sync.WaitGroup
			for _, rc := range classic {
				rc := rc
				wgSim.Add(1)
				go func() {
					defer wgSim.Done()
					var resp *flashbots.CallBundleResponse
					err2 := rc.C.Call(
						flashbots.CallBundle(&flashbots.CallBundleRequest{
							Transactions: signedList,
							BlockNumber:  new(big.Int).Set(targetBlock),
						}).Returns(&resp),
					)
					ok := (err2 == nil)
					raw := ""
					errStr := ""
					if resp != nil {
						b, _ := json.Marshal(resp)
						raw = string(b)
						for _, r := range resp.Results {
							if r.Error != nil || len(r.Revert) > 0 {
								ok = false
								if r.Error != nil {
									errStr = r.Error.Error()
								} else {
									errStr = r.Revert
								}
								break
							}
						}
					}
					if !ok && err2 != nil {
						errStr = err2.Error()
					}
					if p.OnSimResult != nil {
						p.OnSimResult(rc.URL, raw, ok, errStr)
					}
					if ok {
						simOK.Store(true)
					}
				}()
			}
			for _, u := range matchmakers {
				if p.OnSimResult != nil {
					if raw, ok, err := simulateMevBundle(ctx, u, p.headerFor(u), authPrv, txHexes, targetBlock); ok {
						p.OnSimResult(u, raw, err == nil, "")
					} else {
						p.OnSimResult(u, "", false, "simulation not supported on matchmaker")
					}
				}
			}
			wgSim.Wait()

			if !simOK.Load() {
				p.logf("[attempt %d/%d] block=%s gas=%d(+%d) tip=%s gwei (~%s ETH/gas) feeCap=%s gwei (~%s ETH/gas) prefund=%s ETH nonce(safe=%d, from=%d)%s",
					attempt+1, p.Blocks, targetBlock.String(),
					gasTransfer, cancelGas, fmtGwei(tip), fmtETH(tip), fmtGwei(maxFee), fmtETH(maxFee), fmtETH(prefundWei),
					safeNonce, fromNonce, map[bool]string{true: " (+replace)", false: ""}[replaceMode])
				curFromNonce2, _ := ec.NonceAt(ctx, p.From, nil)
				if curFromNonce2 > startFromNonce {
					return Result{Included: false, Reason: "competing nonce"}, nil
				}
				continue
			}
			return Result{Included: false, Reason: "simulate only"}, nil
		}

		// === SEND TO RELAYS ===
		var wgSend sync.WaitGroup
		for _, rc := range classic {
			rc := rc
			wgSend.Add(1)
			go func() {
				defer wgSend.Done()
				var bundleHash common.Hash
				err3 := rc.C.Call(
					flashbots.SendBundle(&flashbots.SendBundleRequest{
						Transactions: signedList,
						BlockNumber:  new(big.Int).Set(targetBlock),
					}).Returns(&bundleHash),
				)
				if err3 != nil {
					p.logf("[send %s] err: %v", rc.URL, err3)
					return
				}
				p.logf("[send %s] bundle submitted: %s", rc.URL, bundleHash.Hex())
			}()
		}
		for _, u := range matchmakers {
			u := u
			wgSend.Add(1)
			go func() {
				defer wgSend.Done()
				res, err3 := sendMevBundle(ctx, &p, u, p.headerFor(u), authPrv, txHexes, targetBlock)
				if err3 != nil {
					p.logf("[mev_sendBundle %s] err: %v", u, err3)
					return
				}
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
		if incl {
			return Result{Included: true, Reason: reason}, nil
		}
		if reason == "competing nonce" {
			return Result{Included: false, Reason: reason}, nil
		}
	}

	return Result{Included: false, Reason: "exhausted attempts"}, nil
}

// waitInclusionOrCompete waits for target block and checks inclusion/nonce race.
func waitInclusionOrCompete(ctx context.Context, ec *ethclient.Client, from common.Address, startNonce uint64, ourTx2 common.Hash, targetBlock *big.Int) (bool, string, error) {
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
