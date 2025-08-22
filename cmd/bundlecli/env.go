package main

import (
	"fmt"
	"math/big"
	"os"
	"strings"
)

type EnvConfig struct {
	RPC         string
	ChainIDStr  string
	RelaysCSV   string
	AuthPK      string
	SafePK      string
	Blocks      int
	TipGwei     int64
	TipMul      float64
	BaseMul     int64
	BufferPct   int64
	Builders    []string
	MinTs       int64
	MaxTs       int64
	BeaverAllow bool
	BeaverRefundTo string
	NetBlocks   int
	NetPcts     []int
}

// loadEnvConfig reads config exactly as the old main.go did (logic preserved).
func loadEnvConfig() EnvConfig {
	rpc := getenv("RPC_URL", "https://eth.llamarpc.com")
	chainIDStr := getenv("CHAIN_ID", "")
	relays := getenv("RELAYS", "https://relay.flashbots.net")
	if v := getenv("BLOXROUTE_RELAY", "https://api.blxrbdn.com"); v != "" {
		if !strings.Contains(relays, v) { relays = relays + "," + v }
	}
	authPK := getenv("FLASHBOTS_AUTH_PK", "")
	safePK := getenv("SAFE_PRIVATE_KEY", "")
	blocks := atoi(getenv("BLOCKS", "6"), 6)
	tipGwei := atoi64(getenv("TIP_GWEI", "3"), 3)
	tipMul := atof(getenv("TIP_MUL", "1.25"), 1.25)
	baseMul := atoi64(getenv("BASEFEE_MUL", "2"), 2)
	bufferPct := atoi64(getenv("BUFFER_PCT", "5"), 5)
	builders := splitCSV(getenv("BUILDERS", ""))
	minTs := atoi64(getenv("MIN_TIMESTAMP", "0"), 0)
	maxTs := atoi64(getenv("MAX_TIMESTAMP", "0"), 0)
	beaverAllow := strings.ToLower(getenv("BEAVER_ALLOW_BUILDERNET_REFUNDS", "true")) == "true"
	beaverRefundTo := strings.TrimSpace(getenv("BEAVER_REFUND_RECIPIENT", ""))
	netBlocks := atoi(getenv("NETCHECK_BLOCKS", "100"), 100)
	netPcts := parseCSVInts(getenv("NETCHECK_PCTS", "50,95,99"), []int{50,95,99})
	return EnvConfig{
		RPC: rpc, ChainIDStr: chainIDStr, RelaysCSV: relays, AuthPK: authPK, SafePK: safePK,
		Blocks: blocks, TipGwei: tipGwei, TipMul: tipMul, BaseMul: baseMul, BufferPct: bufferPct,
		Builders: builders, MinTs: minTs, MaxTs: maxTs,
		BeaverAllow: beaverAllow, BeaverRefundTo: beaverRefundTo,
		NetBlocks: netBlocks, NetPcts: netPcts,
	}
}

func printConfig(cfg EnvConfig, chainID *big.Int, safeAddr Address, safeBal *big.Int) {
	fmt.Println("=== CONFIG (.env) ===")
	fmt.Println("RPC_URL           :", cfg.RPC)
	fmt.Println("CHAIN_ID          :", chainID.String())
	fmt.Println("RELAYS            :", cfg.RelaysCSV)
	fmt.Println("FLASHBOTS_AUTH_PK :", maskHex(cfg.AuthPK))
	fmt.Println("SAFE_PRIVATE_KEY  :", maskHex(cfg.SafePK))
	fmt.Println("  -> Safe address :", safeAddr.Hex())
	fmt.Println("  -> Safe balance :", formatEther(safeBal), "ETH")
	fmt.Println("Blocks            :", cfg.Blocks)
	fmt.Println("Tip (gwei)        :", cfg.TipGwei)
	fmt.Println("TipMul            :", cfg.TipMul)
	fmt.Println("BaseFeeMul        :", cfg.BaseMul)
	fmt.Println("BufferPct         :", cfg.BufferPct)
	fmt.Println("=====================")
}

func getenv(k, d string) string { v := strings.TrimSpace(os.Getenv(k)); if v=="" { return d }; return v }
func atoi(s string, d int) int { var n int; _,err := fmt.Sscan(strings.TrimSpace(s), &n); if err!=nil { return d }; return n }
func atoi64(s string, d int64) int64 { var n int64; _,err := fmt.Sscan(strings.TrimSpace(s), &n); if err!=nil { return d }; return n }
func atof(s string, d float64) float64 { var n float64; _,err := fmt.Sscan(strings.TrimSpace(s), &n); if err!=nil { return d }; return n }
func must(err error, msg string) { if err!=nil { die(msg+": "+err.Error()) } }
func die(msg string) { fmt.Fprintln(os.Stderr, msg); os.Exit(1) }
func mustBig(s string) *big.Int { z,newOk := new(big.Int), false; s=strings.TrimSpace(s); if strings.HasPrefix(s,"0x") { z,newOk = z.SetString(s[2:],16) } else { z,newOk = z.SetString(s,10) }; if !newOk { return big.NewInt(0) }; return z }
func splitCSV(s string) []string { arr := strings.Split(s, ","); out := make([]string,0,len(arr)); for _,x := range arr { x=strings.TrimSpace(x); if x!="" { out=append(out,x) } }; return out }
