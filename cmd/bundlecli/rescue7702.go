package main

import (
	"bufio"
	"context"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	eip7702 "github.com/ligun0805/bundle-rescue/internal/eip7702"
	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"
)

// runRescue7702 collects minimal inputs and sends a single sponsored EIP-7702 sweep ERC20 tx.
func runRescue7702(ctx context.Context, ec *ethclient.Client, chainID *big.Int, cfg EnvConfig, safeAddr Address, compromisedPrivHex string, compromisedAddr Address) error {
	reader := bufio.NewReader(os.Stdin)

    // 1) Tokens list (CSV) — use TOKEN_ADDRESS from .env if present
    var tokenAddrs []common.Address
    if strings.TrimSpace(cfg.TokenAddrHex) != "" {
        if !common.IsHexAddress(cfg.TokenAddrHex) {
            return fmt.Errorf("bad TOKEN_ADDRESS in .env")
        }
        tokenAddrs = []common.Address{ common.HexToAddress(cfg.TokenAddrHex) }
    } else {
        tokensCSV := readLine(reader, "Введите адреса токенов: ")
        var err error
        tokenAddrs, err = parseCSVAddresses(tokensCSV)
        if err != nil || len(tokenAddrs) == 0 {
            return fmt.Errorf("empty/invalid token list")
        }
    }
	// Show balances (best-effort)
	if strings.TrimSpace(cfg.TokenAddrHex) == "" {
		for _, t := range tokenAddrs {
			dec, _ := fetchTokenDecimals(ctx, ec, t)
			bal, _ := fetchTokenBalance(ctx, ec, t, compromisedAddr)
			fmt.Println("  ", t.Hex(), "dec:", dec, "balance:", formatTokensFromWei(bal, dec))
		}
	}

	// 2) Recipient (always SAFE when using env-driven flow; keep interactive only when TOKEN_ADDRESS not set)
	recipient := safeAddr
	if strings.TrimSpace(cfg.TokenAddrHex) == "" {
		// fallback interactive mode (legacy)
		toHex := readLine(reader, "Куда вывести токены? [ENTER = SAFE]: ")
		if strings.TrimSpace(toHex) != "" {
			if !common.IsHexAddress(toHex) { return fmt.Errorf("bad recipient") }
			recipient = common.HexToAddress(toHex)
		}
	}

	// 3) Delegate (always from env; do not ask)
	if strings.TrimSpace(cfg.DelegateHex) == "" || !common.IsHexAddress(cfg.DelegateHex) {
		return fmt.Errorf("bad DELEGATE_ADDRESS in .env")
	}
	delegate := common.HexToAddress(cfg.DelegateHex)
	
    // 3.1) Token guard checks (single-token flow): bots/limits
    guardsOK, guardsWhy := true, ""
    if len(tokenAddrs) == 1 && strings.TrimSpace(cfg.TokenAddrHex) == "" {
        fmt.Println("  [*] Проверяю токен: blacklist/лимиты…")
        dec, _ := fetchTokenDecimals(ctx, ec, tokenAddrs[0])
        balVictim, _ := fetchTokenBalance(ctx, ec, tokenAddrs[0], compromisedAddr)
        ok, warn, err := inspectTokenGuards(ctx, ec, tokenAddrs[0], compromisedAddr, recipient, balVictim)
        if err != nil {
            guardsOK, guardsWhy = false, fmt.Sprintf("token guards error: %v", err)
        } else if !ok {
            guardsOK, guardsWhy = false, fmt.Sprintf("%s (balance=%s, dec=%d)", warn, formatTokensFromWei(balVictim, dec), dec)
        } else {
            fmt.Println("  [+] Token guards OK.")
        }
		
        // 3.1.1) Global restrictions (paused/whitelist/blacklist) using bundlecore
        if restr, err := core.CheckRestrictions(ctx, ec, tokenAddrs[0], compromisedAddr, recipient); err == nil {
            fmt.Println("  [*] Token restrictions:", restr.Summary())
            if restr.Blocked() {
                guardsOK = false
                if guardsWhy != "" {
                    guardsWhy = guardsWhy + "; "
                }
                guardsWhy = guardsWhy + "restricted: " + restr.Summary()
            }
        } else {
            fmt.Println("  [!] Token restrictions: error:", err)
        }		
    }
	
	
	
    // 3.2) Token preflight: single-token case — check contract code and simulate transfer via eth_call.
    preflightOK, preflightWhy := true, ""
    if len(tokenAddrs) == 1 && strings.TrimSpace(cfg.TokenAddrHex) == "" {
        fmt.Println("  [*] Предпроверка токена через eth_call…")
        if ok, why, err := preflightERC20Transfer(ctx, ec, tokenAddrs[0], compromisedAddr, recipient); err != nil {
            preflightOK, preflightWhy = false, fmt.Sprintf("token preflight error: %v", err)
        } else if !ok {
            preflightOK, preflightWhy = false, fmt.Sprintf("token preflight FAIL: %s", why)
        } else {
            fmt.Println("  [+] Token preflight OK.")
        }
    }

    // 3.2.1) Print checks summary and ask whether to continue if something failed
    if len(tokenAddrs) == 1 && strings.TrimSpace(cfg.TokenAddrHex) == "" {
        fmt.Println("  --- Результат проверок ---")
        if guardsOK {
            fmt.Println("   • Guards   : OK")
        } else {
            fmt.Println("   • Guards   : FAIL —", guardsWhy)
        }
        if preflightOK {
            fmt.Println("   • Preflight: OK")
        } else {
            fmt.Println("   • Preflight: FAIL —", preflightWhy)
        }
        if !(guardsOK && preflightOK) {
            ans := strings.ToLower(readLine(reader, "Проверки не пройдены. Продолжить на свой риск и выбрать маршрут? [y/N]: "))
            if ans != "y" && ans != "yes" && ans != "д" && ans != "да" {
                return fmt.Errorf("aborted due to failed checks")
            }
        }
    }
	
	
	// 3.3) Route selection menu (wire classic modes here to avoid jumping back to another REPL)
    fmt.Println("  --- Готово к выбору сценария ---")
    fmt.Println("  Доступные варианты вывода:")
	fmt.Println("   [1] Стандартный бандл")
	fmt.Println("   [2] Бандл с feeHistory + coinbase bribe")
	fmt.Println("   [3] EIP-7702 бандл (sponsored)")
	route := readLine(reader, "Выберите режим [1/2/3, ENTER=3]: ")
	route = strings.TrimSpace(route)
	if route == "" { route = "3" }
	switch route {
	case "3":
		// EIP-7702: require non-zero token balance on FROM, else fail explicitly
		allZero := true
		for _, t := range tokenAddrs {
			if bal, err := fetchTokenBalance(ctx, ec, t, compromisedAddr); err == nil && bal != nil && bal.Sign() > 0 {
				allZero = false
				break
			}
		}
		if allZero {
			return fmt.Errorf("token balance is zero")
		}
		// continue with EIP-7702 flow below
	case "1", "2":
		// Execute classic bundle inline (no code duplication, reuse core.Params like in params_build.go)
		if len(tokenAddrs) != 1 {
			return fmt.Errorf("для классического режима ожидается один токен")
		}
		if err := runClassicBundleFromRescueMenu(ctx, ec, chainID, cfg, compromisedPrivHex, compromisedAddr, tokenAddrs[0], recipient, route); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("неизвестный режим: %s", route)
	}
		


	// 4) Auth nonce and count
	// For sponsored 7702 the authorization nonce equals the authority's current tx.nonce (latest).
	authNonceDefault, _ := ec.NonceAt(ctx, compromisedAddr, nil)
	fmt.Printf("  Suggested authNonce (latest): 0x%x (%d)\n", authNonceDefault, authNonceDefault)
	authNonceStr := readLine(reader, fmt.Sprintf("Текущий authNonce [ENTER=%d, поддерживает 0x..]: ", authNonceDefault))
	firstAuthNonce := authNonceDefault
	if s := strings.TrimSpace(authNonceStr); s != "" {
		v, err := parseUint64Flexible(s)
		if err != nil { return fmt.Errorf("bad authNonce: %w", err) }
		firstAuthNonce = v
	}
	authCountStr := readLine(reader, "Сколько sequential authorizations положить? [ENTER=3]: ")
	authCount := 3
	if strings.TrimSpace(authCountStr) != "" {
		if _, err := fmt.Sscan(strings.TrimSpace(authCountStr), &authCount); err != nil || authCount <= 0 || authCount > 8 {
			return fmt.Errorf("bad AuthCount")
		}
	}

	// 5) Sponsor (SAFE) keys/addr
	sponsorPriv, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.SafePK, "0x"))
	if err != nil { return fmt.Errorf("bad SAFE_PRIVATE_KEY: %w", err) }
	sponsorAddr := crypto.PubkeyToAddress(sponsorPriv.PublicKey)

	// 6) Fees
	tipWei := new(big.Int).Mul(big.NewInt(cfg.TipGwei), big.NewInt(1_000_000_000)) // gwei->wei

	// 7) Extra headers for relays (BLXR)
	extraHeaders := map[string]map[string]string{}
	if v := getenv("BLOXROUTE_RELAY", ""); v != "" {
		// For bloXroute Cloud API you must pass computed Authorization header.
		// We read it as-is from env to avoid adding heavy auth code here.
		if auth := getenv("BLOXROUTE_AUTH_HEADER", ""); auth != "" {
			extraHeaders[v] = map[string]string{"Authorization": auth}
		}
	}

	// 8) Execute
	req := eip7702.RescueRequest{
		ChainID:          chainID,
		AuthorityPrivKey: eip7702.MustLoadKey(compromisedPrivHex),
		AuthorityAddress: compromisedAddr,
		SponsorPrivKey:   sponsorPriv,
		SponsorAddress:   sponsorAddr,
		DelegateContract: delegate,
		Recipient:        recipient,
		TokenList:        tokenAddrs,
		FirstAuthNonce:   firstAuthNonce,
		AuthCount:        authCount,
		TipWei:           tipWei,
		RelayURLs:        splitCSV(cfg.RelaysCSV),
		ExtraHeaders:     extraHeaders,
		AuthSignerPriv:   eip7702.MustLoadKey(cfg.AuthPK),
		EnableSimulation: true, // simulate raw 7702 tx via eth_callBundle before sending
	}
	fmt.Println("  [*] Отправляю приватную 7702-транзакцию…")
	out, err := eip7702.ExecuteRescue(ctx, ec, req)
	if err != nil { return err }
	fmt.Println("  tx:", out.TxHash.Hex())
	for _, a := range out.RelayAttempts {
		fmt.Printf("    [%s] %s -> %d accepted=%v\n", a.RelayURL, a.RequestMethod, a.HTTPStatus, a.Accepted)
		if strings.TrimSpace(a.ResponseBody) != "" {
			fmt.Println("      resp:", a.ResponseBody)
		}
	}
	return nil
}

// runClassicBundleFromRescueMenu builds core.Params similar to the classic REPL and calls core.Run().
// mode: "1" => fixed tip; "2" => feehist + optional coinbase bribe.
func runClassicBundleFromRescueMenu(
	ctx context.Context,
	ec *ethclient.Client,
	chainID *big.Int,
	cfg EnvConfig,
	fromPK string,
	fromAddr common.Address,
	tokenAddr common.Address,
	toAddr common.Address,
	mode string,
) error {
	// Resolve full balance as amount (single-token flow)
	bal, err := fetchTokenBalance(ctx, ec, tokenAddr, fromAddr)
	if err != nil {
		return fmt.Errorf("fetch balance failed: %w", err)
	}
	if bal.Sign() == 0 {
		return fmt.Errorf("token balance is zero")
	}

	// Extra headers (bloxroute): keep parity with classic flow (API key OR ready Authorization)
	extraHeaders := map[string]map[string]string{}
	if v := getenv("BLOXROUTE_RELAY", ""); v != "" {
		h := map[string]string{}
		if k := getenv("BLOXROUTE_API_KEY", ""); k != "" {
			// Classic path sets both headers for Cloud API
			h["X-API-KEY"] = k
			h["Authorization"] = k
		}
		if auth := getenv("BLOXROUTE_AUTH_HEADER", ""); auth != "" {
			// Allow overriding with ready Authorization header
			h["Authorization"] = auth
		}
		if len(h) > 0 {
			extraHeaders[v] = h
		}
	}

	// Tip strategy
	tipMode := "fixed"
	tipWindow := 100
	tipPercentile := 99
	tipBase := cfg.TipGwei
	if mode == "1" {
		// Allow manual override of TIP_GWEI for standard bundle
		if yes(strings.ToLower(readLine(bufio.NewReader(os.Stdin), "Задать TIP_GWEI вручную для этого вывода? [y/N]: "))) {
			if s := strings.TrimSpace(readLine(bufio.NewReader(os.Stdin), "Введите TIP_GWEI: ")); s != "" {
				if v, err := strconv.ParseInt(s, 10, 64); err == nil && v >= 0 {
					tipBase = v
				}
			}
		}
	} else {
		// feehist
		tipMode = "feehist"
		if s := strings.TrimSpace(readLine(bufio.NewReader(os.Stdin), "Окно feeHistory (блоков) [ENTER=100]: ")); s != "" {
			if v, err := strconv.Atoi(s); err == nil && v > 0 && v < 5000 { tipWindow = v }
		}
		if s := strings.TrimSpace(readLine(bufio.NewReader(os.Stdin), "Перцентиль вознаграждения [ENTER=99]: ")); s != "" {
			if v, err := strconv.Atoi(s); err == nil && v >= 1 && v <= 99 { tipPercentile = v }
		}
	}

	// Optional coinbase bribe only for mode==2 (as per your menu)
	var bribeWei *big.Int
	var bribeGasLimit uint64
	if mode == "2" && yes(strings.ToLower(readLine(bufio.NewReader(os.Stdin), "Включить coinbase bribe? [y/N]: "))) {
		if s := strings.TrimSpace(readLine(bufio.NewReader(os.Stdin), "Сумма bribe в ETH (например 0.02) [ENTER=0]: ")); s != "" {
			if v, ok := parseAmountETHToWei(s); ok {
				bribeWei = v
			}
		}
		if s := strings.TrimSpace(readLine(bufio.NewReader(os.Stdin), "GasLimit для bribe [ENTER=50000]: ")); s != "" {
			if v, err := strconv.ParseUint(s, 10, 64); err == nil && v > 21000 && v < 1_000_000 {
				bribeGasLimit = v
			} else {
				bribeGasLimit = 50_000
			}
		} else {
			bribeGasLimit = 50_000
		}
	}

	// Assemble params (mirrors classic path in params_build.go)
	params := core.Params{
		RPC: cfg.RPC, ChainID: chainID,
		Relays: splitCSV(cfg.RelaysCSV),
		AuthPrivHex: cfg.AuthPK,
		Token: tokenAddr, From: fromAddr, To: toAddr, AmountWei: new(big.Int).Set(bal),
		SafePKHex: cfg.SafePK, FromPKHex: fromPK,
		Blocks: cfg.Blocks, TipGweiBase: tipBase, TipMul: cfg.TipMul, BaseMul: cfg.BaseMul, BufferPct: cfg.BufferPct,
		TipMode: tipMode, TipWindow: tipWindow, TipPercentile: tipPercentile,
		BribeWei: bribeWei, BribeGasLimit: bribeGasLimit, ExtraHeaders: extraHeaders,
		Builders: cfg.Builders, ReplacementUUID: "", MinTimestamp: cfg.MinTs, MaxTimestamp: cfg.MaxTs,
		BeaverAllowBuilderNetRefunds: &cfg.BeaverAllow, BeaverRefundRecipientHex: cfg.BeaverRefundTo,
		Verbose: false, SimulateOnly: false, SkipIfPaused: true,
		Logf: func(format string, a ...any){ fmt.Printf(format+"\n", a...) },
		OnSimResult: func(relay, raw string, ok bool, err string){
			state := "OK"; if !ok { state = "FAIL" }
			if err != "" { err = friendlySimErr(err) }
			fmt.Printf("  [sim %s] %s err=%s\n", relay, state, err)
		},
	}

	fmt.Println("  [*] Отправляю классический бандл…")
	if res, err := core.Run(ctx, ec, params); err != nil {
		return fmt.Errorf("classic bundle error: %w", err)
	} else {
		fmt.Println("  [RESULT]", res.Reason, "| included:", res.Included)
	}
	return nil
}

// parseAmountETHToWei parses a decimal ETH string (e.g., "0.02", "1", ".5") into wei.
// Returns (value, true) on success; (nil, false) on invalid input or >18 fractional digits.
func parseAmountETHToWei(s string) (*big.Int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	// handle optional sign
	neg := false
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	} else if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	parts := strings.SplitN(s, ".", 2)
	intPart := parts[0]
	if intPart == "" {
		intPart = "0"
	}
	// validate digits
	for _, ch := range intPart {
		if ch < '0' || ch > '9' {
			return nil, false
		}
	}
	var fracPart string
	if len(parts) == 2 {
		fracPart = parts[1]
		for _, ch := range fracPart {
			if ch < '0' || ch > '9' {
				return nil, false
			}
		}
		if len(fracPart) > 18 {
			// more than 18 decimals is not representable in wei without rounding
			return nil, false
		}
		// right-pad to 18 decimals
		fracPart = fracPart + strings.Repeat("0", 18-len(fracPart))
	} else {
		fracPart = strings.Repeat("0", 18)
	}
	intWei := new(big.Int)
	if _, ok := intWei.SetString(intPart, 10); !ok {
		return nil, false
	}
	// multiply integer part by 1e18
	weiMul := new(big.Int).SetUint64(1_000_000_000_000_000_000)
	intWei.Mul(intWei, weiMul)
	// add fractional part (already 18 digits)
	fracWei := new(big.Int)
	if _, ok := fracWei.SetString(fracPart, 10); !ok {
		return nil, false
	}
	res := new(big.Int).Add(intWei, fracWei)
	if neg {
		res.Neg(res)
	}
	return res, true
}


// inspectTokenGuards reads common honeypot guards and returns (ok, warning, error).
// Checks:
//  - bots(victim/recipient)
//  - _maxTxAmount()
//  - _maxWalletSize() with recipient balance
func inspectTokenGuards(ctx context.Context, ec *ethclient.Client, token common.Address, victim common.Address, recipient common.Address, amount *big.Int) (bool, string, error) {
	const guardsABI = `[
	  {"type":"function","name":"bots","stateMutability":"view","inputs":[{"name":"a","type":"address"}],"outputs":[{"type":"bool"}]},
	  {"type":"function","name":"_maxTxAmount","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
	  {"type":"function","name":"_maxWalletSize","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
	  {"type":"function","name":"_swapTokensAtAmount","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
	  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"a","type":"address"}],"outputs":[{"type":"uint256"}]}
	]`
	parser, err := abi.JSON(strings.NewReader(guardsABI))
	if err != nil { return false, "ABI parse failed", err }

	// helpers
	call := func(data []byte, from common.Address) ([]byte, error) {
		msg := ethereum.CallMsg{From: from, To: &token, Data: data}
		return ec.CallContract(ctx, msg, nil)
	}
	// 1) bots(victim)
	if method, ok := parser.Methods["bots"]; ok {
		data, _ := method.Inputs.Pack(victim)
		data = append(method.ID, data...)
		if ret, err := call(data, victim); err == nil {
			if out, err := method.Outputs.Unpack(ret); err == nil && len(out) == 1 {
				if black, _ := out[0].(bool); black { return false, "bots[victim]==true (blacklisted)", nil }
			}
		}
		// bots(recipient)
		data2, _ := method.Inputs.Pack(recipient)
		data2 = append(method.ID, data2...)
		if ret, err := call(data2, victim); err == nil {
			if out, err := method.Outputs.Unpack(ret); err == nil && len(out) == 1 {
				if black, _ := out[0].(bool); black { return false, "bots[recipient]==true (blacklisted)", nil }
			}
		}
	}
	// 2) _maxTxAmount()
	if method, ok := parser.Methods["_maxTxAmount"]; ok {
		data := method.ID
		if ret, err := call(data, victim); err == nil {
			if out, err := method.Outputs.Unpack(ret); err == nil && len(out) == 1 {
				if maxTx, _ := out[0].(*big.Int); maxTx != nil && maxTx.Sign() > 0 && amount.Cmp(maxTx) > 0 {
					return false, fmt.Sprintf("amount > _maxTxAmount (%s > %s)", amount.String(), maxTx.String()), nil
				}
			}
		}
	}
	// 3) _maxWalletSize() vs recipient balance
	if method, ok := parser.Methods["_maxWalletSize"]; ok {
		data := method.ID
		if ret, err := call(data, victim); err == nil {
			if out, err := method.Outputs.Unpack(ret); err == nil && len(out) == 1 {
				if maxWal, _ := out[0].(*big.Int); maxWal != nil && maxWal.Sign() > 0 {
					balTo, _ := fetchTokenBalance(ctx, ec, token, recipient)
					sum := new(big.Int).Add(balTo, amount)
					if sum.Cmp(maxWal) >= 0 {
						return false, fmt.Sprintf("recipient balance limit: balance(%s)+amount(%s) >= _maxWalletSize(%s)", balTo.String(), amount.String(), maxWal.String()), nil
					}
				}
			}
		}
	}
  // 4) swap trigger: if token's own balance >= _swapTokensAtAmount then non-pair transfer may trigger an internal swap (risky).
  if method, ok := parser.Methods["_swapTokensAtAmount"]; ok {
      data := method.ID
      if ret, err := call(data, victim); err == nil {
          if out, err := method.Outputs.Unpack(ret); err == nil && len(out) == 1 {
              if thr, _ := out[0].(*big.Int); thr != nil && thr.Sign() > 0 {
                  // contract self-balance
                  selfBal, _ := fetchTokenBalance(ctx, ec, token, token)
                  if selfBal.Cmp(thr) >= 0 {
                      return false, fmt.Sprintf("token self-balance (%s) >= _swapTokensAtAmount (%s) — internal swap may revert", selfBal.String(), thr.String()), nil
                  }
              }
          }
      }
  }	
	return true, "", nil
}

// parseCSVAddresses converts "a,b,c" to []common.Address with validation.
func parseCSVAddresses(s string) ([]common.Address, error) {
	s = strings.TrimSpace(s)
	if s == "" { return nil, fmt.Errorf("empty") }
	parts := strings.Split(s, ",")
	out := make([]common.Address, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if !common.IsHexAddress(p) { return nil, fmt.Errorf("bad address: %s", p) }
		out = append(out, common.HexToAddress(p))
	}
	return out, nil
}

// parseUint64Flexible parses decimal like "1275" or hex like "0x4fb".
func parseUint64Flexible(s string) (uint64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if strings.HasPrefix(s, "0x") {
		var v uint64
		_, err := fmt.Sscanf(s, "0x%x", &v)
		return v, err
	}
	var v uint64
	_, err := fmt.Sscan(s, &v)
	return v, err
}

// preflightERC20Transfer simulates token.transfer(to, balanceOf(from)) from `from` address.
// Returns (true, "", nil) if transfer likely succeeds; (false, reason, nil) if contract/transfer looks bad.
func preflightERC20Transfer(ctx context.Context, ec *ethclient.Client, token common.Address, from common.Address, to common.Address) (bool, string, error) {
	// 1) Basic: ensure code exists at token address.
	code, err := ec.CodeAt(ctx, token, nil)
	if err != nil { return false, "failed to fetch code", err }
	if len(code) == 0 { return false, "no contract code at token address", nil }
	// 2) Balance to transfer.
	bal, err := fetchTokenBalance(ctx, ec, token, from)
	if err != nil { return false, "balanceOf() failed", nil }
	if bal.Sign() == 0 { return true, "zero balance", nil }
	// 3) Build calldata for transfer(to, bal).
	const erc20ABI = `[{"type":"function","name":"transfer","stateMutability":"nonpayable","inputs":[{"name":"to","type":"address"},{"name":"value","type":"uint256"}],"outputs":[{"type":"bool"}]}]`
	parsed, err := abi.JSON(strings.NewReader(erc20ABI))
	if err != nil { return false, "ABI parse failed", err }
	data, err := parsed.Pack("transfer", to, bal)
	if err != nil { return false, "ABI pack failed", err }
	// 4) Static eth_call from `from` (EOA context matches 7702 execution semantics).
	msg := ethereum.CallMsg{From: from, To: &token, Data: data}
	ret, callErr := ec.CallContract(ctx, msg, nil)
	if callErr != nil { return false, "revert on transfer()", nil }
	// 5) Interpret return data: empty or bool(true) => ok; anything else => bad.
	if len(ret) == 0 { return true, "", nil }
	// Decode explicitly
	out, err := parsed.Methods["transfer"].Outputs.Unpack(ret)
	if err != nil { return false, "unexpected return data", nil }
	if len(out) == 1 {
		if b, _ := out[0].(bool); b {
			return true, "", nil
		}
	}
	return false, "transfer() returned false", nil
}