package main

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

var (
	flatBackground = color.NRGBA{R: 49, G: 51, B: 56, A: 255}
	flatSidebar    = color.NRGBA{R: 43, G: 45, B: 49, A: 255}
	flatRail       = color.NRGBA{R: 30, G: 31, B: 34, A: 255}
	flatSelected   = color.NRGBA{R: 64, G: 66, B: 73, A: 255}
	flatAccent     = color.NRGBA{R: 88, G: 101, B: 242, A: 255}
	flatText       = color.NRGBA{R: 242, G: 243, B: 245, A: 255}
	flatMuted      = color.NRGBA{R: 174, G: 179, B: 189, A: 255}
)

type flatTheme struct{ fyne.Theme }

func newFlatTheme() fyne.Theme { return &flatTheme{Theme: theme.DarkTheme()} }

func (t *flatTheme) Color(name fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameBackground:
		return flatBackground
	case theme.ColorNameButton, theme.ColorNameHeaderBackground:
		return flatSelected
	case theme.ColorNamePrimary, theme.ColorNameHyperlink:
		return flatAccent
	case theme.ColorNameForeground:
		return flatText
	case theme.ColorNameForegroundOnPrimary, theme.ColorNameForegroundOnError, theme.ColorNameForegroundOnSuccess:
		return color.White
	case theme.ColorNameInputBackground, theme.ColorNameMenuBackground, theme.ColorNameOverlayBackground:
		return flatRail
	case theme.ColorNameInputBorder, theme.ColorNameShadow:
		return color.Transparent
	case theme.ColorNamePlaceHolder:
		return flatMuted
	case theme.ColorNameDisabled:
		return flatMuted
	case theme.ColorNameDisabledButton:
		return flatSidebar
	case theme.ColorNameHover:
		return color.NRGBA{R: 255, G: 255, B: 255, A: 18}
	case theme.ColorNamePressed, theme.ColorNameFocus:
		return color.NRGBA{R: 88, G: 101, B: 242, A: 75}
	case theme.ColorNameSelection:
		return flatSelected
	case theme.ColorNameSeparator:
		return color.NRGBA{R: 38, G: 40, B: 44, A: 255}
	case theme.ColorNameScrollBar:
		return flatRail
	case theme.ColorNameScrollBarBackground:
		return color.Transparent
	case theme.ColorNameSuccess:
		return color.NRGBA{R: 35, G: 165, B: 90, A: 255}
	case theme.ColorNameError:
		return color.NRGBA{R: 218, G: 55, B: 60, A: 255}
	}
	return t.Theme.Color(name, theme.VariantDark)
}

func (t *flatTheme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNameText:
		return 14
	case theme.SizeNamePadding:
		return 6
	case theme.SizeNameInnerPadding:
		return 8
	case theme.SizeNameCaptionText:
		return 12
	case theme.SizeNameInputRadius, theme.SizeNameButtonRadius, theme.SizeNameSelectionRadius:
		return 6
	case theme.SizeNameScrollBar:
		return 8
	case theme.SizeNameInputBorder:
		return 1
	}
	return t.Theme.Size(name)
}

// The navigation uses a quiet selected row while action buttons keep the accent.
type navigationTheme struct{ fyne.Theme }

func (t navigationTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	if name == theme.ColorNamePrimary {
		return flatSelected
	}
	return t.Theme.Color(name, variant)
}
