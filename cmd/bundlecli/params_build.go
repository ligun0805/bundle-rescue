package main

import (
	"bufio"
	"context"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"
)

// runInteractiveLoop keeps the original REPL-style flow but split into smaller steps.
func runInteractiveLoop(ctx context.Context, ec *ethclient.Client, chainID *big.Int, cfg EnvConfig, safeAddr Address) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Println("\n--- Ввод пары (compromised -> token -> amount -> to) ---")
		fromPK := readPassword("Введите приватный ключ скомпрометированного адреса: ")
		fromAddr := mustAddrFromPK(fromPK)
		fromBal, _ := ec.BalanceAt(ctx, fromAddr, nil)
		fmt.Println("  from:", fromAddr.Hex(), " | ETH balance:", formatEther(fromBal))

		token := readLine(reader, "Введите адрес ERC20 токена: ")
		if !common.IsHexAddress(token) { fmt.Println("  [!] Некорректный адрес токена"); continue }
		tokenAddr := common.HexToAddress(token)

		dec, err := fetchTokenDecimals(ctx, ec, tokenAddr)
		if err != nil { fmt.Println("  [!] Ошибка decimals:", err); continue }
		known, paused, _ := core.CheckPaused(ctx, ec, tokenAddr)
		if known && paused { fmt.Println("  [X] Токен в состоянии PAUSED — переход к следующему"); continue }
		bal, err := fetchTokenBalance(ctx, ec, tokenAddr, fromAddr)
		if err != nil { fmt.Println("  [!] Ошибка чтения баланса токена:", err); continue }
		amountWei := new(big.Int).Set(bal)
		fmt.Println("  Decimals:", dec, " | TokenBalance(from):", formatTokensFromWei(bal, dec), " -> amount=ALL")
		toAddr := safeAddr
		fmt.Println("  To:", toAddr.Hex(), "(SAFE)")

		if restr, err := core.CheckRestrictions(ctx, ec, tokenAddr, fromAddr, toAddr); err == nil {
			fmt.Println("  Restrictions:", restr.Summary())
			if restr.Blocked() { fmt.Println("  [X] Токен заблокирован правилами контракта — переход к следующему"); continue }
		} else {
			fmt.Println("  Restrictions: error:", err)
		}
		if ok, txBps, walBps, ts := tryReadBPSAndTS(ctx, ec, tokenAddr); ok {
			maxTxWei := new(big.Int).Div(new(big.Int).Mul(ts, big.NewInt(int64(txBps))), big.NewInt(10_000))
			maxWalWei := new(big.Int).Div(new(big.Int).Mul(ts, big.NewInt(int64(walBps))), big.NewInt(10_000))
			toBal, _ := fetchTokenBalance(ctx, ec, tokenAddr, toAddr)
			fmt.Printf("  Limits: maxTx=%s (%d bps), maxWallet=%s (%d bps)\n", formatTokensFromWei(maxTxWei, dec), txBps, formatTokensFromWei(maxWalWei, dec), walBps)
			if amountWei.Cmp(maxTxWei) > 0 {
				fmt.Printf("  [WARN] amount > maxTx (%s > %s)\n", formatTokensFromWei(amountWei, dec), formatTokensFromWei(maxTxWei, dec))
			}
			if new(big.Int).Add(toBal, amountWei).Cmp(maxWalWei) > 0 {
				fmt.Printf("  [WARN] toBalance+amount > maxWallet (%s + %s > %s)\n", formatTokensFromWei(toBal, dec), formatTokensFromWei(amountWei, dec), formatTokensFromWei(maxWalWei, dec))
			}
		} else {
			fmt.Println("  Limits: unknown (no maxTxBPS/maxWalletBPS getters)")
		}

		{ // preflight
			checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second); defer cancel()
			ok, reason, _ := core.PreflightTransfer(checkCtx, ec, tokenAddr, fromAddr, toAddr, amountWei)
			if !ok {
				fmt.Println("  [X] Токен не переводим на текущих параметрах:", reason)
				addMore := strings.ToLower(readLine(reader, "Перейти к добавлению новой пары? [Y/n]: "))
				if addMore == "n" || addMore == "no" || addMore == "нет" { break }
				continue
			}
			fmt.Println("  Transferable: yes")
		}

		if yes(strings.ToLower(readLine(reader, "Проверить состояние сети? [y/N]: "))) {
			printNetworkState(ctx, ec, cfg, cfg.RPC, fromAddr, toAddr, tokenAddr, amountWei, dec)
		}

		var tipMode string = "fixed"
		var tipWindow int = 100
		var tipPercentile int = 99
		tipOverride := int64(-1)

		if yes(strings.ToLower(readLine(reader, "Задать TIP_GWEI вручную для этого вывода? [y/N]: "))) {
			if s := strings.TrimSpace(readLine(reader, "Введите TIP_GWEI (целое, например 8): ")); s != "" {
				if v, err := strconv.Atoi(s); err == nil && v > 0 { tipOverride = int64(v); fmt.Printf("  => кастомный TIP_GWEI=%d gwei\n", v) } else { fmt.Println("  [!] некорректное значение — игнорирую") }
			}
		}
		if yes(strings.ToLower(readLine(reader, "Вывести токены сейчас? [y/N]: "))) {
			tipModeSel := strings.TrimSpace(strings.ToLower(readLine(reader, "Режим tip: 1) стандарт (эскалация)  2) feeHistory потолок [1/2]: ")))
			if tipModeSel == "2" || tipModeSel == "feehist" { tipMode = "feehist" }
			if tipMode == "feehist" {
				if s := strings.TrimSpace(readLine(reader, "Окно N блоков [100]: ")); s != "" { if v, err := strconv.Atoi(s); err == nil { tipWindow = v } }
				if s := strings.TrimSpace(readLine(reader, "Перцентиль (1..99) [99]: ")); s != "" { if v, err := strconv.Atoi(s); err == nil { tipPercentile = v } }
			}
			if tipOverride > 0 {
				fmt.Println("  => используем стандартный режим с фиксированным TIP_GWEI; feeHistory отключен")
				tipMode, tipWindow, tipPercentile = "fixed", 0, 0
				cfg.TipGwei, cfg.TipMul = tipOverride, 1.00
			}
			enableBribe := yes(strings.ToLower(readLine(reader, "Включить прямой перевод coinbase? [y/N]: ")))
			bribeWei := big.NewInt(0); bribeGasLimit := uint64(60000)
			if enableBribe {
				bribeStr := strings.TrimSpace(readLine(reader, "Сумма (ETH): "))
				bribeWei = parseETH(bribeStr)
				if bribeWei.Sign() <= 0 { fmt.Println("  [!] 0 — выключено"); enableBribe = false }
				if s := strings.TrimSpace(readLine(reader, "Bribe gas limit [60000]: ")); s != "" { if v, err := strconv.Atoi(s); err == nil { bribeGasLimit = uint64(v) } }
			}
			if bribeGasLimit < 53000 { fmt.Println("  [!] bribe gas limit слишком мал для contract creation; выставляю 60000"); bribeGasLimit = 60000 }

			extraHeaders := map[string]map[string]string{}
			if v := getenv("BLOXROUTE_RELAY", "https://api.blxrbdn.com"); v != "" {
				if k := getenv("BLOXROUTE_API_KEY", ""); k != "" {
					extraHeaders[v] = map[string]string{ "X-API-KEY": k, "Authorization": k }
				}
			}
			replUUID := genUUIDv4()
			params := core.Params{
				RPC: cfg.RPC, ChainID: chainID, Relays: splitCSV(cfg.RelaysCSV), AuthPrivHex: cfg.AuthPK,
				Token: tokenAddr, From: fromAddr, To: toAddr, AmountWei: amountWei,
				SafePKHex: cfg.SafePK, FromPKHex: fromPK,
				Blocks: cfg.Blocks, TipGweiBase: cfg.TipGwei, TipMul: cfg.TipMul, BaseMul: cfg.BaseMul, BufferPct: cfg.BufferPct,
				TipMode: tipMode, TipWindow: tipWindow, TipPercentile: tipPercentile,
				BribeWei: bribeWei, BribeGasLimit: bribeGasLimit, ExtraHeaders: extraHeaders,
				Builders: cfg.Builders, ReplacementUUID: replUUID, MinTimestamp: cfg.MinTs, MaxTimestamp: cfg.MaxTs,
				BeaverAllowBuilderNetRefunds: &cfg.BeaverAllow, BeaverRefundRecipientHex: cfg.BeaverRefundTo,
				Verbose: false, SimulateOnly: false, SkipIfPaused: true,
				Logf: func(f string, a ...any){ fmt.Printf(f+"\n", a...) },
				OnSimResult: func(relay, raw string, ok bool, err string){
					state := "OK"; if !ok { state = "FAIL" }
					if err != "" { err = friendlySimErr(err) }
					fmt.Printf("  [sim %s] %s err=%s\n", relay, state, err)
				},
			}
			if res, err := core.Run(ctx, ec, params); err != nil {
				fmt.Println("[ERROR run]", err)
			} else {
				fmt.Println("[RESULT]", res.Reason, "| included:", res.Included)
			}
		}
		again := strings.ToLower(readLine(reader, "Перейти к добавлению новой пары? [y/N]: "))
		if again != "y" && again != "yes" && again != "д" && again != "да" { break }
	}
}

func parseCSVInts(s string, def []int) []int {
	s = strings.TrimSpace(s)
	if s == "" { return def }
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if v, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && v > 0 && v < 100 { out = append(out, v) }
	}
	if len(out) == 0 { return def }
	return out
}
