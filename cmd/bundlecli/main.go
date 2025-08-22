package main

import (
	"context"
	"math/big"
	"strings"

	"github.com/joho/godotenv"
	"github.com/ethereum/go-ethereum/ethclient"
	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"
)

// main keeps high-level flow; details are extracted to small helpers (see *.go in this folder).
func main() {
	_ = godotenv.Load()
	_ = godotenv.Overload(".env.local")

	ctx := context.Background()
	cfg := loadEnvConfig()

	ec, err := ethclient.Dial(cfg.RPC)
	must(err, "dial RPC")

	var chainID *big.Int
	if strings.TrimSpace(cfg.ChainIDStr) != "" {
		chainID = mustBig(cfg.ChainIDStr)
	} else {
		chainID, err = ec.ChainID(ctx); must(err, "chain id")
	}

	if strings.TrimSpace(cfg.SafePK) == "" { die("SAFE_PRIVATE_KEY is empty in env") }
	safeAddr := mustAddrFromPK(cfg.SafePK)
	safeBal, _ := ec.BalanceAt(ctx, safeAddr, nil)

	printConfig(cfg, chainID, safeAddr, safeBal)
	runInteractiveLoop(ctx, ec, chainID, cfg, safeAddr)
	_ = core.Result{} // keep import pinned
}