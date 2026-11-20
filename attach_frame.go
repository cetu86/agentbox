package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const (
	frameMinCols = 20
	frameMinRows = 5

	frameButtonLabel = " Detach (F10) "
	shellButtonLabel = " Shell (F9) "
	agentButtonLabel = " Agent (F9) "

	sgrReset      = "\x1b[0m"
	sgrButton     = "\x1b[1;97;48;5;124m"
	sgrSwitch     = "\x1b[1;97;48;5;25m"
	sgrTitleBar   = "\x1b[97;48;5;238m"
	sgrBorder     = "\x1b[38;5;244m"
	enterFrameUI  = "\x1b[?1049h\x1b[2J\x1b[?1000h\x1b[?1006h\x1b[?2004h"
	leaveFrameUI  = "\x1b[?2004l\x1b[?1006l\x1b[?1003l\x1b[?1002l\x1b[?1000l" + sgrReset + "\x1b[?25h\x1b[?1049l"
	maxOutputTail = 4096

	// modeSeedDelay is how long an attach waits for output before it
	// recovers the application's terminal modes anyway.
	modeSeedDelay  = 400 * time.Millisecond
	modeSeedSettle = 250 * time.Millisecond
)

// framedTerminalSize reports the size of the user's terminal when the framed
// attach UI can be used: stdin and stdout are terminals that are large enough.
func framedTerminalSize() (cols, rows int, ok bool) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return 0, 0, false
	}
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || cols < frameMinCols || rows < frameMinRows {
		return 0, 0, false
	}
	return cols, rows, true
}

// attachSession attaches to the agent of a box in the framed UI. The titlebar
// switches between the agent and a persistent shell container on the same
// snapshot; both keep running while they are not shown.
func (c *Config) attachSession(name string) error {
	return c.attachSessionAt(name, false)
}

// attachSessionAt is attachSession, optionally starting in the shell.
func (c *Config) attachSessionAt(name string, startInShell bool) error {
	cols, rows, ok := framedTerminalSize()
	if !ok {
		return c.runAttachCommand([]string{"attach", c.containerName(name)})
	}
	shell := startInShell
	if shell {
		if err := c.ensureShell(name, c.snapPath(name)); err != nil {
			return err
		}
	}
	for {
		var (
			opts      frameOptions
			container string
		)
		if shell {
			container = c.shellContainerName(name)
			opts = frameOptions{label: name + " (shell)", switchLabel: agentButtonLabel, quietExit: true}
		} else {
			container = c.containerName(name)
			opts = frameOptions{label: name, switchLabel: shellButtonLabel}
		}
		opts.modes = func() string { return c.terminalState(container) }
		outcome, err := runFramedAttach(exec.Command(c.Runtime, "attach", container), opts, cols, rows)
		switch {
		case outcome == outcomeDetached:
			return nil
		case outcome == outcomeSwitch && !shell:
			if err := c.ensureShell(name, c.snapPath(name)); err != nil {
				return err
			}
			shell = true
		case outcome == outcomeSwitch:
			shell = false
		case shell:
			// The shell ended (the user left it); go back to the agent.
			_ = c.removeShell(name)
			shell = false
		default:
			return err
		}
		if !shell {
			status, err := c.containerStatus(name)
			if err != nil {
				return err
			}
			if !containerIsRunning(status) {
				fmt.Fprintf(os.Stderr, "agent container for %q is not running\n", name)
				return nil
			}
		}
		if w, h, ok := framedTerminalSize(); ok {
			cols, rows = w, h
		}
	}
}

type mouseEvent struct {
	button  int
	x, y    int
	release bool
}

func (m mouseEvent) leftPress() bool {
	return !m.release && m.button&(3|32|64|128) == 0
}

type parseStatus int

const (
	parseOK parseStatus = iota
	parseIncomplete
	parseInvalid
)

// parseSGRMouse parses ESC [ < b ; x ; y (M|m) at the start of b.
func parseSGRMouse(b []byte) (mouseEvent, int, parseStatus) {
	const prefix = "\x1b[<"
	if !bytes.HasPrefix(b, []byte(prefix)) {
		return mouseEvent{}, 0, parseInvalid
	}
	var fields [3]int
	field, digits := 0, 0
	for i := len(prefix); i < len(b); i++ {
		switch ch := b[i]; {
		case ch >= '0' && ch <= '9':
			digits++
			if digits > 6 {
				return mouseEvent{}, 0, parseInvalid
			}
			fields[field] = fields[field]*10 + int(ch-'0')
		case ch == ';' && field < 2 && digits > 0:
			field++
			digits = 0
		case (ch == 'M' || ch == 'm') && field == 2 && digits > 0:
			return mouseEvent{button: fields[0], x: fields[1], y: fields[2], release: ch == 'm'}, i + 1, parseOK
		default:
			return mouseEvent{}, 0, parseInvalid
		}
	}
	return mouseEvent{}, 0, parseIncomplete
}

// inputFilter sits between the user's terminal and the container. It removes
// the sequences that belong to the frame (F10, mouse reports on the titlebar)
// and rewrites or drops the ones the container did not ask for.
type inputFilter struct {
	pending []byte
	detach  func()
	// switchView is nil when the frame has no second view; F9 is then
	// forwarded to the container.
	switchView func()
	mouse      func(mouseEvent) []byte
	paste      func() bool
}

var filteredPrefixes = []string{"\x1b[<", "\x1b[200~", "\x1b[201~", "\x1b[21~", "\x1b[20~"}

// feed returns the bytes to forward to the container. Input that may be the
// start of a longer sequence is held back until more data arrives or flush is
// set.
func (f *inputFilter) feed(data []byte, flush bool) []byte {
	buf := append(f.pending, data...)
	f.pending = nil
	var out []byte
	for len(buf) > 0 {
		i := bytes.IndexByte(buf, 0x1b)
		if i < 0 {
			out = append(out, buf...)
			break
		}
		out = append(out, buf[:i]...)
		buf = buf[i:]

		switch {
		case bytes.HasPrefix(buf, []byte("\x1b[<")):
			ev, n, status := parseSGRMouse(buf)
			switch {
			case status == parseOK:
				out = append(out, f.mouse(ev)...)
				buf = buf[n:]
				continue
			case status == parseIncomplete && !flush:
				f.pending = append([]byte(nil), buf...)
				return out
			}
		case bytes.HasPrefix(buf, []byte("\x1b[200~")), bytes.HasPrefix(buf, []byte("\x1b[201~")):
			if f.paste() {
				out = append(out, buf[:6]...)
			}
			buf = buf[6:]
			continue
		case bytes.HasPrefix(buf, []byte("\x1b[21~")):
			f.detach()
			buf = buf[5:]
			continue
		case f.switchView != nil && bytes.HasPrefix(buf, []byte("\x1b[20~")):
			f.switchView()
			buf = buf[5:]
			continue
		default:
			if !flush && isProperPrefix(buf) {
				f.pending = append([]byte(nil), buf...)
				return out
			}
		}
		out = append(out, buf[0])
		buf = buf[1:]
	}
	return out
}

func isProperPrefix(b []byte) bool {
	for _, p := range filteredPrefixes {
		if len(b) < len(p) && strings.HasPrefix(p, string(b)) {
			return true
		}
	}
	return false
}

// attachFrame holds the virtual terminal of the container and the geometry of
// the box it is drawn into: a titlebar on the first row, one-cell borders on
// the other three sides, and the container's terminal in between.
type attachFrame struct {
	mu         sync.Mutex
	emu        *vt.Emulator
	cols, rows int
	name       string
	appTitle   string
	mouseModes map[int]bool
	mouseSGR   bool
	// outerMouse is the tracking level (1000, 1002 or 1003) currently
	// requested from the user's terminal.
	outerMouse int
	paste      bool
	prev       []string
	// switchLabel is the second titlebar button; empty when the frame has
	// only one view.
	switchLabel string

	// Text selection, used while the container does not track the mouse.
	// Positions are 0-based cells of the container's screen.
	selecting         bool // left button is held
	selActive         bool // a non-empty selection is highlighted
	selStart, selEnd  cellPos
	onSelect          func(text string) // called with the selected text on release
	onSelectionChange func()
	// onClipboard receives OSC 52 clipboard writes of the container, which
	// the emulator would otherwise drop.
	onClipboard func(seq string)
}

type cellPos struct{ x, y int }

type frameOutcome int

const (
	outcomeExited frameOutcome = iota
	outcomeDetached
	outcomeSwitch
)

type frameOptions struct {
	label       string        // shown in the titlebar
	switchLabel string        // second button; empty for none
	quietExit   bool          // do not print the output of a failed exit
	modes       func() string // sequences for the terminal modes the application enabled before this attach
}

func newAttachFrame(name string, cols, rows int) *attachFrame {
	f := &attachFrame{
		emu:        vt.NewEmulator(cols-2, rows-2),
		cols:       cols,
		rows:       rows,
		name:       name,
		mouseModes: map[int]bool{},
		outerMouse: 1000,
	}
	set := func(mode ansi.Mode, on bool) {
		dec, ok := mode.(ansi.DECMode)
		if !ok {
			return
		}
		switch dec {
		case ansi.ModeMouseX10, ansi.ModeMouseNormal, ansi.ModeMouseButtonEvent, ansi.ModeMouseAnyEvent:
			f.mouseModes[int(dec)] = on
		case ansi.ModeMouseExtSgr:
			f.mouseSGR = on
		case ansi.ModeBracketedPaste:
			f.paste = on
		}
	}
	// The callbacks run while Write holds f.mu.
	f.emu.SetCallbacks(vt.Callbacks{
		Title:       func(title string) { f.appTitle = sanitizeTitle(title) },
		EnableMode:  func(mode ansi.Mode) { set(mode, true) },
		DisableMode: func(mode ansi.Mode) { set(mode, false) },
	})
	f.emu.RegisterOscHandler(52, func(data []byte) bool {
		// data is "52;<targets>;<base64>"; queries ("?") are not answered.
		parts := strings.SplitN(string(data), ";", 3)
		if len(parts) == 3 && parts[2] != "?" && f.onClipboard != nil {
			f.onClipboard("\x1b]52;" + parts[1] + ";" + parts[2] + "\x07")
		}
		return true
	})
	return f
}

func sanitizeTitle(title string) string {
	title = ansi.Strip(title)
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, title)
}

func (f *attachFrame) innerSize() (cols, rows int) { return f.cols - 2, f.rows - 2 }

func (f *attachFrame) write(p []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = f.emu.Write(p)
}

func (f *attachFrame) resize(cols, rows int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cols, f.rows = cols, rows
	f.emu.Resize(cols-2, rows-2)
	f.prev = nil
}

func (f *attachFrame) title() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appTitle
}

func (f *attachFrame) pasteEnabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paste
}

// handleMouse consumes clicks on the titlebar and translates reports inside
// the box for containers that asked for mouse input.
func (f *attachFrame) handleMouse(ev mouseEvent, detach, switchView func()) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ev.y == 1 {
		if !ev.leftPress() {
			return nil
		}
		showSwitch, _, switchStart, detachStart := f.buttonLayout()
		switch {
		case ev.x >= detachStart && ev.x <= f.cols:
			detach()
		case showSwitch && switchView != nil && ev.x >= switchStart && ev.x < detachStart:
			switchView()
		}
		return nil
	}
	level := f.mouseLevel()
	if level == 0 {
		f.handleSelection(ev)
		return nil
	}
	if ev.x < 2 || ev.x > f.cols-1 || ev.y < 2 || ev.y > f.rows-1 {
		return nil
	}
	motion := ev.button&32 != 0 && ev.button&64 == 0
	if motion && (level == 1000 || (level == 1002 && ev.button&3 == 3)) {
		return nil // the container did not ask for this kind of motion
	}
	x, y := ev.x-1, ev.y-1
	if f.mouseSGR {
		final := byte('M')
		if ev.release {
			final = 'm'
		}
		return []byte("\x1b[<" + strconv.Itoa(ev.button) + ";" + strconv.Itoa(x) + ";" + strconv.Itoa(y) + string(final))
	}
	// Legacy X10 encoding, which cannot address cells beyond column 223.
	if x > 223 || y > 223 {
		return nil
	}
	button := ev.button
	if ev.release {
		button = button&^3 | 3
	}
	return []byte{0x1b, '[', 'M', byte(32 + button), byte(32 + x), byte(32 + y)}
}

// handleSelection implements drag-to-select for containers that do not use
// the mouse themselves. The caller holds f.mu.
func (f *attachFrame) handleSelection(ev mouseEvent) {
	if ev.button&(3|64|128) != 0 {
		return
	}
	innerCols, innerRows := f.innerSize()
	pos := cellPos{
		x: min(max(ev.x-2, 0), innerCols-1),
		y: min(max(ev.y-2, 0), innerRows-1),
	}
	motion := ev.button&32 != 0
	switch {
	case !ev.release && !motion:
		changed := f.selActive
		f.selecting, f.selActive = true, false
		f.selStart, f.selEnd = pos, pos
		if changed && f.onSelectionChange != nil {
			f.onSelectionChange()
		}
	case motion && f.selecting:
		f.selEnd = pos
		f.selActive = f.selEnd != f.selStart
		if f.onSelectionChange != nil {
			f.onSelectionChange()
		}
	case ev.release && f.selecting:
		f.selecting = false
		f.selEnd = pos
		f.selActive = f.selEnd != f.selStart
		if f.selActive && f.onSelect != nil {
			f.onSelect(f.selectedText())
		}
	}
}

// selectionRange orders the selection so that a precedes b.
func (f *attachFrame) selectionRange() (a, b cellPos) {
	a, b = f.selStart, f.selEnd
	if b.y < a.y || (b.y == a.y && b.x < a.x) {
		a, b = b, a
	}
	return a, b
}

// selectedText returns the selected cells as text, one line per row.
func (f *attachFrame) selectedText() string {
	a, b := f.selectionRange()
	innerCols, _ := f.innerSize()
	var lines []string
	for y := a.y; y <= b.y; y++ {
		x0, x1 := 0, innerCols-1
		if y == a.y {
			x0 = a.x
		}
		if y == b.y {
			x1 = b.x
		}
		var line strings.Builder
		for x := x0; x <= x1; x++ {
			if c := f.emu.CellAt(x, y); c != nil && c.Width > 0 {
				line.WriteString(c.Content)
			} else if c == nil {
				line.WriteByte(' ')
			}
		}
		lines = append(lines, strings.TrimRight(line.String(), " "))
	}
	return strings.Join(lines, "\n")
}

// clearSelection drops the highlight, for instance when the user types.
func (f *attachFrame) clearSelection() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.selActive && f.onSelectionChange != nil {
		f.onSelectionChange()
	}
	f.selecting, f.selActive = false, false
}

// highlightedRow draws one row of the container's screen with the selected
// cells in reverse video. The caller holds f.mu.
func (f *attachFrame) highlightedRow(y int) string {
	a, b := f.selectionRange()
	innerCols, _ := f.innerSize()
	x0, x1 := 0, innerCols-1
	if y == a.y {
		x0 = a.x
	}
	if y == b.y {
		x1 = b.x
	}
	var line strings.Builder
	for x := 0; x < innerCols; x++ {
		c := f.emu.CellAt(x, y)
		if c == nil {
			line.WriteByte(' ')
			continue
		}
		if c.Width == 0 {
			continue
		}
		style := c.Style
		if x >= x0 && x <= x1 {
			style.Attrs ^= uv.AttrReverse
		}
		content := c.Content
		if content == "" {
			content = " "
		}
		line.WriteString(style.Styled(content))
	}
	return line.String()
}

// mouseLevel is the most detailed mouse tracking the container asked for:
// 1000 for clicks, 1002 for drags, 1003 for all motion, 0 for none.
func (f *attachFrame) mouseLevel() int {
	level := 0
	for mode, on := range f.mouseModes {
		if !on {
			continue
		}
		if mode == int(ansi.ModeMouseX10) {
			mode = 1000
		}
		level = max(level, mode)
	}
	return level
}

// syncMouse returns the sequences that make the user's terminal report what
// the container asked for. Clicks are always reported for the titlebar.
func (f *attachFrame) syncMouse() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Drags are needed to select text when the container ignores the mouse.
	want := f.mouseLevel()
	if want == 0 {
		want = 1002
	}
	if want == f.outerMouse {
		return ""
	}
	// Terminals keep a single tracking level, so reset and set it afresh.
	seq := "\x1b[?1003l\x1b[?1002l\x1b[?1000h"
	if want > 1000 {
		seq += "\x1b[?" + strconv.Itoa(want) + "h"
	}
	f.outerMouse = want
	return seq
}

// buttonLayout positions the titlebar buttons at its right end. The switch
// button sits left of the detach button and is dropped when it does not fit.
// Columns are 1-based.
func (f *attachFrame) buttonLayout() (showSwitch bool, titleWidth, switchStart, detachStart int) {
	detachWidth := ansi.StringWidth(frameButtonLabel)
	switchWidth := ansi.StringWidth(f.switchLabel)
	showSwitch = f.switchLabel != "" && f.cols >= detachWidth+switchWidth
	width := detachWidth
	if showSwitch {
		width += switchWidth
	}
	titleWidth = max(f.cols-width, 0)
	detachStart = max(f.cols-detachWidth+1, 1)
	switchStart = detachStart - switchWidth
	return
}

func (f *attachFrame) titleBar() string {
	rest := " agentbox: " + f.name
	if f.appTitle != "" {
		rest += " - " + f.appTitle
	}
	showSwitch, titleWidth, _, _ := f.buttonLayout()
	buttons := sgrButton + ansi.Truncate(frameButtonLabel, f.cols, "")
	if showSwitch {
		buttons = sgrSwitch + f.switchLabel + buttons
	}
	rest = ansi.Truncate(rest, titleWidth, "…")
	pad := titleWidth - ansi.StringWidth(rest)
	return sgrTitleBar + rest + strings.Repeat(" ", pad) + buttons + sgrReset
}

// render returns the escape sequences that bring the user's terminal in line
// with the frame. Unchanged rows are skipped unless full is set.
func (f *attachFrame) render(full bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	innerCols, innerRows := f.innerSize()
	lines := strings.Split(f.emu.Render(), "\n")
	if full || len(f.prev) != innerRows {
		full = true
		f.prev = make([]string, innerRows)
	}

	var b strings.Builder
	b.WriteString("\x1b[?2026h\x1b[?25l")
	if full {
		b.WriteString("\x1b[1;1H" + f.titleBar())
		bottom := sgrBorder + "└" + strings.Repeat("─", innerCols) + "┘" + sgrReset
		b.WriteString("\x1b[" + strconv.Itoa(f.rows) + ";1H" + bottom)
	}
	for r := 0; r < innerRows; r++ {
		line := ""
		if r < len(lines) {
			line = lines[r]
		}
		if f.selActive {
			if a, b := f.selectionRange(); r >= a.y && r <= b.y {
				line = f.highlightedRow(r)
			}
		}
		if !full && line == f.prev[r] {
			continue
		}
		f.prev[r] = line
		row := strconv.Itoa(r + 2)
		b.WriteString("\x1b[" + row + ";1H" + sgrBorder + "│" + sgrReset + line + sgrReset + "\x1b[K")
		b.WriteString("\x1b[" + row + ";" + strconv.Itoa(f.cols) + "H" + sgrBorder + "│" + sgrReset)
	}
	if !f.emu.CursorHidden() {
		pos := f.emu.CursorPosition()
		b.WriteString("\x1b[" + strconv.Itoa(pos.Y+2) + ";" + strconv.Itoa(pos.X+2) + "H\x1b[?25h")
	}
	b.WriteString("\x1b[?2026l")
	return b.String()
}

// titleOnly redraws just the titlebar.
func (f *attachFrame) titleOnly() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "\x1b7\x1b[1;1H" + f.titleBar() + "\x1b8"
}

type outputTail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *outputTail) add(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > maxOutputTail {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-maxOutputTail:]...)
	}
}

func (t *outputTail) text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(ansi.Strip(string(t.buf)))
}

func runFramedAttach(cmd *exec.Cmd, opts frameOptions, cols, rows int) (outcome frameOutcome, err error) {
	stdin := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(stdin)
	if err != nil {
		return outcomeExited, fmt.Errorf("enter raw terminal mode: %w", err)
	}
	defer func() { _ = term.Restore(stdin, oldState) }()

	frame := newAttachFrame(opts.label, cols, rows)
	frame.switchLabel = opts.switchLabel
	innerCols, innerRows := frame.innerSize()
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(innerRows), Cols: uint16(innerCols)})
	if err != nil {
		return outcomeExited, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	defer ptmx.Close()

	var screenMu sync.Mutex
	paint := func(s string) {
		screenMu.Lock()
		defer screenMu.Unlock()
		_, _ = io.WriteString(os.Stdout, s)
	}
	paint(enterFrameUI)
	defer func() { paint(leaveFrameUI) }()

	var (
		leaveOnce sync.Once
		leaveKind atomic.Int32 // frameOutcome that ended the view; -1 while running
		tail      outputTail
		done      = make(chan struct{})
		quit      = make(chan struct{})
		dirty     = make(chan struct{}, 1)
		ptyClosed = make(chan struct{})
	)
	leave := func(kind frameOutcome) {
		leaveOnce.Do(func() {
			leaveKind.Store(int32(kind))
			// Killing the attach client detaches without stopping the
			// container. SIGKILL cannot be proxied to the container.
			_ = cmd.Process.Kill()
		})
	}
	leaveKind.Store(-1)
	requestDetach := func() { leave(outcomeDetached) }
	var requestSwitch func()
	if opts.switchLabel != "" {
		requestSwitch = func() { leave(outcomeSwitch) }
	}

	// The runtime does not replay what the application emitted before this
	// attach, so the modes it enabled then (alternate screen, mouse tracking)
	// are recovered from the log. That happens once the stream has proven to
	// be connected, so nothing falls into the gap between the log and the
	// attach, and output is held back until the modes are applied.
	var (
		seeded    = make(chan struct{})
		firstRead = make(chan struct{}, 1)
	)
	go func() {
		defer close(seeded)
		if opts.modes == nil {
			return
		}
		select {
		case <-firstRead:
		case <-time.After(modeSeedDelay):
		case <-quit:
			return
		}
		// The runtime's log lags behind the stream by a few milliseconds.
		select {
		case <-time.After(modeSeedSettle):
		case <-quit:
			return
		}
		seq := opts.modes()
		if seq != "" {
			frame.write([]byte(seq))
		}
	}()

	// Container output -> virtual terminal.
	go func() {
		defer close(ptyClosed)
		buf := make([]byte, 32*1024)
		held := false
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if !held {
					held = true
					select {
					case firstRead <- struct{}{}:
					default:
					}
					<-seeded
				}
				tail.add(buf[:n])
				frame.write(buf[:n])
				select {
				case dirty <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Replies of the virtual terminal (cursor reports, attribute queries).
	go func() { _, _ = io.Copy(ptmx, frame.emu) }()

	frame.onSelectionChange = func() {
		select {
		case dirty <- struct{}{}:
		default:
		}
	}
	frame.onClipboard = paint
	// Copy through the terminal (OSC 52); the selection stays highlighted.
	frame.onSelect = func(text string) {
		paint("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07")
	}

	// User input -> container.
	filter := &inputFilter{
		detach:     requestDetach,
		switchView: requestSwitch,
		mouse:      func(ev mouseEvent) []byte { return frame.handleMouse(ev, requestDetach, requestSwitch) },
		paste:      frame.pasteEnabled,
	}
	input := make(chan []byte)
	go readStdin(quit, input)
	go func() {
		var flushTimer <-chan time.Time
		forward := func(b []byte) {
			if len(b) > 0 {
				frame.clearSelection()
				_, _ = ptmx.Write(b)
			}
		}
		for {
			select {
			case data, ok := <-input:
				if !ok {
					return
				}
				forward(filter.feed(data, false))
				if len(filter.pending) > 0 {
					flushTimer = time.After(50 * time.Millisecond)
				} else {
					flushTimer = nil
				}
			case <-flushTimer:
				flushTimer = nil
				forward(filter.feed(nil, true))
			case <-quit:
				return
			}
		}
	}()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(stop)

	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()

	// A reattached container has no reason to repaint. Nudging the size makes
	// the runtime forward SIGWINCH so the application redraws the new view.
	if innerRows > 2 {
		go func() {
			select {
			case <-time.After(150 * time.Millisecond):
			case <-quit:
				return
			}
			_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(innerRows - 1), Cols: uint16(innerCols)})
			select {
			case <-time.After(100 * time.Millisecond):
			case <-quit:
			}
			ic, ir := frame.innerSize()
			_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(ir), Cols: uint16(ic)})
		}()
	}

	paint(frame.render(true) + frame.syncMouse())
	lastTitle := ""
loop:
	for {
		select {
		case <-dirty:
			time.Sleep(8 * time.Millisecond) // coalesce bursts of output
			select {
			case <-dirty:
			default:
			}
			paint(frame.render(false) + frame.syncMouse())
			if t := frame.title(); t != lastTitle {
				lastTitle = t
				paint(frame.titleOnly())
			}
		case <-winch:
			if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w >= frameMinCols && h >= frameMinRows {
				frame.resize(w, h)
				ic, ir := frame.innerSize()
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(ir), Cols: uint16(ic)})
				paint("\x1b[2J" + frame.render(true))
			}
		case <-stop:
			requestDetach()
		case <-done:
			break loop
		}
	}
	close(quit)
	select {
	case <-ptyClosed:
	case <-time.After(500 * time.Millisecond):
	}

	if kind := leaveKind.Load(); kind >= 0 {
		return frameOutcome(kind), nil
	}
	if waitErr != nil {
		if opts.quietExit {
			return outcomeExited, waitErr
		}
		paint(leaveFrameUI)
		if text := tail.text(); text != "" {
			// The terminal is still in raw mode here.
			fmt.Fprint(os.Stderr, strings.ReplaceAll(text, "\n", "\r\n")+"\r\n")
		}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return outcomeExited, exitErr
		}
		return outcomeExited, waitErr
	}
	return outcomeExited, nil
}

// readStdin forwards raw input until quit is closed. It polls so that it does
// not keep consuming stdin after the frame is gone.
func readStdin(quit <-chan struct{}, out chan<- []byte) {
	buf := make([]byte, 4096)
	fds := []unix.PollFd{{Fd: int32(os.Stdin.Fd()), Events: unix.POLLIN}}
	for {
		select {
		case <-quit:
			return
		default:
		}
		n, err := unix.Poll(fds, 100)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return
		}
		m, err := os.Stdin.Read(buf)
		if m > 0 {
			select {
			case out <- append([]byte(nil), buf[:m]...):
			case <-quit:
				return
			}
		}
		if err != nil {
			return
		}
	}
}
