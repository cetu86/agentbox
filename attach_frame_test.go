package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func newTestFilter(f *attachFrame, detached *int) *inputFilter {
	return newTestFilterSwitch(f, detached, new(int))
}

func newTestFilterSwitch(f *attachFrame, detached, switched *int) *inputFilter {
	detach := func() { *detached++ }
	return &inputFilter{
		detach:     detach,
		switchView: func() { *switched++ },
		mouse:      func(ev mouseEvent) []byte { return f.handleMouse(ev, detach, func() { *switched++ }) },
		paste:      f.pasteEnabled,
	}
}

func TestParseSGRMouse(t *testing.T) {
	ev, n, status := parseSGRMouse([]byte("\x1b[<0;5;1Mrest"))
	if status != parseOK || n != 9 || ev.button != 0 || ev.x != 5 || ev.y != 1 || ev.release {
		t.Fatalf("got %+v n=%d status=%v", ev, n, status)
	}
	if ev, _, _ := parseSGRMouse([]byte("\x1b[<0;5;1m")); !ev.release {
		t.Fatal("m should be a release")
	}
	if _, _, status := parseSGRMouse([]byte("\x1b[<0;5")); status != parseIncomplete {
		t.Fatalf("status = %v, want incomplete", status)
	}
	if _, _, status := parseSGRMouse([]byte("\x1b[<x")); status != parseInvalid {
		t.Fatalf("status = %v, want invalid", status)
	}
}

func TestInputFilterDetachKeys(t *testing.T) {
	f := newAttachFrame("agent", 80, 24)
	detached := 0
	filter := newTestFilter(f, &detached)

	if got := filter.feed([]byte("a\x1b[21~b"), false); string(got) != "ab" || detached != 1 {
		t.Fatalf("F10: out=%q detached=%d", got, detached)
	}
	// Click on the detach button, and a click elsewhere on the titlebar.
	if got := filter.feed([]byte("\x1b[<0;75;1M"), false); len(got) != 0 || detached != 2 {
		t.Fatalf("button click: out=%q detached=%d", got, detached)
	}
	if got := filter.feed([]byte("\x1b[<0;3;1M"), false); len(got) != 0 || detached != 2 {
		t.Fatalf("titlebar click: out=%q detached=%d", got, detached)
	}
	// Release, wheel, and motion on the button do not detach.
	for _, seq := range []string{"\x1b[<0;3;1m", "\x1b[<64;3;1M", "\x1b[<32;3;1M"} {
		filter.feed([]byte(seq), false)
	}
	if detached != 2 {
		t.Fatalf("detached = %d, want 2", detached)
	}
}

func TestInputFilterPassesOtherInput(t *testing.T) {
	f := newAttachFrame("agent", 80, 24)
	detached := 0
	filter := newTestFilter(f, &detached)

	for _, in := range []string{"hello", "\x1b[A", "\x1bOP", "\x1b[1;5C", "\x00\x11"} {
		if got := filter.feed([]byte(in), false); string(got) != in {
			t.Errorf("feed(%q) = %q", in, got)
		}
	}
	if detached != 0 {
		t.Fatalf("detached = %d", detached)
	}
}

func TestInputFilterHoldsPartialSequences(t *testing.T) {
	f := newAttachFrame("agent", 80, 24)
	detached := 0
	filter := newTestFilter(f, &detached)

	if got := filter.feed([]byte("x\x1b[2"), false); string(got) != "x" {
		t.Fatalf("out = %q", got)
	}
	if got := filter.feed([]byte("1~"), false); len(got) != 0 || detached != 1 {
		t.Fatalf("out=%q detached=%d", got, detached)
	}
	// A lone Escape is released when the flush timer fires.
	if got := filter.feed([]byte("\x1b"), false); len(got) != 0 {
		t.Fatalf("out = %q", got)
	}
	if got := filter.feed(nil, true); string(got) != "\x1b" {
		t.Fatalf("flushed = %q", got)
	}
}

func TestInputFilterBracketedPaste(t *testing.T) {
	f := newAttachFrame("agent", 80, 24)
	detached := 0
	filter := newTestFilter(f, &detached)
	paste := "\x1b[200~text\x1b[201~"

	if got := filter.feed([]byte(paste), false); string(got) != "text" {
		t.Fatalf("paste without container support = %q", got)
	}
	f.write([]byte("\x1b[?2004h"))
	if got := filter.feed([]byte(paste), false); string(got) != paste {
		t.Fatalf("paste with container support = %q", got)
	}
}

func TestMouseForwardedOnlyWhenRequested(t *testing.T) {
	f := newAttachFrame("agent", 80, 24)
	detached := 0
	filter := newTestFilter(f, &detached)
	click := "\x1b[<0;10;5M"

	if got := filter.feed([]byte(click), false); len(got) != 0 {
		t.Fatalf("mouse forwarded without request: %q", got)
	}
	f.write([]byte("\x1b[?1000h\x1b[?1006h"))
	if got := filter.feed([]byte(click), false); string(got) != "\x1b[<0;9;4M" {
		t.Fatalf("translated mouse = %q", got)
	}
	// Clicks on the border are not forwarded.
	if got := filter.feed([]byte("\x1b[<0;1;5M"), false); len(got) != 0 {
		t.Fatalf("border click forwarded: %q", got)
	}
}

func TestFrameRender(t *testing.T) {
	f := newAttachFrame("agent-1", 80, 8)
	f.write([]byte("\x1b]0;my title\x07hello\r\n\x1b[31mred\x1b[0m"))

	out := f.render(true)
	plain := ansi.Strip(out)
	for _, want := range []string{"Detach (F10)", "agentbox: agent-1 - my title", "│hello", "│red", "└────"} {
		if !strings.Contains(plain, want) {
			t.Errorf("render missing %q in %q", want, plain)
		}
	}
	if strings.Index(plain, "Detach (F10)") < strings.Index(plain, "agentbox:") {
		t.Error("detach button must come after the title, at the right end")
	}
	// Cursor follows the container's cursor: after "red" on its second row.
	if !strings.Contains(out, "\x1b[3;5H\x1b[?25h") {
		t.Errorf("cursor not placed at row 3 col 5: %q", out)
	}

	if got := f.render(false); strings.Contains(ansi.Strip(got), "hello") {
		t.Errorf("unchanged rows were redrawn: %q", got)
	}
	f.write([]byte("!"))
	if got := ansi.Strip(f.render(false)); !strings.Contains(got, "red!") || strings.Contains(got, "hello") {
		t.Errorf("incremental render = %q", got)
	}
}

func TestFrameTitleBarFitsWidth(t *testing.T) {
	for _, width := range []int{frameMinCols, 30, 80, 200} {
		f := newAttachFrame("a-rather-long-container-name", width, 10)
		f.appTitle = "some application title"
		if got := ansi.StringWidth(f.titleBar()); got != width {
			t.Errorf("width %d: titlebar is %d cells wide", width, got)
		}
	}
}

func TestFrameResize(t *testing.T) {
	f := newAttachFrame("agent", 40, 10)
	f.resize(60, 20)
	if cols, rows := f.innerSize(); cols != 58 || rows != 18 {
		t.Fatalf("inner size = %dx%d", cols, rows)
	}
	if f.emu.Width() != 58 || f.emu.Height() != 18 {
		t.Fatalf("emulator size = %dx%d", f.emu.Width(), f.emu.Height())
	}
}

func TestInputFilterSwitchKey(t *testing.T) {
	f := newAttachFrame("agent", 80, 24)
	detached, switched := 0, 0
	filter := newTestFilterSwitch(f, &detached, &switched)
	if got := filter.feed([]byte("a\x1b[20~b"), false); string(got) != "ab" || switched != 1 || detached != 0 {
		t.Fatalf("got %q switched=%d detached=%d", got, switched, detached)
	}
	// A partial F9 sequence is held back until it completes.
	if got := filter.feed([]byte("\x1b[2"), false); len(got) != 0 {
		t.Fatalf("partial sequence forwarded: %q", got)
	}
	if got := filter.feed([]byte("0~"), false); len(got) != 0 || switched != 2 {
		t.Fatalf("got %q switched=%d", got, switched)
	}
	// Without a second view F9 reaches the container.
	filter.switchView = nil
	if got := filter.feed([]byte("\x1b[20~"), false); string(got) != "\x1b[20~" {
		t.Fatalf("F9 not forwarded: %q", got)
	}
}

func TestShellButtonClick(t *testing.T) {
	const cols = 80
	f := newAttachFrame("agent", cols, 24)
	f.switchLabel = shellButtonLabel
	detached, switched := 0, 0
	filter := newTestFilterSwitch(f, &detached, &switched)

	dw := ansi.StringWidth(frameButtonLabel)
	sw := ansi.StringWidth(shellButtonLabel)
	detachStart := cols - dw + 1
	switchStart := detachStart - sw
	click := func(x int) { filter.feed([]byte("\x1b[<0;"+strconv.Itoa(x)+";1M"), false) }

	click(detachStart)
	click(cols)
	if detached != 2 || switched != 0 {
		t.Fatalf("detach button: detached=%d switched=%d", detached, switched)
	}
	click(switchStart)
	click(detachStart - 1)
	if detached != 2 || switched != 2 {
		t.Fatalf("shell button: detached=%d switched=%d", detached, switched)
	}
	click(1)
	click(switchStart - 1)
	if detached != 2 || switched != 2 {
		t.Fatalf("title text must not click: detached=%d switched=%d", detached, switched)
	}

	f.switchLabel = ""
	click(switchStart)
	if switched != 2 {
		t.Fatalf("no switch button, switched=%d", switched)
	}
}

func TestTitleBarWithSwitchButton(t *testing.T) {
	for width := 20; width <= 80; width += 7 {
		f := newAttachFrame("a-rather-long-container-name", width, 10)
		f.switchLabel = agentButtonLabel
		bar := f.titleBar()
		if got := ansi.StringWidth(bar); got != width {
			t.Errorf("width %d: titlebar is %d wide", width, got)
		}
		plain := ansi.Strip(bar)
		if !strings.HasSuffix(plain, frameButtonLabel) {
			t.Errorf("width %d: detach button not at the right end of %q", width, plain)
		}
		if width >= 40 && !strings.HasSuffix(plain, agentButtonLabel+frameButtonLabel) {
			t.Errorf("width %d: switch button not left of detach in %q", width, plain)
		}
		if width >= 50 && !strings.HasPrefix(plain, " agentbox: ") {
			t.Errorf("width %d: title not at the left of %q", width, plain)
		}
	}
}

func TestMouseLevelsAndEncoding(t *testing.T) {
	f := newAttachFrame("agent", 80, 24)
	press := mouseEvent{button: 0, x: 10, y: 5}
	drag := mouseEvent{button: 32, x: 11, y: 5}
	hover := mouseEvent{button: 35, x: 12, y: 5}
	wheel := mouseEvent{button: 64, x: 10, y: 5}
	noop := func() {}

	if got := f.handleMouse(press, noop, noop); got != nil {
		t.Fatalf("no tracking requested, got %q", got)
	}
	if got := f.syncMouse(); got != "" {
		t.Fatalf("baseline sync = %q", got)
	}

	// Copilot-style: any-event tracking with SGR encoding.
	f.write([]byte("\x1b[?1003;1006h"))
	if got := f.syncMouse(); !strings.Contains(got, "\x1b[?1003h") {
		t.Fatalf("1003 not requested from the terminal: %q", got)
	}
	if got := f.syncMouse(); got != "" {
		t.Fatalf("repeated sync = %q", got)
	}
	for _, ev := range []mouseEvent{press, drag, hover, wheel} {
		if got := f.handleMouse(ev, noop, noop); got == nil {
			t.Errorf("event %+v was dropped", ev)
		}
	}
	if got := string(f.handleMouse(press, noop, noop)); got != "\x1b[<0;9;4M" {
		t.Errorf("sgr press = %q", got)
	}

	// Clicks only: motion is not forwarded.
	f.write([]byte("\x1b[?1003l\x1b[?1000h"))
	if got := f.syncMouse(); !strings.Contains(got, "\x1b[?1000h") || strings.Contains(got, "1003h") {
		t.Fatalf("sync back to clicks = %q", got)
	}
	if got := f.handleMouse(drag, noop, noop); got != nil {
		t.Errorf("drag forwarded at click level: %q", got)
	}
	if got := f.handleMouse(press, noop, noop); got == nil {
		t.Error("press dropped at click level")
	}

	// Without SGR the legacy encoding is used.
	f.write([]byte("\x1b[?1006l"))
	if got := f.handleMouse(press, noop, noop); string(got) != "\x1b[M"+string([]byte{32, 32 + 9, 32 + 4}) {
		t.Errorf("legacy press = %q", got)
	}
	rel := press
	rel.release = true
	if got := f.handleMouse(rel, noop, noop); len(got) != 6 || got[3] != 32+3 {
		t.Errorf("legacy release = %q", got)
	}
}
