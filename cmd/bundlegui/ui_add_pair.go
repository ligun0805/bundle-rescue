package main

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"
)

// openAddPairWindow opens the form to add a row into the queue.
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
		if addr, err := deriveAddrFromPK(s); err == nil { toE.SetText(addr) }
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
			fromPreview.SetText("from: <empty privkey>"); fromPreview.TextStyle = fyne.TextStyle{Bold:true}; fromE.SetText(""); return
		}
		addr, err := deriveAddrFromPK(s)
		if err != nil {
			fromPreview.SetText("from: <invalid privkey>"); fromPreview.TextStyle = fyne.TextStyle{Bold:true}; fromE.SetText(""); return
		}
		fromPreview.TextStyle = fyne.TextStyle{}; fromPreview.SetText("from: " + addr); fromE.SetText(addr)
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

// closeAddPairWindows closes all "Add Pair" windows.
func closeAddPairWindows() {
	addWinsMu.Lock()
	ws := append([]fyne.Window(nil), addWins...)
	addWins = nil
	addWinsMu.Unlock()
	for _, w := range ws { w.Close() }
}
