package main

import (
	"fmt"
	"image/color"
	"sort"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// openViewPairsWindow shows the pairs table with filter/sort controls.
func openViewPairsWindow(a fyne.App, rpc string) {
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
		viewSort = widget.NewSelect([]string{"Token","From","To","Amount","Decimals"}, func(string){})
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
			if id.Row%2 == 0 { bg.FillColor = color.NRGBA{R:22,G:26,B:34,A:255} } else { bg.FillColor = color.NRGBA{R:16,G:20,B:28,A:255} }
			switch id.Col {
			case colToken:
				padAct.Hide(); padLbl.Show(); lbl.Alignment = fyne.TextAlignLeading; lbl.SetText(pr.Token)
			case colFrom:
				padAct.Hide(); padLbl.Show(); lbl.Alignment = fyne.TextAlignLeading; lbl.SetText(shortAddr(pr.From))
			case colTo:
				padAct.Hide(); padLbl.Show(); lbl.Alignment = fyne.TextAlignLeading; lbl.SetText(shortAddr(pr.To))
			case colAmtTok:
				padAct.Hide(); padLbl.Show(); lbl.Alignment = fyne.TextAlignTrailing; lbl.SetText(pr.AmountTokens)
			case colAmtWei:
				padAct.Hide(); padLbl.Show(); lbl.Alignment = fyne.TextAlignTrailing; lbl.SetText(pr.AmountWei)
			case colDec:
				padAct.Hide(); padLbl.Show(); lbl.Alignment = fyne.TextAlignCenter;  lbl.SetText(fmt.Sprintf("%d", pr.Decimals))
			case colActions:
				padLbl.Hide(); padAct.Show()
				row := viewIdx[id.Row]
				editBtn.OnTapped = func() {
					form := buildEditForm(&pairs[row], func(){
						saveQueueToFile()
						rebuildViewIdx()
						table.Refresh()
					})
					fyne.CurrentApp().SendNotification(&fyne.Notification{Title:"Edit", Content:"Row editor opened"})
					viewWin.Canvas().Overlays().Show(container.NewPadded(form))
				}
				delBtn.OnTapped = func() {
					dialog := widget.NewPopUp(container.NewPadded(widget.NewLabel("Removing row…")), viewWin.Canvas())
					pairs = append(pairs[:row], pairs[row+1:]...)
					saveQueueToFile()
					rebuildViewIdx()
					table.Refresh()
					dialog.Hide()
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

// rebuildViewIdx rebuilds filtered/sorted indices.
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

// shortAddr formats a hex address to a short preview.
func shortAddr(s string) string {
	if len(s) <= 16 { return s }
	return s[:10] + "…" + s[len(s)-6:]
}
