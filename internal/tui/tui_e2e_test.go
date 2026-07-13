package tui

import (
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func runProgramE2E(t *testing.T, m *Model, msgs []tea.Msg) {
	t.Helper()
	w := m.width
	h := m.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	p := tea.NewProgram(
		m,
		tea.WithOutput(io.Discard),
		tea.WithWindowSize(w, h),
		tea.WithoutSignals(),
	)

	done := make(chan error, 1)
	go func() {
		_, err := p.Run()
		done <- err
	}()

	for _, msg := range msgs {
		p.Send(msg)
	}
	// Give the event loop one render tick to process queued messages before quit.
	time.Sleep(80 * time.Millisecond)
	p.Quit()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("program exited with error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("program did not exit in time")
	}
}

func TestProgramE2E_TabCompletionFlow(t *testing.T) {
	m := New("test-model", nil, nil, nil, []CommandItem{
		{Name: "help", Desc: "show help"},
		{Name: "clear", Desc: "clear session"},
	})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	runProgramE2E(t, m, []tea.Msg{
		tea.KeyPressMsg{Code: '/', Text: "/"},
		tea.KeyPressMsg{Code: 'h', Text: "h"},
		tea.KeyPressMsg{Code: 'e', Text: "e"},
		tea.KeyPressMsg{Code: tea.KeyTab},
	})

	if got, want := m.ta.Value(), "/help "; got != want {
		t.Fatalf("tab completion through program loop failed, got %q want %q", got, want)
	}
}

func TestProgramE2E_ReportedUIScenario(t *testing.T) {
	m := New("gpt-5.3-codex", nil, nil, nil, []CommandItem{
		{Name: "help", Desc: "show help"},
		{Name: "history", Desc: "show sessions"},
	})
	m.Update(tea.WindowSizeMsg{Width: 96, Height: 12})

	runProgramE2E(t, m, []tea.Msg{
		statusUpdateMsg(StatusInfo{
			Version:  "7.1.0",
			Model:    "gpt-5.3-codex",
			Provider: "openai",
			PermMode: "default",
			TokensIn: 39800,
			Budget:   10,
		}),
		activityMsg(".   .   . thinking..."),
		tea.KeyPressMsg{Code: '/', Text: "/"},
		tea.KeyPressMsg{Code: 'h', Text: "h"},
	})

	view := m.View().Content
	if !utf8.ValidString(view) {
		t.Fatal("expected UTF-8 valid frame in reported UI scenario")
	}
	if bar := m.renderBottomBar(); !strings.Contains(bar, "Ctrl+Y") && !strings.Contains(bar, "Copy:") && !strings.Contains(bar, "复制") {
		t.Fatalf("expected shortcut hint in bottom bar, got %q", bar)
	}
	if !strings.Contains(view, "thinking") {
		t.Fatal("expected activity line to be rendered in transient area")
	}
	for i, line := range strings.Split(view, "\n") {
		if w := ansi.StringWidth(line); w > m.width {
			t.Fatalf("line %d overflowed viewport width: got %d > %d", i+1, w, m.width)
		}
	}
}

func TestProgramE2E_ASCIIBottomBarFallback(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "1")

	m := New("gpt-5.3-codex", nil, nil, nil, []CommandItem{{Name: "help", Desc: "show help"}})
	m.Update(tea.WindowSizeMsg{Width: 64, Height: 12})
	m.Update(statusUpdateMsg(StatusInfo{Model: "gpt-5.3-codex", TokensIn: 1200}))

	bar := m.renderBottomBar()
	if !strings.Contains(bar, "Copy:") {
		t.Fatalf("expected ASCII compact bottom bar hint, got %q", bar)
	}
	if strings.Contains(bar, "复制") {
		t.Fatalf("expected no CJK hint when ASCII fallback is enabled, got %q", bar)
	}
}

func TestProgramE2E_ReportedScenario_WidthAndTextMatrix_NoOverflow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		asciiEnv string
		width    int
		activity string
	}{
		{name: "ascii-narrow", asciiEnv: "1", width: 48, activity: ".   .   . thinking..."},
		{name: "ascii-medium", asciiEnv: "1", width: 64, activity: "running powershell"},
		{name: "cjk-medium", asciiEnv: "0", width: 64, activity: "执行 powershell"},
		{name: "cjk-wide", asciiEnv: "0", width: 96, activity: ".   .   . thinking..."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COVE_TUI_ASCII", tc.asciiEnv)

			m := New("gpt-5.3-codex", nil, nil, nil, []CommandItem{
				{Name: "help", Desc: "show help"},
				{Name: "history", Desc: "show sessions"},
			})
			m.Update(tea.WindowSizeMsg{Width: tc.width, Height: 12})

			runProgramE2E(t, m, []tea.Msg{
				statusUpdateMsg(StatusInfo{
					Version:  "7.1.0",
					Model:    "gpt-5.3-codex",
					Provider: "openai",
					PermMode: "default",
					TokensIn: 39800,
					Budget:   10,
				}),
				activityMsg(tc.activity),
				tea.KeyPressMsg{Code: '/', Text: "/"},
				tea.KeyPressMsg{Code: 'h', Text: "h"},
			})

			view := m.View().Content
			if !utf8.ValidString(view) {
				t.Fatalf("expected UTF-8 valid frame, got %q", view)
			}
			if !strings.Contains(view, tc.activity) {
				t.Fatalf("expected activity line to be present, want %q", tc.activity)
			}

			for i, line := range strings.Split(view, "\n") {
				if w := ansi.StringWidth(line); w > m.width {
					t.Fatalf("line %d overflowed viewport width: got %d > %d; line=%q", i+1, w, m.width, line)
				}
			}
		})
	}
}
