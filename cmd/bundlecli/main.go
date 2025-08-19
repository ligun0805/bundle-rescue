package main

import (
	"bufio"
	"context"
	"fmt"
	"math/big"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"golang.org/x/term"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"
)

func main() {
	_ = godotenv.Load()
	_ = godotenv.Overload(".env.local")

	ctx := context.Background()

	rpc := getenv("RPC_URL", "https://eth.llamarpc.com")
	chainIDStr := getenv("CHAIN_ID", "")
	relays := getenv("RELAYS", "https://relay.flashbots.net")
	authPK := getenv("FLASHBOTS_AUTH_PK", "")
	safePK := getenv("SAFE_PRIVATE_KEY", "")
	blocks := atoi(getenv("BLOCKS", "6"), 6)
	tipGwei := atoi64(getenv("TIP_GWEI", "3"), 3)
	tipMul := atof(getenv("TIP_MUL", "1.25"), 1.25)
	baseMul := atoi64(getenv("BASEFEE_MUL", "2"), 2)
	bufferPct := atoi64(getenv("BUFFER_PCT", "5"), 5)

	ec, err := ethclient.Dial(rpc)
	must(err, "dial RPC")
	var chainID *big.Int
	if chainIDStr != "" {
		chainID = mustBig(chainIDStr)
	} else {
		chainID, err = ec.ChainID(ctx); must(err, "chain id")
	}

	if strings.TrimSpace(safePK) == "" { die("SAFE_PRIVATE_KEY is empty in env") }
	safeAddr := mustAddrFromPK(safePK)
	safeBal, _ := ec.BalanceAt(ctx, safeAddr, nil)

	fmt.Println("=== CONFIG (.env) ===")
	fmt.Println("RPC_URL           :", rpc)
	fmt.Println("CHAIN_ID          :", chainID.String())
	fmt.Println("RELAYS            :", relays)
	fmt.Println("FLASHBOTS_AUTH_PK :", maskHex(authPK))
	fmt.Println("SAFE_PRIVATE_KEY  :", maskHex(safePK))
	fmt.Println("  -> Safe address :", safeAddr.Hex())
	fmt.Println("  -> Safe balance :", formatEther(safeBal), "ETH")
	fmt.Println("Blocks            :", blocks)
	fmt.Println("Tip (gwei)        :", tipGwei)
	fmt.Println("TipMul            :", tipMul)
	fmt.Println("BaseFeeMul        :", baseMul)
	fmt.Println("BufferPct         :", bufferPct)
	fmt.Println("=====================")

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
		if known && paused {
			fmt.Println("  [X] Токен в состоянии PAUSED — переход к следующему")
			continue
		}	
		bal, err := fetchTokenBalance(ctx, ec, tokenAddr, fromAddr)
		if err != nil { fmt.Println("  [!] Ошибка чтения баланса токена:", err); continue }
		fmt.Println("  Decimals:", dec, " | TokenBalance(from):", formatTokensFromWei(bal, dec))

		amountTok := readLine(reader, "Введите amount (в токенах): ")
		amountWei, err := toWeiFromTokens(amountTok, dec)
		if err != nil { fmt.Println("  [!] Ошибка amount:", err); continue }
		if bal.Cmp(amountWei) < 0 {
			fmt.Println("  [X] Баланс меньше, чем amount — переход к следующему")
			continue
		}

		to := readLine(reader, "Введите адрес получателя: ")
		if !common.IsHexAddress(to) { fmt.Println("  [!] Некорректный адрес получателя"); continue }
		toAddr := common.HexToAddress(to)
		
		restr, err := core.CheckRestrictions(ctx, ec, tokenAddr, fromAddr, toAddr)
		if err == nil {
			fmt.Println("  Restrictions:", restr.Summary())
			if restr.Blocked() {
				fmt.Println("  [X] Токен заблокирован правилами контракта — переход к следующему")
				continue
			}
		} else {
			fmt.Println("  Restrictions: error:", err)
		}
		
		if ok, txBps, walBps, ts := tryReadBPSAndTS(ctx, ec, tokenAddr); ok {
			maxTxWei := new(big.Int).Div(new(big.Int).Mul(ts, big.NewInt(int64(txBps))), big.NewInt(10_000))
			maxWalWei := new(big.Int).Div(new(big.Int).Mul(ts, big.NewInt(int64(walBps))), big.NewInt(10_000))
			toBal, _ := fetchTokenBalance(ctx, ec, tokenAddr, toAddr)
			fmt.Printf("  Limits: maxTx=%s (%d bps), maxWallet=%s (%d bps)\n",
				formatTokensFromWei(maxTxWei, dec), txBps, formatTokensFromWei(maxWalWei, dec), walBps)
			if amountWei.Cmp(maxTxWei) > 0 {
				fmt.Printf("  [WARN] amount > maxTx (%s > %s)\n", formatTokensFromWei(amountWei, dec), formatTokensFromWei(maxTxWei, dec))
			}
			if new(big.Int).Add(toBal, amountWei).Cmp(maxWalWei) > 0 {
				fmt.Printf("  [WARN] toBalance+amount > maxWallet (%s + %s > %s)\n",
					formatTokensFromWei(toBal, dec), formatTokensFromWei(amountWei, dec), formatTokensFromWei(maxWalWei, dec))
			}
		} else {
			fmt.Println("  Limits: unknown (no maxTxBPS/maxWalletBPS getters)")
		}

		{
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

		if yes(strings.ToLower(readLine(reader, "Симулировать транзакцию? [y/N]: "))) {
			params := core.Params{
				RPC: rpc, ChainID: chainID, Relays: splitCSV(relays), AuthPrivHex: authPK,
				Token: tokenAddr, From: fromAddr, To: toAddr,
				AmountWei: amountWei, SafePKHex: safePK, FromPKHex: fromPK,
				Blocks: blocks, TipGweiBase: tipGwei, TipMul: tipMul, BaseMul: baseMul, BufferPct: bufferPct,
				SimulateOnly: true, SkipIfPaused: true,
				Logf: func(f string, a ...any){ fmt.Printf(f+"\n", a...) },
				OnSimResult: func(relay, raw string, ok bool, err string){
					state := "OK"; if !ok { state = "FAIL" }
					fmt.Printf("  [sim %s] %s err=%s\n", relay, state, err)
				},
			}
			if _, err := core.Run(ctx, ec, params); err != nil {
				fmt.Println("[ERROR simulate]", err)
			}
		}

		if yes(strings.ToLower(readLine(reader, "Вывести токены сейчас? [y/N]: "))) {
			params := core.Params{
				RPC: rpc, ChainID: chainID, Relays: splitCSV(relays), AuthPrivHex: authPK,
				Token: tokenAddr, From: fromAddr, To: toAddr,
				AmountWei: amountWei, SafePKHex: safePK, FromPKHex: fromPK,
				Blocks: blocks, TipGweiBase: tipGwei, TipMul: tipMul, BaseMul: baseMul, BufferPct: bufferPct,
				SimulateOnly: false, SkipIfPaused: true,
				Logf: func(f string, a ...any){ fmt.Printf(f+"\n", a...) },
				OnSimResult: func(relay, raw string, ok bool, err string){
					state := "OK"; if !ok { state = "FAIL" }
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

func getenv(k, d string) string { v := strings.TrimSpace(os.Getenv(k)); if v=="" { return d }; return v }
func atoi(s string, d int) int { var n int; _,err := fmt.Sscan(strings.TrimSpace(s), &n); if err!=nil { return d }; return n }
func atoi64(s string, d int64) int64 { var n int64; _,err := fmt.Sscan(strings.TrimSpace(s), &n); if err!=nil { return d }; return n }
func atof(s string, d float64) float64 { var n float64; _,err := fmt.Sscan(strings.TrimSpace(s), &n); if err!=nil { return d }; return n }
func must(err error, msg string) { if err!=nil { die(msg+": "+err.Error()) } }
func die(msg string) { fmt.Fprintln(os.Stderr, msg); os.Exit(1) }
func splitCSV(s string) []string { arr := strings.Split(s, ","); out := make([]string,0,len(arr)); for _,x := range arr { x=strings.TrimSpace(x); if x!="" { out=append(out,x) } }; return out }
func mustBig(s string) *big.Int { z,newOk := new(big.Int), false; s=strings.TrimSpace(s); if strings.HasPrefix(s,"0x") { z,newOk = z.SetString(s[2:],16) } else { z,newOk = z.SetString(s,10) }; if !newOk { return big.NewInt(0) }; return z }
func maskHex(h string) string { h=strings.TrimSpace(h); if len(h)<=10 { return "***" }; return h[:6]+"…"+h[len(h)-4:] }

func mustAddrFromPK(pkHex string) common.Address {
	h := strings.TrimPrefix(strings.TrimSpace(pkHex), "0x")
	prv, err := crypto.HexToECDSA(h); must(err, "bad private key")
	return crypto.PubkeyToAddress(prv.PublicKey)
}

func formatEther(v *big.Int) string {
	if v==nil { return "0" }
	s := new(big.Rat).SetFrac(v, big.NewInt(1_000_000_000_000_000_000))
	return s.FloatString(6)
}

func truncate(s string, n int) string { if len(s)<=n { return s }; return s[:n] + "…(truncated)" }

func readLine(r *bufio.Reader, prompt string) string {
	fmt.Print(prompt)
	t, _ := r.ReadString('\n')
	return strings.TrimSpace(t)
}

func readPassword(prompt string) string {
	fmt.Print(prompt)
	b, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil { die("failed to read password: "+err.Error()) }
	return strings.TrimSpace(string(b))
}


func fetchTokenDecimals(ctx context.Context, ec *ethclient.Client, token common.Address) (int, error) {
	// function decimals() -> uint8 : 0x313ce567
	decimalsSelector := common.FromHex("0x313ce567")
	res, err := ec.CallContract(ctx, ethereum.CallMsg{
		To: &token, Data: decimalsSelector,
	}, nil)
	if err != nil { return 0, err }
	if len(res)==0 { return 18, nil }
	return int(res[len(res)-1]), nil
}

func fetchTokenBalance(ctx context.Context, ec *ethclient.Client, token, owner common.Address) (*big.Int, error) {
	data := append(common.FromHex("0x70a08231"), common.LeftPadBytes(owner.Bytes(),32)...)
	res, err := ec.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil { return nil, err }
	if len(res)==0 { return big.NewInt(0), nil }
	return new(big.Int).SetBytes(res), nil
}


func toWeiFromTokens(amount string, decimals int) (*big.Int, error) {
	amount = strings.TrimSpace(amount)
	if amount == "" { return nil, fmt.Errorf("empty amount") }
	if decimals < 0 { decimals = 18 }
	parts := strings.SplitN(amount, ".", 2)
	intPart := parts[0]; fracPart := ""
	if len(parts) == 2 { fracPart = parts[1] }
	if len(fracPart) > decimals { return nil, fmt.Errorf("too many fractional digits for %d decimals", decimals) }
	fracPart = fracPart + strings.Repeat("0", decimals-len(fracPart))
	clean := strings.TrimLeft(intPart+fracPart, "0")
	if clean == "" { return big.NewInt(0), nil }
	v, ok := new(big.Int).SetString(clean, 10)
	if !ok { return nil, fmt.Errorf("bad amount") }
	return v, nil
}

func formatTokensFromWei(v *big.Int, decimals int) string {
	if v==nil { return "0" }
	if decimals<=0 { return v.String() }
	s := new(big.Int).Abs(v).String()
	neg := v.Sign() < 0
	if len(s) <= decimals {
		frac := strings.Repeat("0", decimals-len(s)) + s
		out := "0." + strings.TrimRight(frac, "0")
		if out == "0." { out = "0" }
		if neg { return "-" + out }
		return out
	}
	intPart := s[:len(s)-decimals]
	frac := strings.TrimRight(s[len(s)-decimals:], "0")
	out := intPart
	if frac != "" { out = intPart + "." + frac }
	if neg { return "-" + out }
	return out
}

var _ = types.LatestSignerForChainID

func yes(s string) bool { return s=="y" || s=="yes" || s=="д" || s=="да" }

func tryReadBPSAndTS(ctx context.Context, ec *ethclient.Client, token common.Address) (ok bool, maxTxBps, maxWalletBps uint64, totalSupply *big.Int) {
	readUint := func(sig string) (*big.Int, error) {
		sel := crypto.Keccak256([]byte(sig))[:4]
		res, err := ec.CallContract(ctx, ethereum.CallMsg{ To:&token, Data: sel }, nil)
		if err != nil || len(res) < 32 { return nil, err }
		return new(big.Int).SetBytes(res[len(res)-32:]), nil
	}
	ts, errTS := readUint("totalSupply()")
	mt, errTx := readUint("maxTxBPS()")
	mw, errW  := readUint("maxWalletBPS()")
	if errTS==nil && ts!=nil && errTx==nil && mt!=nil && errW==nil && mw!=nil {
		return true, mt.Uint64(), mw.Uint64(), ts
	}
	return false, 0, 0, nil
}
