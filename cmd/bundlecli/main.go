package main

import (
	"context"
  "flag"
	"fmt"
	"math/big"
  "net/http"
	"strings"
  "os"
  "time"

	"github.com/joho/godotenv"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/common"
  "github.com/ethereum/go-ethereum/rpc"
	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"
)

// newEthClientWithTimeout dials RPC with keep-alives and sane timeouts.
func newEthClientWithTimeout(rpcURL string) (*ethclient.Client, error) {
	transport := &http.Transport{ MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, DisableCompression: false }
	httpClient := &http.Client{ Timeout: 30 * time.Second, Transport: transport }
	rpcClient, err := rpc.DialHTTPWithClient(rpcURL, httpClient)
	if err != nil { return nil, err }
	return ethclient.NewClient(rpcClient), nil
}

// main keeps high-level flow; details are extracted to small helpers (see *.go in this folder).
func main() {
	var pairsPath string
	flag.StringVar(&pairsPath, "pairs", "", "Path to CSV for batch EIP-7702 mode (token,privateKey,from[,reason])")
	flag.Parse()	
  
  _ = godotenv.Load()
	_ = godotenv.Overload(".env.local")

	ctx := context.Background()
	cfg := loadEnv()

	ec, err := newEthClientWithTimeout(cfg.RPC)
	must(err, "dial RPC")
	// Best-effort RPC client for eth_call stateOverrides (7702 preflight)
	rc, _ := rpc.DialContext(ctx, cfg.RPC)

	var chainID *big.Int
	if strings.TrimSpace(cfg.ChainIDStr) != "" {
		chainID = mustBig(cfg.ChainIDStr)
	} else {
		chainID, err = ec.ChainID(ctx); must(err, "chain id")
	}

	if strings.TrimSpace(cfg.SafePK) == "" { die("SAFE_PRIVATE_KEY is empty in env") }
	safeAddr := mustAddrFromPK(cfg.SafePK)
    safeBal, _ := ec.BalanceAt(ctx, safeAddr, nil)

    // --- Batch mode (EIP-7702 only) BEFORE reading FROM_PK ---
    // Priority: --pairs flag > PAIRS_CSV env > interactive.
    batchPath := strings.TrimSpace(pairsPath)
    if batchPath == "" {
        batchPath = strings.TrimSpace(os.Getenv("PAIRS_CSV"))
    }
    if batchPath != "" {
        if err := runBatchPairsFromCSV(ctx, ec, cfg, chainID, safeAddr, batchPath); err != nil {
            fmt.Println("  [batch] error:", err)
        }
        return
    }

    // Resolve FROM/TOKEN from env (optional token)
    fromAddr := mustAddrFromPK(cfg.FromPK)
    var tokenAddr Address
    if strings.TrimSpace(cfg.TokenAddrHex) != "" {
        tokenAddr = common.HexToAddress(cfg.TokenAddrHex)
    }
    fromEthBal, _ := ec.BalanceAt(ctx, fromAddr, nil)
    // Best-effort ERC-20 meta
    tokDec := 18
    if (tokenAddr != Address{}) {
        if d, err := fetchTokenDecimals(ctx, ec, tokenAddr); err == nil { tokDec = d }
    }
    tokSym := ""
    var fromTokBal *big.Int
    if (tokenAddr != Address{}) {
        if b, err := fetchTokenBalance(ctx, ec, tokenAddr, fromAddr); err == nil { fromTokBal = b }
		if s, err := fetchTokenSymbol(ctx, ec, tokenAddr); err == nil { tokSym = s }
    }

    printConfig(cfg, chainID, safeAddr, safeBal, tokenAddr, fromAddr, fromTokBal, tokSym, tokDec, fromEthBal)

    fmt.Println("Проверка сети ......")
    printNetworkState(ctx, ec, cfg, cfg.RPC,
        fromAddr, safeAddr, tokenAddr,
        big.NewInt(0),
        tokDec,
    )
    fmt.Println()
    fmt.Println("Проверка токена и совместимости ....")
    if (tokenAddr != Address{}) {
		// Perform token guards & restrictions like in rescue7702, but at startup (non-interactive).
		// Amount = victim balance (best-effort).
		guardsOK, guardsWhy := true, ""
		preOK, preWhy := true, ""
		// Guards
		fmt.Println("  [*] Проверяю токен: blacklist/лимиты…")
		victimBal, _ := fetchTokenBalance(ctx, ec, tokenAddr, fromAddr)
		if ok, warn, err := inspectTokenGuards(ctx, ec, tokenAddr, fromAddr, safeAddr, victimBal); err != nil {
			guardsOK, guardsWhy = false, fmt.Sprintf("token guards error: %v", err)
		} else if !ok {
			guardsOK, guardsWhy = false, warn
		} else {
			fmt.Println("  [+] Token guards OK.")
		}
		// Restrictions (paused/whitelist/blacklist)
		if restr, err := core.CheckRestrictions(ctx, ec, tokenAddr, fromAddr, safeAddr); err == nil {
			fmt.Println("  [*] Token restrictions:", restr.Summary())
			if restr.Blocked() {
				guardsOK = false
				if guardsWhy != "" { guardsWhy += "; " }
				guardsWhy += "restricted: " + restr.Summary()
			}
		} else {
			fmt.Println("  [!] Token restrictions: error:", err)
		}
		// Preflight via core.PreflightTransfer (has retry/backoff against 429/-32005)
		// Use victim balance if known, otherwise test with 1 wei to detect pure-revert/false.
		preflightAmt := victimBal
		if preflightAmt == nil || preflightAmt.Sign() <= 0 {
			preflightAmt = big.NewInt(1)
		}
		if ok, why, err := core.PreflightTransfer(ctx, ec, tokenAddr, fromAddr, safeAddr, preflightAmt); err != nil {
			preOK, preWhy = false, fmt.Sprintf("preflight error: %v", err)
		} else if !ok {
			preOK, preWhy = false, why
		} else {
			// legacy preflight OK
			line := "  [+] Token preflight OK"
			if strings.TrimSpace(why) != "" { line += " — " + why }
			// add 7702-aware hint (route selection) without removing legacy result
			if rc != nil {
				if ok2, why2, err2 := core.PreflightTransfer7702(ctx, ec, rc, tokenAddr, fromAddr, safeAddr, preflightAmt); err2 == nil {
					if strings.TrimSpace(why2) != "" {
						line += " | 7702: " + why2
					} else if ok2 {
						line += " | 7702: ok"
					}
				} else if err2 != nil {
					line += " | 7702: error: " + err2.Error()
				}
			}
			fmt.Println(line)
		}
		// Summary
		fmt.Println("  --- Результат проверок ---")
		if guardsOK { fmt.Println("   • Guards   : OK") } else { fmt.Println("   • Guards   : FAIL —", guardsWhy) }
		if preOK    { fmt.Println("   • Preflight: OK") } else { fmt.Println("   • Preflight: FAIL —", preWhy) }
    } else {
        fmt.Println("  TOKEN_ADDRESS not set")
    }
    fmt.Println()
    fmt.Println("Результаты проверок....")


  runInteractiveLoop(ctx, ec, chainID, cfg, safeAddr)
	_ = core.Result{} // keep import pinned
}


