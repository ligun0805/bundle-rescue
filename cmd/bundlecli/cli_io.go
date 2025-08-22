package main

import (
	"bufio"
	"fmt"
	"strings"
	"syscall"

	"golang.org/x/term"
)

func readLine(r *bufio.Reader, prompt string) string {
	fmt.Print(prompt)
	t, _ := r.ReadString('\n')
	return strings.TrimSpace(t)
}

func readPassword(prompt string) string {
	fmt.Print(prompt)
	b, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil { die("failed to read password: "+err.Error()) }
	return strings.TrimSpace(string(b))
}

func yes(s string) bool { return s=="y" || s=="yes" || s=="д" || s=="да" }
func maskHex(h string) string { h=strings.TrimSpace(h); if len(h)<=10 { return "***" }; return h[:6]+"…"+h[len(h)-4:] }
func truncate(s string, n int) string { if len(s)<=n { return s }; return s[:n] + "…(truncated)" }
