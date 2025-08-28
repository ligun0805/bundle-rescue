package main

import (
	"bufio"
	"fmt"
	"image/color"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/joho/godotenv"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
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

	importBtn := widget.NewButtonWithIcon("IMPORT LIST", theme.FolderOpenIcon(), func(){
		dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error){
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
			pairs = append(pairs, ps...)
			statsAdded += len(ps)
			saveQueueToFile()
		}, w).Show()
	})

	buttons := container.NewGridWithColumns(2, importBtn, widget.NewButton("REMOVE NON-TRANSFERABLE", func(){
		var keep []pairRow
		for _,pr := range pairs {
			if strings.TrimSpace(pr.BalanceWei)!="0" && strings.TrimSpace(pr.BalanceWei)!="" { keep = append(keep, pr) }
		}
		pairs = keep
		saveQueueToFile()
	}))

	statsAddedLbl := widget.NewLabel("Added: 0"); statsSimLbl := widget.NewLabel("Simulated: 0"); statsResLbl := widget.NewLabel("Rescued: 0")
	go func(){
		for range time.NewTicker(500*time.Millisecond).C {
			statsAddedLbl.SetText(fmt.Sprintf("Added: %d", statsAdded))
			statsSimLbl.SetText(fmt.Sprintf("Simulated: %d", statsSimulated))
			statsResLbl.SetText(fmt.Sprintf("Rescued: %d", statsRescued))
		}
	}()
	statsCard := widget.NewCard("Session Stats", "", container.NewGridWithColumns(3, statsAddedLbl, statsSimLbl, statsResLbl))

	resBtn := widget.NewButtonWithIcon("RESCUE",   theme.ConfirmIcon(),   func(){
        go runAll(a, false,
            rpcEntry.Text, chainEntry.Text, relaysEntry.Text,
            authPkEntry.Text, safePkEntry.Text,
            blocks.Text, tip.Text, tipMul.Text, baseMul.Text, buffer.Text,
        )
    })
	runRow := container.NewGridWithColumns(2, widget.NewButton("UPDATE NETWORK", func(){ dialog.ShowInformation("Network", "Updated from RPC", w) }), resBtn)

    content := container.NewVBox(globalsCard, strategyCard, buttons, runRow, statsCard)
    bg := canvas.NewLinearGradient(color.NRGBA{12,16,24,255}, color.NRGBA{20,28,40,255}, 90)
    w.SetContent(
        container.NewMax(
            bg,
            container.NewBorder(nil, nil, nil, nil, container.NewVScroll(content)),
        ),
    )
	w.ShowAndRun()
}

func defaultStr(v, d string) string { if strings.TrimSpace(v)=="" { return d }; return v }
