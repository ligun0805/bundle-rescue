package main

import (
    "image/color"
    "fyne.io/fyne/v2"
    "fyne.io/fyne/v2/theme"
)

type rescueTheme struct{ fyne.Theme }

func (t rescueTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
    switch n {
    case theme.ColorNameForeground:
        if v == theme.VariantDark { return color.NRGBA{240,240,240,255} }
    case theme.ColorNamePlaceHolder:
        if v == theme.VariantDark { return color.NRGBA{200,200,200,255} }
    }
    if t.Theme != nil { return t.Theme.Color(n, v) }
    return theme.DefaultTheme().Color(n, v)
}
