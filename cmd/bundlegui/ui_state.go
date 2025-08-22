package main

import (
	"context"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

// Shared UI state (kept as globals to preserve original behavior).
var (
	runCtx    context.Context
	runCancel context.CancelFunc

	viewWin fyne.Window
	logWin  fyne.Window

	logBox    *widget.Entry
	logProg   *widget.ProgressBar
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
