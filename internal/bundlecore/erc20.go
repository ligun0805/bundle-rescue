package bundlecore

import (
	"encoding/hex"
	"fmt"
	"strings"

	"context"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Encode ERC-20 transfer calldata.
func EncodeERC20Transfer(to common.Address, amount *big.Int) []byte {
	selector := common.FromHex("0xa9059cbb")
	arg1 := common.LeftPadBytes(to.Bytes(), 32)
	arg2 := common.LeftPadBytes(amount.Bytes(), 32)
	return append(selector, append(arg1, arg2...)...)
}

// --- small RPC helpers (retry + backoff) ---
func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "Too Many Requests") || strings.Contains(s, "-32005")
}

// callWithRetry performs eth_call with small exponential backoff.
func callWithRetry(ctx context.Context, ec *ethclient.Client, msg ethereum.CallMsg) ([]byte, error) {
	const maxAttempts = 3
	backoff := 200 * time.Millisecond
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ret, err := ec.CallContract(ctx, msg, nil)
		if err == nil {
			return ret, nil
		}
		lastErr = err
		if attempt < maxAttempts {
			time.Sleep(backoff)
			if isRateLimitError(err) {
				backoff *= 2
			}
		}
	}
	return nil, lastErr
}

func estimateGasWithRetry(ctx context.Context, ec *ethclient.Client, msg ethereum.CallMsg) (uint64, error) {
	const maxAttempts = 3
	backoff := 200 * time.Millisecond
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		g, err := ec.EstimateGas(ctx, msg)
		if err == nil {
			return g, nil
		}
		lastErr = err
		if attempt < maxAttempts {
			time.Sleep(backoff)
			if isRateLimitError(err) {
				backoff *= 2
			}
		}
	}
	return 0, lastErr
}

func PreflightTransfer(ctx context.Context, ec *ethclient.Client, token, from, to common.Address, amount *big.Int) (bool, string, error) {
	// Build ERC-20 calldata: transfer(to, amount)
	data := EncodeERC20Transfer(to, amount)
	msg := ethereum.CallMsg{From: from, To: &token, Data: data, Value: big.NewInt(0)}

	// 1) Static call with retry to inspect return data (strict ERC-20 semantics).
	ret, callErr := callWithRetry(ctx, ec, msg)
	if callErr != nil {
		// Revert or other VM error: clearly not transferable.
		return false, revertReason(callErr), nil
	}

	// 2) Interpret return:
	//    - Some tokens return no data (pre-ERC20 behavior) => fall back to gas heuristic.
	//    - Standard tokens return ABI-encoded bool in 32 bytes; treat last byte == 1 as true.
	if len(ret) == 0 {
		if _, e := estimateGasWithRetry(ctx, ec, msg); e == nil {
			return true, "", nil
		}
		return false, "transfer would revert", nil
	}
	if ret[len(ret)-1] == 1 {
		return true, "", nil
	}
	return false, "transfer() returned false", nil
}

func revertReason(e error) string {
	s := e.Error()
	if i := strings.Index(s, "execution reverted"); i >= 0 {
		return s[i:]
	}
	return s
}

// Pause checks (various signatures in the wild).
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
		res, e := callWithRetry(ctx, ec, ethereum.CallMsg{To: &token, Data: sig})
		if e != nil || len(res) == 0 {
			continue
		}
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
	if tr.Paused || tr.TransferDisabled || tr.BlacklistedFrom || tr.BlacklistedTo {
		return true
	}
	if tr.OnlyWhitelisted {
		if tr.FromWhitelisted != nil && !*tr.FromWhitelisted {
			return true
		}
		if tr.ToWhitelisted != nil && !*tr.ToWhitelisted {
			return true
		}
	}
	return false
}

func (tr TokenRestrictions) Summary() string {
	parts := []string{}
	if tr.Paused {
		parts = append(parts, "paused")
	}
	if tr.TransferDisabled {
		parts = append(parts, "transferDisabled")
	}
	if tr.BlacklistedFrom {
		parts = append(parts, "from:blacklisted")
	}
	if tr.BlacklistedTo {
		parts = append(parts, "to:blacklisted")
	}
	if tr.OnlyWhitelisted {
		wf := "unknown"
		if tr.FromWhitelisted != nil {
			if *tr.FromWhitelisted {
				wf = "yes"
			} else {
				wf = "no"
			}
		}
		wt := "unknown"
		if tr.ToWhitelisted != nil {
			if *tr.ToWhitelisted {
				wt = "yes"
			} else {
				wt = "no"
			}
		}
		parts = append(parts, fmt.Sprintf("whitelist:on (from=%s,to=%s)", wf, wt))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func CheckRestrictions(ctx context.Context, ec *ethclient.Client, token common.Address, from, to common.Address) (TokenRestrictions, error) {
	var out TokenRestrictions

	known, paused, _ := CheckPaused(ctx, ec, token)
	if known && paused {
		out.Paused = true
		return out, nil
	}

	call := func(data []byte) (ret []byte, ok bool) {
		res, err := callWithRetry(ctx, ec, ethereum.CallMsg{To: &token, Data: data})
		if err != nil || len(res) == 0 {
			return nil, false
		}
		return res, true
	}
	boolOf := func(b []byte) bool {
		if len(b) == 0 {
			return false
		}
		return b[len(b)-1] == 1
	}

	for _, s := range transferDisabledGlobalSigsStr {
		if ret, ok := call(sel(s)); ok && boolOf(ret) {
			out.TransferDisabled = true
			return out, nil
		}
	}
	for _, s := range onlyWhitelistGlobalSigsStr {
		if ret, ok := call(sel(s)); ok && boolOf(ret) {
			out.OnlyWhitelisted = true
			break
		}
	}

	whitelisted := func(addr common.Address) *bool {
		for _, s := range whitelistAddrViewSigsStr {
			data := append(sel(s), common.LeftPadBytes(addr.Bytes(), 32)...)
			if ret, ok := call(data); ok {
				v := boolOf(ret)
				return &v
			}
		}
		return nil
	}
	if out.OnlyWhitelisted {
		out.FromWhitelisted = whitelisted(from)
		out.ToWhitelisted = whitelisted(to)
	}

	isBlacklisted := func(addr common.Address) bool {
		for _, s := range blacklistAddrViewSigsStr {
			data := append(sel(s), common.LeftPadBytes(addr.Bytes(), 32)...)
			if ret, ok := call(data); ok && boolOf(ret) {
				return true
			}
		}
		return false
	}
	out.BlacklistedFrom = isBlacklisted(from)
	out.BlacklistedTo = isBlacklisted(to)

	return out, nil
}

func EstimateTransferGas(ctx context.Context, ec *ethclient.Client, from common.Address, token common.Address, data []byte) (uint64, error) {
	msg := ethereum.CallMsg{From: from, To: &token, Value: big.NewInt(0), Data: data}
	return estimateGasWithRetry(ctx, ec, msg)
}
