package main

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/joho/godotenv"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/common"
	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"
)

// main keeps high-level flow; details are extracted to small helpers (see *.go in this folder).
func main() {
	_ = godotenv.Load()
	_ = godotenv.Overload(".env.local")

	ctx := context.Background()
	cfg := loadEnv()

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
		// Preflight via eth_call
		if ok, why, err := preflightERC20Transfer(ctx, ec, tokenAddr, fromAddr, safeAddr); err != nil {
			preOK, preWhy = false, fmt.Sprintf("preflight error: %v", err)
		} else if !ok {
			preOK, preWhy = false, why
		} else {
			if strings.TrimSpace(why) != "" {
				fmt.Println("  [+] Token preflight OK —", why)
			} else {
				fmt.Println("  [+] Token preflight OK.")
			}
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


