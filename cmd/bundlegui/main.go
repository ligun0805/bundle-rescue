package main

import (
	"bufio"
	"context"
	"fmt"
	"image/color"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/crypto"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/joho/godotenv"
	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// --- UI globals used across files (ui_run.go needs them) ---
var (
	pairsTable   *widget.Table
	pairScenario []string   // per-row chosen scenario "1"/"2"/"3"
	pairStatus   []string   // per-row status: "", PENDING, FAILED, COMPLETED
	pairCheckS   []string   // short check text for row
	pairCheckD   []string   // details text for dialog
)

func main() {
	hideConsoleWindow()

	_ = godotenv.Load()
	_ = godotenv.Overload(".env.local")

	a := app.New()
	curTheme := makeTheme("dark", false)
	a.Settings().SetTheme(curTheme)

	w := a.NewWindow("Bundle Rescue")
	w.SetOnClosed(func(){
		if viewWin != nil { viewWin.Close(); viewWin = nil }
		if logWin  != nil { logWin.Close();  logWin  = nil }
		closeAddPairWindows()
	})
	loadQueueFromFile()
	w.Resize(fyne.NewSize(1180, 760))

	rpcEntry := widget.NewEntry(); rpcEntry.SetText(os.Getenv("RPC_URL"))
	chainEntry := widget.NewEntry(); chainEntry.SetText(defaultStr(os.Getenv("CHAIN_ID"), "1"))
	relaysEntry := widget.NewEntry(); relaysEntry.SetText(defaultStr(os.Getenv("RELAYS"), "https://relay.flashbots.net"))
	authPkEntry := widget.NewPasswordEntry(); authPkEntry.SetText(os.Getenv("FLASHBOTS_AUTH_PK"))
	safePkEntry := widget.NewPasswordEntry(); safePkEntry.SetText(os.Getenv("SAFE_PRIVATE_KEY"))

	useEnvGlobals := widget.NewCheck("Use .env globals (lock)", func(b bool){
		rpcEntry.Disable(); chainEntry.Disable(); relaysEntry.Disable(); authPkEntry.Disable(); safePkEntry.Disable()
		if !b { rpcEntry.Enable(); chainEntry.Enable(); relaysEntry.Enable(); authPkEntry.Enable(); safePkEntry.Enable() }
	})
	useEnvGlobals.SetChecked(true)

	blocks := widget.NewEntry(); blocks.SetText(defaultStr(os.Getenv("BLOCKS"), "6"))
	tip := widget.NewEntry(); tip.SetText(defaultStr(os.Getenv("TIP_GWEI"), "3"))
	tipMul := widget.NewEntry(); tipMul.SetText(defaultStr(os.Getenv("TIP_MUL"), "1.25"))
	baseMul := widget.NewEntry(); baseMul.SetText(defaultStr(os.Getenv("BASEFEE_MUL"), "2"))
	buffer := widget.NewEntry(); buffer.SetText(defaultStr(os.Getenv("BUFFER_PCT"), "5"))

	themeSelect := widget.NewSelect([]string{"Dark","Light"}, func(s string){
		mode := "dark"; if s == "Light" { mode = "light" }
		curTheme = makeTheme(mode, curTheme.(*appTheme).compact)
		a.Settings().SetTheme(curTheme)
	})
	themeSelect.SetSelected("Dark")
	compactCheck := widget.NewCheck("Compact", func(b bool){
		curTheme = makeTheme(curTheme.(*appTheme).mode, b)
		a.Settings().SetTheme(curTheme)
	})

	// Read-only fields: Delegate & SAFE_ADDRESS (без bindReadOnly)
	delegateEntry := widget.NewEntry()
	delegateEntry.SetText(os.Getenv("DELEGATE_ADDRESS"))
	delegateEntry.Disable()
	safeAddrEntry := widget.NewEntry()
	if v, err := deriveAddrFromPK(strings.TrimSpace(safePkEntry.Text)); err == nil { safeAddrEntry.SetText(v) }
	safeAddrEntry.Disable()
	safePkEntry.OnChanged = func(s string){
		if v, err := deriveAddrFromPK(strings.TrimSpace(s)); err == nil { safeAddrEntry.SetText(v) } else { safeAddrEntry.SetText("") }
	}

	globalsCard := widget.NewCard("Globals", "", widget.NewForm(
		widget.NewFormItem("RPC URL", rpcEntry),
		widget.NewFormItem("Chain ID", chainEntry),
		widget.NewFormItem("Relays", relaysEntry),
		widget.NewFormItem("Auth PK", authPkEntry),
		widget.NewFormItem("Delegate (7702)", delegateEntry),
		widget.NewFormItem("Safe PK", safePkEntry),
		widget.NewFormItem("SAFE_ADDRESS", safeAddrEntry),
		widget.NewFormItem("", container.NewGridWithColumns(3, useEnvGlobals, themeSelect, compactCheck)),
	))

	strategyCard := widget.NewCard("Strategy", "", widget.NewForm(
		widget.NewFormItem("Blocks", blocks),
		widget.NewFormItem("Tip (gwei)", tip),
		widget.NewFormItem("Tip ×", tipMul),
		widget.NewFormItem("BaseFee ×", baseMul),
		widget.NewFormItem("Buffer %", buffer),
	))
	
	// ---------- Imported Pairs (full-height list) ----------
	// state arrays declared at package level (used in ui_run.go too)

	// Local helper to render token amount from wei string considering decimals.
	formatTokFromWei := func(weiStr string, decimals int) string {
		if strings.TrimSpace(weiStr) == "" { return "0" }
		wei := new(big.Int)
		if _, ok := wei.SetString(strings.TrimSpace(weiStr), 10); !ok { return "0" }
		if decimals <= 0 { return wei.String() }
		den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
		q, r := new(big.Int).QuoRem(wei, den, new(big.Int))
		if r.Sign() == 0 { return q.String() }
		rStr := r.String()
		if len(rStr) < decimals {
			rStr = strings.Repeat("0", decimals-len(rStr)) + rStr
		}
		rStr = strings.TrimRight(rStr, "0")
		if len(rStr) > 8 { rStr = rStr[:8] }
		return fmt.Sprintf("%s.%s", q.String(), rStr)
	}
	// Table with imported pairs (8 columns)
	pairsTable = widget.NewTable(
		func() (int, int) { return len(pairs)+1, 8 }, // rows, cols
		func() fyne.CanvasObject {
			// reusable cell: label + details + scenario + delete
			lbl := widget.NewLabel("")
			btn := widget.NewButton("Details", nil)
			sel := widget.NewSelect([]string{"1","2","3"}, nil)
			ref := widget.NewButtonWithIcon("", theme.ViewRefreshIcon(), nil)
			del := widget.NewButtonWithIcon("", theme.DeleteIcon(), nil)
			return container.NewHBox(lbl, btn, sel, ref, del)
		},
		func(id widget.TableCellID, obj fyne.CanvasObject) {
			row, col := id.Row-1, id.Col
			box := obj.(*fyne.Container)
			lbl := box.Objects[0].(*widget.Label)
			btn := box.Objects[1].(*widget.Button)
			sel := box.Objects[2].(*widget.Select)
			ref := box.Objects[3].(*widget.Button)
			del := box.Objects[4].(*widget.Button)
			// reset visibilities
			lbl.Hide(); btn.Hide(); sel.Hide(); ref.Hide(); del.Hide()
			lbl.TextStyle = fyne.TextStyle{}
			if id.Row == 0 {
				// header
				lbl.Show()
				lbl.TextStyle = fyne.TextStyle{Bold:true}
				switch col {
				case 0: lbl.SetText("#")
				case 1: lbl.SetText("From")
				case 2: lbl.SetText("Token")
				case 3: lbl.SetText("Balance")
				case 4: lbl.SetText("Check")
				case 5: lbl.SetText("Scenario")
				case 6: lbl.SetText("Status")
				case 7: lbl.SetText("Actions")				
				}
				return
			}
			if row < 0 || row >= len(pairs) { return }
			pr := pairs[row]
			// ensure side arrays have capacity
			for len(pairScenario) < len(pairs) { pairScenario = append(pairScenario, "") }
			for len(pairStatus)   < len(pairs) { pairStatus   = append(pairStatus,   "") }
			for len(pairCheckS)   < len(pairs) { pairCheckS   = append(pairCheckS,   "") }
			for len(pairCheckD)   < len(pairs) { pairCheckD   = append(pairCheckD,   "") }
			switch col {
			case 0:
				lbl.Show(); lbl.SetText(fmt.Sprintf("%d", row+1))
			case 1:
				lbl.Show(); lbl.TextStyle = fyne.TextStyle{Monospace: true}; lbl.SetText(pr.From)
			case 2:
				lbl.Show(); lbl.TextStyle = fyne.TextStyle{Monospace: true}; lbl.SetText(pr.Token)
			case 3:
				lbl.Show(); lbl.SetText(formatTokFromWei(pr.BalanceWei, pr.Decimals))
			case 4:
				// short + details button
				lbl.Show()
				if pairCheckS[row] == "" {
					if strings.TrimSpace(pr.BalanceWei) == "" || pr.BalanceWei == "0" {
						pairCheckS[row] = "No balance"
					} else {
						pairCheckS[row] = "OK"
					}
					pairCheckD[row] = fmt.Sprintf("From: %s\nToken: %s\nDecimals: %d\nBalance (wei): %s",
						pr.From, pr.Token, pr.Decimals, pr.BalanceWei)
				}
				lbl.SetText(pairCheckS[row])
				btn.Show()
				btn.OnTapped = func() {
					dialog.ShowInformation("Check details", pairCheckD[row], w)
				}
			case 5:
				// scenario selector
				sel.Show()
				if pairScenario[row] != "" {
					sel.SetSelected(pairScenario[row])
				} else {
					sel.ClearSelected()
				}
				sel.OnChanged = func(v string){ pairScenario[row] = v }
			case 6:
				// status text
				lbl.Show()
				if pairStatus[row] == "" { pairStatus[row] = "PENDING" }
				lbl.SetText(pairStatus[row])
			case 7:
				// actions: refresh + delete (в отдельной колонке)
				ref.Show()
				ref.OnTapped = func() {
					i := row
					if i < 0 || i >= len(pairs) { return }
					pd := dialog.NewProgressInfinite("Re-check", "Rechecking pair…", w)
					pd.Show()
					go func() {
						defer pd.Hide()
						ec, err := ethclient.Dial(strings.TrimSpace(rpcEntry.Text))
						if err != nil {
							pairCheckS[i] = "FAIL: rpc dial"
							pairCheckD[i] = "RPC dial error: " + err.Error()
							pairsTable.Refresh()
							return
						}
						pr := pairs[i]
						if !common.IsHexAddress(pr.Token) || !common.IsHexAddress(pr.From) || !common.IsHexAddress(pr.To) {
							pairCheckS[i] = "FAIL: bad address"
							pairCheckD[i] = fmt.Sprintf("Bad address in pair:\nFrom=%s\nToken=%s\nTo=%s", pr.From, pr.Token, pr.To)
							pairsTable.Refresh(); return
						}
						token := common.HexToAddress(pr.Token)
						from  := common.HexToAddress(pr.From)
						to    := common.HexToAddress(pr.To)
						gOK, gShort, gDetail := guardChecksRetry(ec, token, from, to)
						if !gOK {
							pairCheckS[i] = "FAIL: " + gShort
							pairCheckD[i] = "Guards: " + gDetail
							pairsTable.Refresh(); return
						}
						restrSum, blocked := checkRestrictionsRetry(ec, token, from, to)
						if blocked {
							pairCheckS[i] = "FAIL: " + restrSum
							pairCheckD[i] = fmt.Sprintf("Guards: %s\nRestrictions: %s\nFrom=%s\nToken=%s\nTo=%s",
								gDetail, restrSum, pr.From, pr.Token, pr.To)
							pairsTable.Refresh(); return
						}
						ok, why := preflightSimpleRetry(ec, token, from, to, pr.Decimals, pr.BalanceWei)
						switch {
						case !ok && why != "":
							pairCheckS[i] = "FAIL: " + why
						case !ok:
							pairCheckS[i] = "FAIL"
						case strings.EqualFold(why, "zero balance"):
							pairCheckS[i] = "No balance"
						default:
							pairCheckS[i] = "OK"
						}
						pairCheckD[i] = fmt.Sprintf("Guards: %s\nRestrictions: %s\nPreflight: %s\nFrom=%s\nToken=%s\nTo=%s",
							gDetail, restrSum, why, pr.From, pr.Token, pr.To)
						pairsTable.Refresh()
					}()
				}
				del.Show()
				del.OnTapped = func() {
					i := row
					if i < 0 || i >= len(pairs) { return }
					pairs = append(pairs[:i], pairs[i+1:]...)
					if i < len(pairScenario) { pairScenario = append(pairScenario[:i], pairScenario[i+1:]...) }
					if i < len(pairStatus)   { pairStatus   = append(pairStatus[:i],   pairStatus[i+1:]...) }
					if i < len(pairCheckS)   { pairCheckS   = append(pairCheckS[:i],   pairCheckS[i+1:]...) }
					if i < len(pairCheckD)   { pairCheckD   = append(pairCheckD[:i],   pairCheckD[i+1:]...) }
					saveQueueToFile()
					pairsTable.Refresh()
				}
			}
		},
	)
	// widen columns + enable horizontal scroll
	pairsTable.SetColumnWidth(0,  44)  // #
	pairsTable.SetColumnWidth(1, 420)  // From
	pairsTable.SetColumnWidth(2, 460)  // Token
	pairsTable.SetColumnWidth(3, 200)  // Balance
	pairsTable.SetColumnWidth(4, 200)  // Check
	pairsTable.SetColumnWidth(5, 160)  // Scenario
	pairsTable.SetColumnWidth(6, 160)  // Status
	pairsTable.SetColumnWidth(7, 130)  // Actions (Refresh + Delete)
	importedPairsCard := widget.NewCard("Imported Pairs", "", container.NewScroll(pairsTable))
	
	// ---------- Footer: Network snapshot (single line, minimal height) ----------
	netLineLbl := widget.NewLabel("[net] baseFee: — gwei · tip: — gwei · gas(≈40766): fixed=— ETH, peak=— ETH")
	// a thin padded bar with single label to save vertical space
	netFooter  := container.NewPadded(netLineLbl)

	// helpers
	parseFloat := func(s string, def float64) float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil { return def }
		return v
	}
	weiToGwei := func(x *big.Int) float64 {
		if x == nil { return 0 }
		f := new(big.Float).SetInt(x)                // wei
		g := new(big.Float).Quo(f, big.NewFloat(1e9)) // gwei
		val, _ := g.Float64()
		return val
	}
	// Updates footer label with current network info (single line).
	updateNetwork := func() {
		go func() {
			ctx := context.Background()
			ec, err := ethclient.Dial(strings.TrimSpace(rpcEntry.Text))
			if err != nil {
				dialog.ShowError(fmt.Errorf("RPC dial failed: %w", err), w)
				return
			}
			defer ec.Close()
			// baseFee(now)
			h, err := ec.HeaderByNumber(ctx, nil)
			if err != nil {
				dialog.ShowError(fmt.Errorf("header: %w", err), w); return
			}
			baseGwei := weiToGwei(h.BaseFee)
			// tip(suggested)
			tipWei, err := ec.SuggestGasTipCap(ctx)
			if err != nil { tipWei = big.NewInt(0) }
			tipGwei := weiToGwei(tipWei)
			// gas cost estimate (transfer≈40766)
			const transferGas = 40766.0
			// effective multipliers from Strategy
			tm := parseFloat(tipMul.Text, 1.25)
			bm := parseFloat(baseMul.Text, 2.0)
			fixedEth := baseGwei * transferGas * 1e-9
			peakEth  := ((baseGwei*bm) + (tipGwei*tm)) * transferGas * 1e-9
			netLineLbl.SetText(
				fmt.Sprintf("[net] baseFee: %.2f gwei · tip: %.2f gwei · gas(≈40766): fixed=%.6f ETH, peak=%.6f ETH",
					baseGwei, tipGwei, fixedEth, peakEth),
			)
		}()
	}	

	importBtn := widget.NewButtonWithIcon("IMPORT LIST", theme.FolderOpenIcon(), func(){
		// Открываем диалог выбора файла, старт — рабочая директория приложения
		cb := func(rc fyne.URIReadCloser, err error){
			if err!=nil || rc==nil { return }
			defer rc.Close()
			ext := strings.ToLower(rc.URI().Extension())
			var ps []pairRow
			if ext==".txt" || ext=="" {
				// Each line: "<fromPrivKey> <tokenAddress>"
				ec, e := ethclient.Dial(rpcEntry.Text); if e!=nil { dialog.ShowInformation("Import", "RPC dial error: "+e.Error(), w); return }
				for scanner := bufio.NewScanner(rc); scanner.Scan(); {
					line := strings.TrimSpace(scanner.Text()); if line=="" || strings.HasPrefix(line,"#") { continue }
					parts := strings.Fields(line); if len(parts) < 2 { continue }
					fromPK := parts[0]; token := strings.ToLower(parts[1])
					fromAddr, derr := deriveAddrFromPK(fromPK); if derr!=nil { continue }
					dec := 18; if d, e := fetchTokenDecimals(ec, common.HexToAddress(token)); e==nil { dec = d }
					balWei := big.NewInt(0); if b, e := fetchTokenBalance(ec, common.HexToAddress(token), common.HexToAddress(fromAddr)); e==nil { balWei = b }
					toAddr := ""; if v, err := deriveAddrFromPK(strings.TrimSpace(safePkEntry.Text)); err==nil { toAddr = v }
					ps = append(ps, pairRow{ Token: token, From: strings.ToLower(fromAddr), FromPK: fromPK, To: toAddr, Decimals: dec, AmountWei: balWei.String(), BalanceWei: balWei.String() })
				}
			} else if ext==".csv" {
				if arr, e := parseCSVAll(rc); e==nil { ps = arr }
			} else if ext==".json" {
				if arr, e := parseJSONAll(rc); e==nil { ps = arr }
			} else {
				dialog.ShowInformation("Import", `Use .txt ("<privKey> <token>") or CSV/JSON`, w); return
			}
			if len(ps)==0 { return }
			start := len(pairs)
			pairs = append(pairs, ps...)
			statsAdded += len(ps)
			saveQueueToFile()
			// init Ui-side arrays for new rows
			for i:=0; i<len(ps); i++ {
				pairScenario = append(pairScenario, "")
				pairStatus   = append(pairStatus,   "PENDING")
				// fill check texts now
				pr := pairs[start+i]
				if strings.TrimSpace(pr.BalanceWei) == "" || pr.BalanceWei == "0" {
					pairCheckS = append(pairCheckS, "No balance")
				} else {
					pairCheckS = append(pairCheckS, "OK")
				}
				pairCheckD = append(pairCheckD, fmt.Sprintf("From: %s\nToken: %s\nDecimals: %d\nBalance (wei): %s",
					pr.From, pr.Token, pr.Decimals, pr.BalanceWei))
			}
			pairsTable.Refresh() // refresh list

			// --- Проверки по парам с прогресс-баром и ретраями ---
			ec, err := ethclient.Dial(rpcEntry.Text)
			if err != nil { dialog.ShowError(fmt.Errorf("RPC dial failed: %w", err), w); return }
			total := float64(len(pairs)-start)
			prog := dialog.NewProgress("Import checks", "Running token checks…", w)
			prog.Show()
			for i := start; i < len(pairs); i++ {
				pr := pairs[i]
				// Validate addresses
				if !common.IsHexAddress(pr.Token) || !common.IsHexAddress(pr.From) || !common.IsHexAddress(pr.To) {
					pairCheckS[i] = "FAIL: bad address"
					pairCheckD[i] = fmt.Sprintf("Bad address in pair:\nFrom=%s\nToken=%s\nTo=%s", pr.From, pr.Token, pr.To)
					pairsTable.Refresh()
					prog.SetValue(float64(i-start+1)/total)
					continue
				}
				token := common.HexToAddress(pr.Token)
				from  := common.HexToAddress(pr.From)
				to    := common.HexToAddress(pr.To)
				
				gOK, gShort, gDetail := guardChecksRetry(ec, token, from, to)
				if !gOK {
					pairCheckS[i] = "FAIL: " + gShort
					pairCheckD[i] = "Guards: " + gDetail
					pairsTable.Refresh()
					prog.SetValue(float64(i-start+1)/total)
					continue
				}				

				// Restrictions через bundlecore с ретраями
				restrSum, blocked := checkRestrictionsRetry(ec, token, from, to)
				if blocked {
					pairCheckS[i] = "FAIL: " + restrSum
					pairCheckD[i] = fmt.Sprintf("Guards: %s\nRestrictions: %s\nFrom=%s\nToken=%s\nTo=%s",
						gDetail, restrSum, pr.From, pr.Token, pr.To)
					pairsTable.Refresh()
					prog.SetValue(float64(i-start+1)/total)
					continue
				}

				// Preflight via eth_call (transfer(to, min(balance, 1 unit)))
				ok, why := preflightSimpleRetry(ec, token, from, to, pr.Decimals, pr.BalanceWei)
				switch {
				case !ok && why != "":
					pairCheckS[i] = "FAIL: " + why
				case !ok:
					pairCheckS[i] = "FAIL"
				case strings.EqualFold(why, "zero balance"):
					pairCheckS[i] = "No balance"
				default:
					pairCheckS[i] = "OK"
				}
				pairCheckD[i] = fmt.Sprintf("Guards: %s\nRestrictions: %s\nPreflight: %s\nFrom=%s\nToken=%s\nTo=%s",
					gDetail, restrSum, why, pr.From, pr.Token, pr.To)
				prog.SetValue(float64(i-start+1)/total)
			}
			prog.Hide()
		}
		fd := dialog.NewFileOpen(cb, w)
		if wd, err := os.Getwd(); err == nil {
			if l, err := storage.ListerForURI(storage.NewFileURI(wd)); err == nil {
				fd.SetLocation(l)
			}
		}
		fd.Show()
	})

	buttons := container.NewGridWithColumns(2, importBtn, widget.NewButton("REMOVE NON-TRANSFERABLE", func(){
		var keep []pairRow
		var keepSc, keepSt, keepS, keepD []string
		for _,pr := range pairs {
			if strings.TrimSpace(pr.BalanceWei)!="0" && strings.TrimSpace(pr.BalanceWei)!="" {
				keep = append(keep, pr)
				idx := len(keep)-1
				// переносим параллельные массивы
				if len(pairScenario) > idx { keepSc = append(keepSc, pairScenario[idx]) } else { keepSc = append(keepSc, "") }
				if len(pairStatus)   > idx { keepSt = append(keepSt, pairStatus[idx])   } else { keepSt = append(keepSt, "") }
				if len(pairCheckS)   > idx { keepS  = append(keepS,  pairCheckS[idx])   } else { keepS  = append(keepS,  "") }
				if len(pairCheckD)   > idx { keepD  = append(keepD,  pairCheckD[idx])   } else { keepD  = append(keepD,  "") }
			}
		}
		pairs = keep
		pairScenario, pairStatus, pairCheckS, pairCheckD = keepSc, keepSt, keepS, keepD
		saveQueueToFile()
		pairsTable.Refresh() // refresh list
	}))

	resBtn := widget.NewButtonWithIcon("RESCUE",   theme.ConfirmIcon(),   func(){
        go runAll(a, false,
            rpcEntry.Text, chainEntry.Text, relaysEntry.Text,
            authPkEntry.Text, safePkEntry.Text,
            blocks.Text, tip.Text, tipMul.Text, baseMul.Text, buffer.Text,
        )
    })
	runRow := container.NewGridWithColumns(2,
		widget.NewButton("UPDATE NETWORK", func(){ updateNetwork() }),
		resBtn,
	)

    // layout: top (globals+strategy+buttons+run) and center (pairs list) to occupy the remaining height
    top := container.NewVBox(globalsCard, strategyCard, buttons, runRow)
    center := importedPairsCard
    bg := canvas.NewLinearGradient(color.NRGBA{12,16,24,255}, color.NRGBA{20,28,40,255}, 90)
    w.SetContent(
        container.NewMax(
            bg,
            container.NewBorder(top, netFooter, nil, nil, center),
        ),
    )
	updateNetwork()
	w.ShowAndRun()
}


// preflightSimple simulates ERC20 transfer(to, amount) from 'from' via eth_call.
// amount = min(balanceWei, 1*10^dec). Returns (ok, reason).
func preflightSimple(ctx context.Context, ec *ethclient.Client, token, from, to common.Address, dec int, balanceWeiStr string) (bool, string) {
	// parse balance
	bal := new(big.Int)
	if _, ok := bal.SetString(strings.TrimSpace(balanceWeiStr), 10); !ok {
		// try to fetch live if parse failed
		if b, err := fetchTokenBalance(ec, token, from); err == nil { bal = b } else { bal.SetInt64(0) }
	}
	if bal.Sign() == 0 {
		return true, "zero balance"
	}
	// 1 unit
	one := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(max(dec,0))), nil)
	amt := new(big.Int).Set(one)
	if bal.Cmp(one) < 0 { amt.Set(bal) } // use min(balance, 1 unit)
	// build call data: transfer(address,uint256)
	data := buildERC20TransferData(to, amt)
	// simulate from 'from'
	out, err := ec.CallContract(ctx, ethereum.CallMsg{From: from, To: &token, Data: data}, nil)
	if err != nil {
		return false, err.Error()
	}
	// Many tokens return no data on success; if they return bool, check !=0
	if len(out) >= 32 {
		z := new(big.Int).SetBytes(out[0:32])
		if z.Sign() == 0 {
			return false, "transfer returned false"
		}
	}
	return true, "ok"
}

// buildERC20TransferData encodes function selector + args for transfer(address,uint256).
func buildERC20TransferData(to common.Address, amount *big.Int) []byte {
	// 0xa9059cbb = keccak("transfer(address,uint256)")[0:4]
	selector := []byte{0xa9, 0x05, 0x9c, 0xbb}
	// 32-byte padded address and amount
	addrPad := make([]byte, 32)
	copy(addrPad[12:], to.Bytes())
	amtPad := make([]byte, 32)
	if amount != nil {
		b := amount.Bytes()
		copy(amtPad[32-len(b):], b)
	}
	return append(append(selector, addrPad...), amtPad...)
}

func max(a, b int) int {
	if a>b { return a }
	return b
}

// --- retry helpers for import checks ---
func guardChecksRetry(ec *ethclient.Client, token, from, to common.Address) (bool, string, string) {
	var ok bool
	var short, detail string
	backoff := []time.Duration{300 * time.Millisecond, 700 * time.Millisecond, 1200 * time.Millisecond}
	for i := 0; i < len(backoff); i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		ok, short, detail = guardChecks(ctx, ec, token, from, to)
		cancel()
		// окончательные результаты — не ретраем
		if ok || short == "no code" || strings.HasPrefix(short, "paused") || strings.HasPrefix(short, "blacklisted") {
			return ok, short, detail
		}
		time.Sleep(backoff[i])
	}
	return ok, short, detail
}

func checkRestrictionsRetry(ec *ethclient.Client, token, from, to common.Address) (string, bool) {
	var sum string
	var lastErr error
	backoff := []time.Duration{300 * time.Millisecond, 700 * time.Millisecond, 1200 * time.Millisecond}
	for i := 0; i < len(backoff); i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		if restr, err := core.CheckRestrictions(ctx, ec, token, from, to); err == nil {
			cancel()
			return restr.Summary(), restr.Blocked()
		} else {
			lastErr = err
		}
		cancel()
		time.Sleep(backoff[i])
	}
	if lastErr != nil {
		sum = "error: " + lastErr.Error()
	}
	return sum, false
}

func preflightSimpleRetry(ec *ethclient.Client, token, from, to common.Address, dec int, balanceWeiStr string) (bool, string) {
	var ok bool
	var why string
	backoff := []time.Duration{300 * time.Millisecond, 700 * time.Millisecond, 1200 * time.Millisecond}
	for i := 0; i < len(backoff); i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		ok, why = preflightSimple(ctx, ec, token, from, to, dec, balanceWeiStr)
		cancel()
		// окончательные результаты — не ретраем
		if ok || strings.EqualFold(why, "zero balance") || strings.Contains(why, "transfer returned false") {
			return ok, why
		}
		time.Sleep(backoff[i])
	}
	return ok, why
}

// --- GUARDS ---
// guardChecks performs lightweight token checks:
//  1) contract code present (EOA check)
//  2) paused() == false   (if function exists)
//  3) not blacklisted: isBlacklisted/isBlackListed(address) for from/to (if present)
// Returns: ok, short, details
func guardChecks(ctx context.Context, ec *ethclient.Client, token, from, to common.Address) (bool, string, string) {
	// 1) Code present
	code, err := ec.CodeAt(ctx, token, nil)
	if err != nil {
		return false, "code error", "codeAt error: " + err.Error()
	}
	if len(code) == 0 {
		return false, "no code", "no bytecode at token address"
	}
	// selector helper
	selector := func(sig string) []byte {
		h := crypto.Keccak256([]byte(sig))
		return h[:4]
	}
	// no-arg bool call (e.g., paused())
	callBool0 := func(sel []byte) (bool, bool, error) {
		out, err := ec.CallContract(ctx, ethereum.CallMsg{To: &token, Data: sel}, nil)
		if err != nil || len(out) == 0 {
			return false, false, err
		}
		if len(out) >= 32 && new(big.Int).SetBytes(out[:32]).Sign() != 0 {
			return true, true, nil
		}
		return true, false, nil
	}
	// bool f(address) call
	callBool1 := func(sel []byte, addr common.Address) (bool, bool, error) {
		arg := make([]byte, 32)
		copy(arg[12:], addr.Bytes())
		data := append(sel, arg...)
		out, err := ec.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
		if err != nil || len(out) == 0 {
			return false, false, err
		}
		if len(out) >= 32 && new(big.Int).SetBytes(out[:32]).Sign() != 0 {
			return true, true, nil
		}
		return true, false, nil
	}
	var details []string
	// 2) paused()
	if ok, val, _ := callBool0(selector("paused()")); ok {
		if val {
			return false, "paused", "paused() == true"
		}
		details = append(details, "paused=false")
	} else {
		details = append(details, "paused=n/a")
	}
	// 3) blacklist checks
	type blq struct{ name string; sel []byte }
	queries := []blq{
		{"isBlacklisted(address)", selector("isBlacklisted(address)")},
		{"isBlackListed(address)", selector("isBlackListed(address)")},
	}
	for _, q := range queries {
		if ok, val, _ := callBool1(q.sel, from); ok {
			if val { return false, "blacklisted(from)", q.name + " FROM == true" }
			details = append(details, "bl(from)=false")
		}
		if ok, val, _ := callBool1(q.sel, to); ok {
			if val { return false, "blacklisted(to)", q.name + " TO == true" }
			details = append(details, "bl(to)=false")
		}
	}
	return true, "ok", strings.Join(details, ", ")
}

func defaultStr(v, d string) string { if strings.TrimSpace(v)=="" { return d }; return v }
