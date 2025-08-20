package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum"	
	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"
)

func isRPCTimeout(err error) bool {
	if err == nil { return false }
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "deadline exceeded") ||
		strings.Contains(s, "timeout") ||
		strings.Contains(s, "timed out") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "context canceled")
}


var (
	runCtx context.Context
	runCancel context.CancelFunc
	viewWin fyne.Window
	logWin  fyne.Window
	logBox  *widget.Entry
	logProg *widget.ProgressBar
	logProgLbl *widget.Label
	logScroll *container.Scroll

	viewFilter *widget.Entry
	viewSort   *widget.Select
	viewAsc    *widget.Check
	viewIdx    []int

	statsAdded     int
	statsSimulated int
	statsRescued   int

	pairs []pairRow
	table *widget.Table
	addWinsMu sync.Mutex
	addWins   []fyne.Window	
)

func ensureLogWindow(a fyne.App) fyne.Window {
	if logWin != nil { return logWin }
	logWin = a.NewWindow("Logs")	
	logWin.SetOnClosed(func(){ logWin = nil })
	logProg = widget.NewProgressBar()
	logProgLbl = widget.NewLabel("")
	exportBtn := widget.NewButtonWithIcon("Export Telemetry JSON", theme.DocumentSaveIcon(), func(){
		saveTelemetryJSON(logWin)
	})
	top := container.NewBorder(nil, nil, nil, exportBtn, container.NewHBox(widget.NewLabel("Progress:"), logProg, logProgLbl))
	bg := canvas.NewLinearGradient(color.NRGBA{12,16,24,255}, color.NRGBA{20,28,40,255}, 90)
	logBox = widget.NewMultiLineEntry()
    logBox.Disable()
    logBox.Wrapping = fyne.TextWrapWord
    logScroll = container.NewVScroll(logBox)
    logScroll.SetMinSize(fyne.NewSize(800, 180))
    logWin.SetContent(container.NewBorder(top, nil, nil, nil, container.NewMax(bg, logScroll)))
	logWin.Resize(fyne.NewSize(1000, 700))
	return logWin
}
func appendLogLine(a fyne.App, s string) {
    w := ensureLogWindow(a)
    logBox.SetText(logBox.Text + time.Now().Format("15:04:05 ") + s + "\n")
    if logScroll != nil { logScroll.ScrollToBottom() }
    w.Canvas().Refresh(logBox)
}

func saveTelemetryJSON(w fyne.Window) {
	ts := time.Now().Format("20060102_150405")
	exe, _ := os.Executable()
	base := filepath.Dir(exe)
	dir := filepath.Join(base, "log_data")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, ts+".json")
	out := map[string]any{
		"generatedAt": time.Now().UTC().Format(time.RFC3339),
		"telemetry":   telemetry,
	}
	f, err := os.Create(path)
	if err != nil {
		dialog.ShowError(fmt.Errorf("save telemetry: %w", err), w); return
	}
	defer f.Close()
	enc := json.NewEncoder(f); enc.SetIndent("", "  "); _ = enc.Encode(out)
	dialog.ShowInformation("Saved", "Telemetry JSON saved to:\n"+path, w)
}

func openViewPairsWindow(a fyne.App, rpc string) {
    defer func() {
        if r := recover(); r != nil {
            w := viewWin
            if w == nil { w = ensureLogWindow(a) }
            dialog.ShowError(fmt.Errorf("%v", r), w)
        }
    }()
    if viewWin == nil {
        viewWin = a.NewWindow("View Pairs")
        viewWin.SetOnClosed(func(){ viewWin = nil })
    }
    if len(pairs) == 0 {
        viewWin.SetContent(container.NewCenter(widget.NewLabel("No pairs loaded yet")))
        viewWin.Resize(fyne.NewSize(720, 480))
        viewWin.Show()
        return
    }
    const (
        colToken = iota
        colFrom
        colTo
        colAmtTok
        colAmtWei
        colDec
        colActions
        colCount
    )
	
    const (
        wToken   = 140
        wFrom    = 190
        wTo      = 190
        wAmtTok  = 120
        wAmtWei  = 180
        wDec     = 60
        wActions = 172
    )


    if viewFilter == nil { viewFilter = widget.NewEntry() }
    viewFilter.SetPlaceHolder("Filter by token/from/to…")
    if viewSort == nil {
        viewSort = widget.NewSelect([]string{"Token","From","To","Amount","Decimals"}, func(string) {})
        viewSort.SetSelected("Token")
    }
    if viewAsc == nil { viewAsc = widget.NewCheck("Asc", func(bool){}) }
    viewAsc.SetChecked(true)

    makeHeadCell := func(text string, w int, align fyne.TextAlign) fyne.CanvasObject {
        r := canvas.NewRectangle(color.NRGBA{R:32,G:40,B:52,A:255}); r.SetMinSize(fyne.NewSize(float32(w), 34))
        lbl := widget.NewLabelWithStyle(text, align, fyne.TextStyle{Bold:true})
        return container.NewMax(r, container.NewPadded(lbl))
    }
    headerWrap := container.NewHBox(
        makeHeadCell("Token",   wToken,   fyne.TextAlignLeading),
        makeHeadCell("From",    wFrom,    fyne.TextAlignLeading),
        makeHeadCell("To",      wTo,      fyne.TextAlignLeading),
        makeHeadCell("Amount (tokens)", wAmtTok, fyne.TextAlignTrailing),
        makeHeadCell("Wei",     wAmtWei,  fyne.TextAlignTrailing),
        makeHeadCell("Dec",     wDec,     fyne.TextAlignCenter),
        makeHeadCell("Actions", wActions, fyne.TextAlignCenter),
    )

    rebuildViewIdx()
    onChange := func() {
        rebuildViewIdx()
        if table != nil { table.Refresh() }
    }
    viewFilter.OnChanged = func(string){ onChange() }
    viewSort.OnChanged   = func(string){ onChange() }
    viewAsc.OnChanged    = func(bool){ onChange() }

    table = widget.NewTable(
        func() (int, int) { return len(viewIdx), colCount },
        func() fyne.CanvasObject {
            bg := canvas.NewRectangle(color.Transparent)
            bg.SetMinSize(fyne.NewSize(0, 28))
            lbl := widget.NewLabel("")
            lbl.Truncation = fyne.TextTruncateEllipsis
            lbl.Wrapping   = fyne.TextWrapOff
            editBtn := widget.NewButton("Edit", nil);  editBtn.Importance = widget.LowImportance
            delBtn  := widget.NewButton("Delete", nil); delBtn.Importance  = widget.LowImportance
            actions := container.NewHBox(editBtn, delBtn)
            return container.NewMax(bg, container.NewPadded(lbl), container.NewPadded(actions))
        },
        func(id widget.TableCellID, obj fyne.CanvasObject) {
            if id.Row < 0 || id.Row >= len(viewIdx) { return }
            pr := pairs[viewIdx[id.Row]]

            c := obj.(*fyne.Container)
            bg := c.Objects[0].(*canvas.Rectangle)
            padLbl := c.Objects[1].(*fyne.Container)
            lbl := padLbl.Objects[0].(*widget.Label)
            padAct := c.Objects[2].(*fyne.Container)
            actBox := padAct.Objects[0].(*fyne.Container)
            editBtn := actBox.Objects[0].(*widget.Button)
            delBtn  := actBox.Objects[1].(*widget.Button)

            // Зебра
            if id.Row%2 == 0 { bg.FillColor = color.NRGBA{R:22,G:26,B:34,A:255} } else { bg.FillColor = color.NRGBA{R:16,G:20,B:28,A:255} }

            switch id.Col {
            case colToken:
                padAct.Hide(); padLbl.Show(); padLbl.Refresh(); padAct.Refresh()
                lbl.Alignment = fyne.TextAlignLeading
                lbl.SetText(pr.Token)
            case colFrom:
                padAct.Hide(); padLbl.Show(); padLbl.Refresh(); padAct.Refresh()
                lbl.Alignment = fyne.TextAlignLeading
                lbl.SetText(shortAddr(pr.From))
            case colTo:
                padAct.Hide(); padLbl.Show(); padLbl.Refresh(); padAct.Refresh()
                lbl.Alignment = fyne.TextAlignLeading
                lbl.SetText(shortAddr(pr.To))
            case colAmtTok:
                padAct.Hide(); padLbl.Show(); padLbl.Refresh(); padAct.Refresh()
                lbl.Alignment = fyne.TextAlignTrailing
                lbl.SetText(pr.AmountTokens)
            case colAmtWei:
                padAct.Hide(); padLbl.Show(); padLbl.Refresh(); padAct.Refresh()
                lbl.Alignment = fyne.TextAlignTrailing
                lbl.SetText(pr.AmountWei)
            case colDec:
                padAct.Hide(); padLbl.Show(); padLbl.Refresh(); padAct.Refresh()
                lbl.Alignment = fyne.TextAlignCenter
                lbl.SetText(fmt.Sprintf("%d", pr.Decimals))
            case colActions:
                padLbl.Hide(); padAct.Show(); padLbl.Refresh(); padAct.Refresh()
                row := viewIdx[id.Row]
                editBtn.OnTapped = func() {
                    form := buildEditForm(&pairs[row], func(){
                        saveQueueToFile()
                        rebuildViewIdx()
                        table.Refresh()
                    })
                    dialog.ShowCustom("Edit Pair", "Close", form, viewWin)
                }
                delBtn.OnTapped = func() {
                    dialog.ShowConfirm("Delete Pair", "Remove this row from the queue?", func(ok bool){
                        if !ok { return }
                        pairs = append(pairs[:row], pairs[row+1:]...)
                        saveQueueToFile()
                        rebuildViewIdx()
                        table.Refresh()
                    }, viewWin)
                }
            }
            bg.Refresh(); c.Refresh()
        },
    )
    table.SetColumnWidth(colToken,   wToken)
    table.SetColumnWidth(colFrom,    wFrom)
    table.SetColumnWidth(colTo,      wTo)
    table.SetColumnWidth(colAmtTok,  wAmtTok)
    table.SetColumnWidth(colAmtWei,  wAmtWei)
    table.SetColumnWidth(colDec,     wDec)
    table.SetColumnWidth(colActions, wActions)

    filterWrap := func(obj fyne.CanvasObject, w int) fyne.CanvasObject {
        r := canvas.NewRectangle(color.Transparent); r.SetMinSize(fyne.NewSize(float32(w), 36))
        return container.NewMax(r, container.NewPadded(obj))
    }
    controls := container.NewHBox(
        filterWrap(viewFilter, wToken+wFrom+wTo),
        filterWrap(viewSort,   wAmtTok),
        filterWrap(viewAsc,    wAmtWei),
        filterWrap(widget.NewLabel(""), wDec+wActions),
    )
    top := container.NewVBox(headerWrap, controls)

    bg := canvas.NewLinearGradient(color.NRGBA{12,16,24,255}, color.NRGBA{20,28,40,255}, 90)
    body := container.NewMax(bg, table)
    viewWin.SetContent(container.NewBorder(top, nil, nil, nil, body))
    viewWin.Resize(fyne.NewSize(980, 620))
    viewWin.Show()
}

func shortAddr(s string) string {
    if len(s) <= 16 { return s }
    return s[:10] + "…" + s[len(s)-6:]
}


func rebuildViewIdx() {
	q := strings.ToLower(strings.TrimSpace(viewFilter.Text))
	viewIdx = viewIdx[:0]
	for i, p := range pairs {
		if q == "" || strings.Contains(strings.ToLower(p.Token), q) || strings.Contains(strings.ToLower(p.From), q) || strings.Contains(strings.ToLower(p.To), q) {
			viewIdx = append(viewIdx, i)
		}
	}
	key := viewSort.Selected
	asc := viewAsc.Checked
	sort.SliceStable(viewIdx, func(i, j int) bool {
		a := pairs[viewIdx[i]]
		b := pairs[viewIdx[j]]
		var less bool
		switch key {
		case "From":
			less = strings.ToLower(a.From) < strings.ToLower(b.From)
		case "To":
			less = strings.ToLower(a.To) < strings.ToLower(b.To)
		case "Amount":
			less = strings.TrimLeft(a.AmountWei, "0") < strings.TrimLeft(b.AmountWei, "0")
		case "Decimals":
			less = a.Decimals < b.Decimals
		default:
			less = strings.ToLower(a.Token) < strings.ToLower(b.Token)
		}
		if asc { return less }
		return !less
	})
}

func openAddPairWindow(a fyne.App, rpc string, safePk string) {
	win := a.NewWindow("Add Pair")
	addWinsMu.Lock()
	addWins = append(addWins, win)
	addWinsMu.Unlock()
	win.SetOnClosed(func(){
		addWinsMu.Lock()
		for i,w2 := range addWins {
			if w2 == win {
				addWins = append(addWins[:i], addWins[i+1:]...)
				break
			}
		}
		addWinsMu.Unlock()
	})
	tokenE := widget.NewEntry()
	fromE := widget.NewEntry()
	fromPkE := widget.NewPasswordEntry()
	toE := widget.NewEntry()
	amountTokE := widget.NewEntry()
	if s := strings.TrimSpace(safePk); s != "" {
		if addr, err := deriveAddrFromPK(s); err == nil {
			toE.SetText(addr)
		}
	}
	decE := widget.NewEntry()
	status := widget.NewLabel("")
	spinner := widget.NewProgressBarInfinite()
	spinner.Hide()
	status.Wrapping = fyne.TextWrapWord
	statusCard := widget.NewCard("Status", "", container.NewVBox(status, spinner))

	fromPreview := widget.NewLabel("")
	fromPkE.OnChanged = func(s string){
		s = strings.TrimSpace(s)
		if s == "" {
			fromPreview.SetText("from: <empty privkey>")
			fromPreview.TextStyle = fyne.TextStyle{Bold:true}
			fromE.SetText("")
			return
		}
		addr, err := deriveAddrFromPK(s)
		if err != nil {
			fromPreview.SetText("from: <invalid privkey>")
			fromPreview.TextStyle = fyne.TextStyle{Bold:true}
			fromE.SetText("")
			return
		}
		fromPreview.TextStyle = fyne.TextStyle{}
		fromPreview.SetText("from: " + addr)
		fromE.SetText(addr)
	}

	saveBtn := widget.NewButtonWithIcon("SAVE", theme.DocumentSaveIcon(), func(){
		spinner.Show(); status.SetText("Saving…")
		token := strings.TrimSpace(tokenE.Text)
		fromPk := strings.TrimSpace(fromPkE.Text)
		to := strings.TrimSpace(toE.Text)
		if fromPk == "" { status.SetText("Enter From PK"); spinner.Hide(); return }
		from := strings.TrimSpace(fromE.Text)
		if from == "" {
			if addr, err := deriveAddrFromPK(fromPk); err == nil { from = addr; fromE.SetText(addr) } else { status.SetText("Cannot derive From from PK"); spinner.Hide(); return }
		}
		if to == "" {
			if addr, err := deriveAddrFromPK(strings.TrimSpace(safePk)); err == nil { to = addr; toE.SetText(addr) }
		}
		if token == "" || !common.IsHexAddress(token) { status.SetText("Token address invalid"); spinner.Hide(); return }
		if !common.IsHexAddress(from) { status.SetText("From address invalid"); spinner.Hide(); return }
		if !common.IsHexAddress(to)   { status.SetText("To address invalid"); spinner.Hide(); return }
		ec, err := ethclient.Dial(rpc); if err != nil { status.SetText("RPC dial: "+err.Error()); spinner.Hide(); return }
		dec := atoi(decE.Text, -1)
		if dec < 0 {
			if d, e := fetchTokenDecimals(ec, common.HexToAddress(token)); e == nil { dec = d; decE.SetText(fmt.Sprintf("%d", d)) } else { status.SetText("decimals: "+e.Error()); spinner.Hide(); return }
		}
		amountTok := strings.TrimSpace(strings.ReplaceAll(amountTokE.Text, ",", "."))
		bal, err := fetchTokenBalance(ec, common.HexToAddress(token), common.HexToAddress(from)); if err != nil { status.SetText("balance: "+err.Error()); spinner.Hide(); return }
		var w *big.Int
		if amountTok == "" || strings.EqualFold(amountTok, "all") {
			w = new(big.Int).Set(bal)
			amountTok = formatTokensFromWei(w, dec)
			amountTokE.SetText(amountTok)
		} else {
			if ww, e := toWeiFromTokens(amountTok, dec); e == nil { w = ww } else { status.SetText("amount: "+e.Error()); spinner.Hide(); return }
		}
		if bal.Cmp(w) < 0 { status.SetText("Rejected: balance < amount"); spinner.Hide(); return }
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second); defer cancel()
		if known, paused, _ := core.CheckPaused(ctx, ec, common.HexToAddress(token)); known && paused {
			status.SetText("Rejected: token is PAUSED"); spinner.Hide(); return
		}
		restr, err := core.CheckRestrictions(ctx, ec, common.HexToAddress(token), common.HexToAddress(from), common.HexToAddress(strings.TrimSpace(toE.Text)))
		if err == nil && restr.Blocked() {
			status.SetText("Rejected: " + restr.Summary()); spinner.Hide(); return
		}

		if ok, reason, err := core.PreflightTransfer(ctx, ec, common.HexToAddress(token), common.HexToAddress(from), common.HexToAddress(to), w); !ok {
			if err != nil && isRPCTimeout(err) {
				status.SetText("Preflight: RPC timeout — saving anyway")
			} else {
				status.SetText("Rejected: token not transferable (" + reason + ")"); spinner.Hide(); return
			}
		}
		pairs = append(pairs, pairRow{
			Token: token, From: from, FromPK: fromPk, To: to,
			AmountWei: w.String(), AmountTokens: amountTok, Decimals: dec,
			BalanceWei: bal.String(), BalanceTokens: formatTokensFromWei(bal, dec),
		})
		statsAdded++
		saveQueueToFile()
		if strings.Contains(strings.ToLower(status.Text), "preflight: rpc timeout") {
			status.SetText("Saved to queue ✔ (preflight skipped due to RPC timeout)")
		} else {
			status.SetText("Saved to queue ✔")
		}
		spinner.Hide()
		win.Close()
	})
	cancelBtn := widget.NewButton("Cancel", func(){ win.Close() })

	form := widget.NewForm(
		widget.NewFormItem("Token", tokenE),
		widget.NewFormItem("From", fromE),
		widget.NewFormItem("From PK", fromPkE),
		widget.NewFormItem("To", toE),
		widget.NewFormItem("Amount (tokens)", amountTokE),
		widget.NewFormItem("Decimals", decE),
		widget.NewFormItem("", container.NewHBox(saveBtn, cancelBtn)),
	)
	win.SetContent(container.NewVBox(form, statusCard))
	win.Resize(fyne.NewSize(560, 520))
	win.Show()
}

func closeAddPairWindows() {
	addWinsMu.Lock()
	ws := append([]fyne.Window(nil), addWins...)
	addWins = nil
	addWinsMu.Unlock()
	for _, w := range ws { w.Close() }
}

func tryReadBPSAndTS(ec *ethclient.Client, token common.Address) (ok bool, maxTxBps, maxWalletBps uint64, totalSupply *big.Int) {
	readUint := func(sig string) (*big.Int, error) {
		sel := gethcrypto.Keccak256([]byte(sig))[:4]
		res, err := ec.CallContract(context.Background(), ethereum.CallMsg{ To:&token, Data: sel }, nil)
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


func buildEditForm(p *pairRow, onChange func()) fyne.CanvasObject {
	token := widget.NewEntry(); token.SetText(p.Token)
	from := widget.NewEntry(); from.SetText(p.From)
	fromPk := widget.NewPasswordEntry(); fromPk.SetText(p.FromPK)
	to := widget.NewEntry(); to.SetText(p.To)
	amountTok := widget.NewEntry(); amountTok.SetText(p.AmountTokens)
	dec := widget.NewEntry(); if p.Decimals >= 0 { dec.SetText(fmt.Sprintf("%d", p.Decimals)) }
	status := widget.NewLabel("")
	form := widget.NewForm(
		widget.NewFormItem("Token", token),
		widget.NewFormItem("From", from),
		widget.NewFormItem("From PK", fromPk),
		widget.NewFormItem("To", to),
		widget.NewFormItem("Amount (tokens)", amountTok),
		widget.NewFormItem("Decimals", dec),
		widget.NewFormItem("", status),
	)
	form.SubmitText = "Save"
	form.OnSubmit = func(){
		if token.Text=="" || fromPk.Text=="" || to.Text=="" || amountTok.Text=="" { status.SetText("Fill required fields"); return }
		addr, err := deriveAddrFromPK(fromPk.Text); if err!=nil { status.SetText("Invalid From PK"); return }
		fromAddr := from.Text; if strings.TrimSpace(fromAddr)=="" { fromAddr = addr }
		d := -1; if strings.TrimSpace(dec.Text)!="" { if n,err := strconv.Atoi(dec.Text); err==nil { d = n } }
		if d < 0 { d = 18 }
		amt := strings.ReplaceAll(strings.TrimSpace(amountTok.Text), ",", ".")
		w, err := toWeiFromTokens(amt, d); if err!=nil { status.SetText("amount: "+err.Error()); return }
		p.Token = token.Text; p.From = fromAddr; p.FromPK = fromPk.Text; p.To = to.Text; p.AmountTokens = amountTok.Text; p.Decimals = d; p.AmountWei = w.String()
		onChange()
	}
	return form
}

func simAndSendOne(a fyne.App, pr pairRow, rpc string) {
	appendLogLine(a, "Sim+Send one: token="+short(pr.Token)+" from="+short(pr.From)+" to="+short(pr.To))
}

func runAll(a fyne.App, simOnly bool, rpc, chain, relays, auth, safe, blocksS, tipS, tipMulS, baseMulS, bufferS string) {
    defer func() {
        if r := recover(); r != nil {
            appendLogLine(a, fmt.Sprintf("[panic] %v", r))
        }
    }()
	if len(pairs)==0 { appendLogLine(a, "no pairs"); return }
	ec, err := ethclient.Dial(rpc); if err!=nil { appendLogLine(a, fmt.Sprintf("dial err: %v", err)); return }
	runCtx, runCancel = context.WithCancel(context.Background())
	ctx := runCtx
	total := len(pairs)
    ensureLogWindow(a).Show()
    if logProg != nil {
        logProg.Min = 0
        logProg.Max = float64(total)
        logProg.SetValue(0)
    }
    if logProgLbl != nil {
        logProgLbl.SetText(fmt.Sprintf("0/%d", total))
    }
	for i, pr := range pairs {
		select { case <-ctx.Done(): appendLogLine(a, "STOP pressed — cancelling"); return; default: }
		appendLogLine(a, fmt.Sprintf("=== %s ALL: pair %d/%d ===", map[bool]string{true:"Simulate", false:"Run"}[simOnly], i+1, len(pairs)))
		p := core.Params{
			RPC: rpc, ChainID: mustBig(chain), Relays: strings.Split(relays, ","), AuthPrivHex: auth,
			Token: common.HexToAddress(pr.Token), From: common.HexToAddress(pr.From), To: common.HexToAddress(pr.To),
			AmountWei: mustBig(pr.AmountWei), SafePKHex: safe, FromPKHex: pr.FromPK,
			Blocks: atoi(blocksS, 6), TipGweiBase: atoi64(tipS, 3), TipMul: atof(tipMulS, 1.25), BaseMul: atoi64(baseMulS, 2), BufferPct: atoi64(bufferS, 5),
			SimulateOnly: simOnly, SkipIfPaused: true,
			Logf: func(f string, a2 ...any){ appendLogLine(a, fmt.Sprintf(f, a2...)) },
			OnSimResult: func(relay, raw string, ok bool, err string){
				telAdd(TelemetryItem{ Time: time.Now().UTC().Format(time.RFC3339), Action:"eth_callBundle", PairIndex:i, Relay: relay, OK: ok, Error: err, Raw: raw })
				if simOnly { statsSimulated++ }
			},
		}
		out, err := core.Run(ctx, ec, p)
		if err != nil { appendLogLine(a, "error: "+err.Error()) } else {
			appendLogLine(a, "result: " + out.Reason)
			if out.Included { statsRescued++ }
		}
        if logProg != nil { logProg.SetValue(float64(i+1)) }
        if logProgLbl != nil { logProgLbl.SetText(fmt.Sprintf("%d/%d", i+1, total)) }
	}
	appendLogLine(a, "ALL: completed")
}
