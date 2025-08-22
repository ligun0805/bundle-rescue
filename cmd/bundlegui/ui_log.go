package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// ensureLogWindow creates or returns the log window.
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

// appendLogLine adds a timestamped line to the log.
func appendLogLine(a fyne.App, s string) {
	w := ensureLogWindow(a)
	logBox.SetText(logBox.Text + time.Now().Format("15:04:05 ") + s + "\n")
	if logScroll != nil { logScroll.ScrollToBottom() }
	w.Canvas().Refresh(logBox)
}

// saveTelemetryJSON writes telemetry to a timestamped JSON file.
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
		fyne.CurrentApp().SendNotification(&fyne.Notification{Title:"Save error", Content:fmt.Sprintf("%v", err)})
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f); enc.SetIndent("", "  "); _ = enc.Encode(out)
	fyne.CurrentApp().SendNotification(&fyne.Notification{Title:"Saved", Content:path})
}
