package main

import (
	"bufio"
	"fmt"
	"os"
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
	if err != nil { die("failed to read password: " + err.Error()) }
	return strings.TrimSpace(string(b))
}

func yes(s string) bool { return s=="y" || s=="yes" || s=="д" || s=="да" }
func maskHex(h string) string { h=strings.TrimSpace(h); if len(h)<=10 { return "***" }; return h[:6]+"…"+h[len(h)-4:] }

// AskBundleMode asks user whether to run plain bundle or apply strategy.
// Returns true when user chose to apply a strategy.
func AskBundleMode(r *bufio.Reader) bool {
    fmt.Println("Режим бандла: стратегия EIP-7702 (forced)")
    return true
}

// AskStrategy lets user pick a concrete strategy; return code as string.
func AskStrategy(r *bufio.Reader) string {
    fmt.Println("Strategy: EIP-7702 (forced)")
    return "3"
}

// die prints an error and waits for Enter before exiting.
// This prevents instant console close on Windows double-click runs.
func die(message string) {
	fmt.Fprintln(os.Stderr, "Error:", message)
	fmt.Fprint(os.Stderr, "Exit now? Press Enter to close...")
	_, _ = bufio.NewReader(os.Stdin).ReadBytes('\n')
	os.Exit(1)
}