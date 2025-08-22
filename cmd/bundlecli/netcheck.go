package main

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/ethclient"
	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"
)

// printNetworkState reproduces the exact prints from the original block.
func printNetworkState(ctx context.Context, ec *ethclient.Client, cfg EnvConfig, rpc string, fromAddr, toAddr Address, tokenAddr Address, amountWei *big.Int, dec int) {
	h, _ := ec.HeaderByNumber(ctx, nil)
	var baseFee *big.Int = big.NewInt(0)
	if h != nil && h.BaseFee != nil { baseFee = new(big.Int).Set(h.BaseFee) }
	fmt.Printf("[net] baseFee(now): %s gwei\n", formatGwei(baseFee))

	stats, err := core.FeeHistoryStats(ctx, rpc, cfg.NetBlocks, cfg.NetPcts)
	if err != nil {
		fmt.Println("[net] feeHistory error:", err)
	} else {
		fmt.Printf("[net] reward stats last %d blocks:\n", cfg.NetBlocks)
		for _, p := range cfg.NetPcts {
			st := stats[p]
			fmt.Printf("  p%-2d min/avg/max: %s / %s / %s gwei\n", p, formatGwei(st.Min), formatGwei(st.Avg), formatGwei(st.Max))
		}
	}
	calldata := core.EncodeERC20Transfer(toAddr, new(big.Int).Set(amountWei))
	gasTransfer := uint64(90000)
	if est, err := core.EstimateTransferGas(ctx, ec, fromAddr, tokenAddr, calldata); err == nil && est > 0 {
		gasTransfer = est
	}
	fixedTipWei := new(big.Int).Mul(big.NewInt(cfg.TipGwei), big.NewInt(1_000_000_000))
	maxFeeFixed := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(cfg.BaseMul)), fixedTipWei)
	maxTip := big.NewInt(0)
	if len(cfg.NetPcts) > 0 {
		if st, ok := stats[cfg.NetPcts[len(cfg.NetPcts)-1]]; ok && st.Max != nil { maxTip = st.Max }
		for _, p := range cfg.NetPcts {
			if st, ok := stats[p]; ok && st.Max != nil && st.Max.Cmp(maxTip) > 0 { maxTip = st.Max }
		}
	}
	maxFeePeak := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(cfg.BaseMul)), maxTip)
	gCostFixed := new(big.Int).Mul(new(big.Int).SetUint64(gasTransfer), maxFeeFixed)
	gCostPeak  := new(big.Int).Mul(new(big.Int).SetUint64(gasTransfer), maxFeePeak)
	fmt.Printf("[net] gas(transfer≈%d) cost: fixed=%s ETH, peak=%s ETH\n", gasTransfer, formatEther(gCostFixed), formatEther(gCostPeak))
	if bribes, err := core.ScanCoinbaseBribes(ctx, ec, cfg.NetBlocks); err == nil {
		s := core.SummarizeBribes(bribes)
		fmt.Printf("[net] coinbase bribes in last %d blocks: count=%d, sum=%s ETH, max=%s ETH\n", cfg.NetBlocks, s.Count, formatEther(s.Sum), formatEther(s.Max))
		if s.Count > 0 {
			fmt.Printf("      quantiles: p50=%s ETH, p95=%s ETH, p99=%s ETH\n", formatEther(s.P50), formatEther(s.P95), formatEther(s.P99))
		}
	} else {
		fmt.Println("[net] bribe scan error:", err)
	}
	_ = dec // reserved for future local prints (kept to match original signature)
}
