package main

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

type appTheme struct{ mode string; compact bool }

func makeTheme(mode string, compact bool) fyne.Theme { return &appTheme{mode: mode, compact: compact} }

func (t *appTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	isDark := t.mode == "dark" || v == theme.VariantDark

	switch n {
	case theme.ColorNameForeground:
		if isDark {
			return color.NRGBA{255, 255, 255, 255}
		}
		return color.NRGBA{0, 0, 0, 255}

	case theme.ColorNamePlaceHolder, theme.ColorNameDisabled:
		if isDark {
			return color.NRGBA{200, 200, 200, 255}
		}
		return color.NRGBA{90, 90, 90, 255}
	}

	if isDark {
		return theme.DarkTheme().Color(n, v)
	}
	return theme.LightTheme().Color(n, v)
}

func (t *appTheme) Font(style fyne.TextStyle) fyne.Resource {
	if t.mode == "light" { return theme.LightTheme().Font(style) }
	return theme.DarkTheme().Font(style)
}
func (t *appTheme) Icon(n fyne.ThemeIconName) fyne.Resource {
	if t.mode == "light" { return theme.LightTheme().Icon(n) }
	return theme.DarkTheme().Icon(n)
}
func (t *appTheme) Size(n fyne.ThemeSizeName) float32 {
	var base float32
	if t.mode == "light" { base = theme.LightTheme().Size(n) } else { base = theme.DarkTheme().Size(n) }
	if t.compact {
		switch n { case theme.SizeNameText: return base * 0.95; case theme.SizeNamePadding: return base * 0.85 }
	} else {
		switch n { case theme.SizeNameText: return base * 1.05; case theme.SizeNamePadding: return base * 1.10 }
	}
	return base
}
