package ansi

import (
	"fmt"
	"os"
)

const (
	Reset   = "\033[0m"
	Bold    = "\033[1m"
	Dim     = "\033[2m"
	Red     = "\033[31m"
	Green   = "\033[32m"
	Yellow  = "\033[33m"
	Blue    = "\033[34m"
	Magenta = "\033[35m"
	Cyan    = "\033[36m"
	White   = "\033[37m"
)

func Enabled() bool {
	return os.Getenv("NO_COLOR") == ""
}

func Paint(color, text string) string {
	if !Enabled() {
		return text
	}
	return color + text + Reset
}

func Clear() {
	if Enabled() {
		fmt.Print("\033[2J\033[H")
	}
}

func Banner() {
	fmt.Println(Paint(Cyan+Bold, "╔══════════════════════════════════════════════════════════╗"))
	fmt.Println(Paint(Cyan+Bold, "║                        TITANUS                           ║"))
	fmt.Println(Paint(Cyan+Bold, "║             Native Realm Infrastructure                 ║"))
	fmt.Println(Paint(Cyan+Bold, "╚══════════════════════════════════════════════════════════╝"))
	fmt.Println()
}

func OK(text string)    { fmt.Println(Paint(Green, "✔ ")+text) }
func Warn(text string)  { fmt.Println(Paint(Yellow, "⚠ ")+text) }
func Error(text string) { fmt.Println(Paint(Red, "✖ ")+text) }
func Info(text string)  { fmt.Println(Paint(Cyan, "• ")+text) }
