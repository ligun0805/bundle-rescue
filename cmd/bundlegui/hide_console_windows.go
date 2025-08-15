//go:build windows

package main

import "syscall"

var (
    modkernel32          = syscall.NewLazyDLL("kernel32.dll")
    procGetConsoleWindow = modkernel32.NewProc("GetConsoleWindow")
    moduser32            = syscall.NewLazyDLL("user32.dll")
    procShowWindow       = moduser32.NewProc("ShowWindow")
)

const SW_HIDE = 0

func hideConsoleWindow() {
    hwnd, _, _ := procGetConsoleWindow.Call()
    if hwnd != 0 {
        procShowWindow.Call(hwnd, SW_HIDE)
    }
}
