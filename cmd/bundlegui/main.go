package main

import (
	"fmt"
	"image/color"
	"os"
	"strings"
	"time"

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

	globalsCard := widget.NewCard("Globals", "", widget.NewForm(
		widget.NewFormItem("RPC URL", rpcEntry),
		widget.NewFormItem("Chain ID", chainEntry),
		widget.NewFormItem("Relays", relaysEntry),
		widget.NewFormItem("Auth PK", authPkEntry),
		widget.NewFormItem("Safe PK", safePkEntry),
		widget.NewFormItem("", container.NewGridWithColumns(3, useEnvGlobals, themeSelect, compactCheck)),
	))

	strategyCard := widget.NewCard("Strategy", "", widget.NewForm(
		widget.NewFormItem("Blocks", blocks),
		widget.NewFormItem("Tip (gwei)", tip),
		widget.NewFormItem("Tip ×", tipMul),
		widget.NewFormItem("BaseFee ×", baseMul),
		widget.NewFormItem("Buffer %", buffer),
	))

	addBtn := widget.NewButtonWithIcon("ADD PAIR", theme.ContentAddIcon(), func(){ openAddPairWindow(a, rpcEntry.Text, safePkEntry.Text) })
	importBtn := widget.NewButtonWithIcon("IMPORT PAIRS", theme.FolderOpenIcon(), func(){
		dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error){
			if err!=nil || rc==nil { return }
			defer rc.Close()
			ext := strings.ToLower(rc.URI().Extension())
			var ps []pairRow
			if ext==".csv" {
				if arr, e := parseCSVAll(rc); e==nil { ps = arr }
			} else if ext==".json" {
				if arr, e := parseJSONAll(rc); e==nil { ps = arr }
			} else {
				dialog.ShowInformation("Import", "Only CSV or JSON", w); return
			}
			pairs = append(pairs, ps...)
			statsAdded += len(ps)
			saveQueueToFile()
		}, w).Show()
	})
	viewBtn := widget.NewButtonWithIcon("VIEW PAIRS", theme.DocumentIcon(), func(){ openViewPairsWindow(a, rpcEntry.Text) })
	logBtn := widget.NewButtonWithIcon("LOGS", theme.DocumentIcon(), func(){ ensureLogWindow(a).Show() })
	saveQ := widget.NewButtonWithIcon("SAVE QUEUE", theme.DocumentSaveIcon(), func(){ saveQueueToFile(); dialog.ShowInformation("Save", "Queue saved", w) })
	loadQ := widget.NewButtonWithIcon("LOAD QUEUE", theme.FolderOpenIcon(), func(){ loadQueueFromFile(); dialog.ShowInformation("Load", "Queue loaded", w) })

	buttons := container.NewGridWithColumns(6, addBtn, importBtn, viewBtn, logBtn, saveQ, loadQ)

	statsAddedLbl := widget.NewLabel("Added: 0"); statsSimLbl := widget.NewLabel("Simulated: 0"); statsResLbl := widget.NewLabel("Rescued: 0")
	go func(){
		for range time.NewTicker(500*time.Millisecond).C {
			statsAddedLbl.SetText(fmt.Sprintf("Added: %d", statsAdded))
			statsSimLbl.SetText(fmt.Sprintf("Simulated: %d", statsSimulated))
			statsResLbl.SetText(fmt.Sprintf("Rescued: %d", statsRescued))
		}
	}()
	statsCard := widget.NewCard("Session Stats", "", container.NewGridWithColumns(3, statsAddedLbl, statsSimLbl, statsResLbl))

    simBtn := widget.NewButtonWithIcon("SIMULATE", theme.MediaPlayIcon(), func(){
        go runAll(a, true,
            rpcEntry.Text, chainEntry.Text, relaysEntry.Text,
            authPkEntry.Text, safePkEntry.Text,
            blocks.Text, tip.Text, tipMul.Text, baseMul.Text, buffer.Text,
        )
    })
    resBtn := widget.NewButtonWithIcon("RESCUE",   theme.ConfirmIcon(),   func(){
        go runAll(a, false,
            rpcEntry.Text, chainEntry.Text, relaysEntry.Text,
            authPkEntry.Text, safePkEntry.Text,
            blocks.Text, tip.Text, tipMul.Text, baseMul.Text, buffer.Text,
        )
    })
    stopBtn := widget.NewButtonWithIcon("STOP",    theme.MediaStopIcon(), func(){ if runCancel!=nil { runCancel() } })
    runRow := container.NewGridWithColumns(3, simBtn, resBtn, stopBtn)

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
