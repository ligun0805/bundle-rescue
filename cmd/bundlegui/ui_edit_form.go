package main

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/ethereum/go-ethereum/common"
)

// buildEditForm builds a small editor for a pairRow. It updates pr in-place.
func buildEditForm(pr *pairRow, onSave func()) fyne.CanvasObject {
	tokenE := widget.NewEntry();     tokenE.SetText(strings.TrimSpace(pr.Token))
	fromE  := widget.NewEntry();     fromE.SetText(strings.TrimSpace(pr.From))
	toE    := widget.NewEntry();     toE.SetText(strings.TrimSpace(pr.To))
	amtTok := widget.NewEntry();     amtTok.SetText(strings.TrimSpace(pr.AmountTokens))
	decE   := widget.NewEntry();     decE.SetText(fmt.Sprintf("%d", pr.Decimals))

	saveBtn := widget.NewButtonWithIcon("Save", theme.ConfirmIcon(), func() {
		token := strings.TrimSpace(tokenE.Text)
		from  := strings.TrimSpace(fromE.Text)
		to    := strings.TrimSpace(toE.Text)
		dec   := strings.TrimSpace(decE.Text)
		if !common.IsHexAddress(token) || !common.IsHexAddress(from) || !common.IsHexAddress(to) {
			dialog.ShowInformation("Edit", "Invalid address field(s)", viewWin)
			return
		}
		decimals := pr.Decimals
		if d, err := strconv.Atoi(dec); err == nil && d >= 0 && d <= 36 {
			decimals = d
		} else {
			dialog.ShowInformation("Edit", "Bad decimals", viewWin); return
		}
		amountTokens := strings.TrimSpace(strings.ReplaceAll(amtTok.Text, ",", "."))
		var amountWei *big.Int
		if amountTokens == "" {
			amountWei = new(big.Int).SetInt64(0)
		} else {
			w, err := toWeiFromTokens(amountTokens, decimals)
			if err != nil { dialog.ShowInformation("Edit", "Bad amount: "+err.Error(), viewWin); return }
			amountWei = w
		}
		// Commit changes
		pr.Token = token
		pr.From = from
		pr.To = to
		pr.Decimals = decimals
		pr.AmountTokens = amountTokens
		pr.AmountWei = amountWei.String()
		if onSave != nil { onSave() }
		// Close overlay if visible
		if viewWin != nil {
			if top := viewWin.Canvas().Overlays().Top(); top != nil {
				top.Hide()
			}
		}
	})
	cancelBtn := widget.NewButton("Cancel", func() {
		if viewWin != nil {
			if top := viewWin.Canvas().Overlays().Top(); top != nil { top.Hide() }
		}
	})
	form := widget.NewForm(
		widget.NewFormItem("Token", tokenE),
		widget.NewFormItem("From",  fromE),
		widget.NewFormItem("To",    toE),
		widget.NewFormItem("Amount (tokens)", amtTok),
		widget.NewFormItem("Decimals", decE),
		widget.NewFormItem("", container.NewHBox(saveBtn, cancelBtn)),
	)
	return container.NewPadded(form)
}