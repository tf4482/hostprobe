// Shared progress frame: transient spinner and bar for long phases.
// Terminal-only progress for long phases.
package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// Look of the progress line
const (
	progressCells    = 48
	progressInterval = 100 * time.Millisecond
	progressFrames   = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
	cyan             = "36"
	green            = "32"
)

// Live progress line; silent when output is not a terminal
type progress struct {
	out     printer
	label   string
	total   int
	width   int
	mutex   sync.Mutex
	current int
	message string
	frame   int
	stop    chan struct{}
	stopped chan struct{}
}

// Start a bar for a phase of total steps; always pair with done
func (p printer) progress(label string, total int) *progress {
	bar := &progress{out: p, label: label, total: total, width: 80}
	file, ok := p.out.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return bar
	}
	if width, _, err := term.GetSize(int(file.Fd())); err == nil && width > 0 {
		bar.width = width
	}
	bar.stop, bar.stopped = make(chan struct{}), make(chan struct{})
	go bar.animate()
	return bar
}

// Count one finished step; safe to call from goroutines
func (b *progress) step(message string) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.current++
	b.message = message
	b.draw()
}

// Erase the line so normal output can continue
func (b *progress) done() {
	if b.stop == nil {
		return
	}
	close(b.stop)
	<-b.stopped
	fmt.Fprint(b.out.out, "\r\033[K")
}

// Keep the spinner moving while steps take long
func (b *progress) animate() {
	defer close(b.stopped)
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	for {
		b.mutex.Lock()
		b.draw()
		b.frame++
		b.mutex.Unlock()
		select {
		case <-b.stop:
			return
		case <-ticker.C:
		}
	}
}

// Redraw the progress line within the available terminal width
func (b *progress) draw() {
	if b.stop == nil {
		return
	}
	frames := []rune(progressFrames)
	spinner := string(frames[b.frame%len(frames)])
	current := max(0, min(b.current, b.total))
	percent := 100 * current / max(b.total, 1)
	count := fmt.Sprintf("%d/%d", b.current, b.total)
	limit := b.width - 1

	// Prioritize the count over the label on narrow terminals
	if limit < utf8.RuneCountInString(spinner+"    100% · "+count)+6 {
		compact := spinner + " · " + count
		if space := limit - utf8.RuneCountInString(compact) - 1; space > 0 {
			label := []rune(b.label)
			compact = spinner + " " + string(label[:min(len(label), space)]) + " · " + count
		}
		text := string([]rune(compact)[:min(utf8.RuneCountInString(compact), max(0, limit))])
		if b.out.color {
			text = "\033[" + cyan + "m" + text + "\033[0m"
		}
		fmt.Fprint(b.out.out, "\r\033[K"+text)
		return
	}

	// Reserve space for the count before fitting the label and bar
	fixed := utf8.RuneCountInString(spinner + "     100% · " + count)
	label := []rune(b.label)
	label = label[:min(len(label), 20, max(0, limit-fixed-6))]
	barWidth := min(progressCells, limit-fixed-len(label))
	filled := barWidth * current / max(b.total, 1)
	message := ""
	if b.message != "" {
		remaining := limit - fixed - len(label) - barWidth
		if remaining > 3 {
			runes := []rune(b.message)
			message = " · " + string(runes[:min(len(runes), remaining-3)])
		}
	}

	prefix := spinner + " " + string(label) + "  "
	active := strings.Repeat("━", filled)
	inactive := strings.Repeat("─", barWidth-filled)
	suffix := fmt.Sprintf("  %3d%% · %s%s", percent, count, message)
	text := prefix + active + inactive + suffix
	if b.out.color {
		text = "\033[" + cyan + "m" + spinner + "\033[0m" + prefix[len(spinner):] +
			progressTint(current, b.total) + active + "\033[0m" +
			"\033[2m" + inactive + "\033[0m" +
			"  " + progressTint(current, b.total) + fmt.Sprintf("%3d%%", percent) + "\033[0m · " + count + message
	}
	fmt.Fprint(b.out.out, "\r\033[K"+text)
}

// Blend the progress tint from pink through gold to turquoise
func progressTint(current, total int) string {
	percent := 100 * max(0, min(current, total)) / max(total, 1)
	from, to := [3]int{255, 45, 145}, [3]int{255, 190, 65}
	if percent > 50 {
		percent -= 50
		from, to = to, [3]int{45, 220, 195}
	}
	mix := min(percent*2, 100)
	return fmt.Sprintf("\033[38;2;%d;%d;%dm",
		from[0]+(to[0]-from[0])*mix/100,
		from[1]+(to[1]-from[1])*mix/100,
		from[2]+(to[2]-from[2])*mix/100)
}
