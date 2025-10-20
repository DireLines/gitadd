package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type fileChange struct {
	Path     string
	Index    rune // index status (staged) – first column
	Worktree rune // worktree status (unstaged) – second column
	RawLine  string

	// Diff totals across staged + unstaged
	Added   int
	Deleted int
	Binary  bool // true if numstat reports "-" (binary) for this path
}

func (f fileChange) Title() string       { return f.Path }
func (f fileChange) Description() string { return "" }
func (f fileChange) FilterValue() string { return f.Path }

// --- custom one-line delegate (no color shift on focus; bold only) ---

type oneLineDelegate struct{}

func (d oneLineDelegate) Height() int                               { return 1 }
func (d oneLineDelegate) Spacing() int                              { return 0 }
func (d oneLineDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd { return nil }

const (
	// Keep your earlier color semantics: green for index, red for worktree.
	colorIndex    = lipgloss.Color("#22c55e") // also used for insertions
	colorWorktree = lipgloss.Color("#ef4444") // also used for deletions
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true)
	legendStyle = lipgloss.NewStyle().Faint(true)
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5555"))
	addStyle    = lipgloss.NewStyle().Foreground(colorIndex)
	delStyle    = lipgloss.NewStyle().Foreground(colorWorktree)
)

func (d oneLineDelegate) Render(w io.Writer, m list.Model, index int, it list.Item) {
	f, ok := it.(fileChange)
	if !ok {
		fmt.Fprint(w, "")
		return
	}

	// Icons for staged/unstaged presence (keep your arrows)
	var icons []string
	color := colorIndex
	if f.Index != ' ' {
		icons = append(icons, " -›")
	}
	if f.Worktree != ' ' {
		color = colorWorktree
		icons = append(icons, "‹- ")
	}
	iconStr := strings.Join(icons, " ")

	// Compose the base line: icons + path
	line := f.Title()
	if iconStr != "" {
		line = iconStr + " " + line
	}

	// Append diff counts (like git pull’s per-file stat)
	if f.Binary {
		line = line + " " + legendStyle.Render("(bin)")
	} else if f.Added > 0 || f.Deleted > 0 {
		parts := make([]string, 0, 2)
		if f.Added > 0 {
			parts = append(parts, addStyle.Render("+"+strconv.Itoa(f.Added)))
		}
		if f.Deleted > 0 {
			parts = append(parts, delStyle.Render("-"+strconv.Itoa(f.Deleted)))
		}
		if len(parts) > 0 {
			line = line + " " + strings.Join(parts, " ")
		}
	}

	// Focused row is bold; no color change
	style := lipgloss.NewStyle().Foreground(color)
	startChar := " "
	if index == m.Index() {
		style = style.Bold(true)
		startChar = "*"
	}
	line = "   " + startChar + style.Render(line)
	fmt.Fprint(w, line)
}

// --- program state ---

type bubbleteaModel struct {
	l            list.Model
	confirmInput textinput.Model
	err          error
}

func main() {
	files, err := loadFilesWithNumstat()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gitadd:", err)
		os.Exit(1)
	}
	items := toBubbleteaItems(files)

	// Use our one-line delegate (1 row per item, no spacing)
	delegate := oneLineDelegate{}
	l := list.New(items, delegate, 0, 0)
	l.Title = "gitadd — interactive add/reset"
	l.SetShowHelp(false)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)

	ti := textinput.New()
	ti.Placeholder = "Type 'y' to confirm"
	ti.CharLimit = 1
	ti.Prompt = "Discard working changes? (y/N): "

	m := bubbleteaModel{l: l, confirmInput: ti}
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "gitadd:", err)
		os.Exit(1)
	}
}

func (m bubbleteaModel) Init() tea.Cmd { return nil }

func (m bubbleteaModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.l.SetSize(msg.Width, msg.Height-4)
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc":
			return m, tea.Quit
		case "r":
			return m.refresh()
		case "right": // stage current
			return m.stageSelected()
		case "left": // unstage current
			return m.unstageSelected()
		case "a":
			return m.stageAll()
		case "u":
			return m.unstageAll()
		}
	}

	var cmd tea.Cmd
	m.l, cmd = m.l.Update(msg)
	return m, cmd
}

func (m bubbleteaModel) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.l.Title) + "\n")
	b.WriteString(m.l.View())
	b.WriteString("\n")
	if m.err != nil {
		b.WriteString(errorStyle.Render("Error: " + m.err.Error()))
		b.WriteString("\n")
	}
	b.WriteString(legendStyle.Render(
		"↑/↓ move  •  ← unstage  •  → stage  •  a stage all  •  u unstage all  •  r refresh  •  q quit\n" +
			"[Index|Work] legend: M=modified, A=added, D=deleted, R=renamed, C=copied, U=updated, ?=untracked, -=clean  •  counts show total +adds/-dels",
	))
	return b.String()
}

func (m *bubbleteaModel) currentItem() *fileChange {
	if len(m.l.Items()) == 0 || m.l.Index() < 0 {
		return nil
	}
	if it, ok := m.l.SelectedItem().(fileChange); ok {
		return &it
	}
	return nil
}

func (m bubbleteaModel) refresh() (tea.Model, tea.Cmd) {
	files, err := loadFilesWithNumstat()
	if err != nil {
		m.err = err
		return m, nil
	}
	m.l.SetItems(toBubbleteaItems(files))
	if m.l.Index() >= len(m.l.Items()) {
		m.l.Select(len(m.l.Items()) - 1)
	}
	return m, nil
}

func (m bubbleteaModel) stageSelected() (tea.Model, tea.Cmd) {
	item := m.currentItem()
	if item == nil {
		return m, nil
	}
	if err := gitAdd(item.Path); err != nil {
		m.err = err
		return m, nil
	}
	return m.refresh()
}

func (m bubbleteaModel) unstageSelected() (tea.Model, tea.Cmd) {
	item := m.currentItem()
	if item == nil {
		return m, nil
	}
	if err := gitUnstage(item.Path); err != nil {
		m.err = err
		return m, nil
	}
	return m.refresh()
}

func (m bubbleteaModel) stageAll() (tea.Model, tea.Cmd) {
	paths := itemsPaths(m.l.Items())
	if len(paths) == 0 {
		return m, nil
	}
	if err := gitAdd(paths...); err != nil {
		m.err = err
		return m, nil
	}
	return m.refresh()
}

func (m bubbleteaModel) unstageAll() (tea.Model, tea.Cmd) {
	paths := itemsPaths(m.l.Items())
	if len(paths) == 0 {
		return m, nil
	}
	if err := gitUnstage(paths...); err != nil {
		m.err = err
		return m, nil
	}
	return m.refresh()
}

// -------- git helpers --------

// loadFilesWithNumstat: status + numstat merged into fileChange rows
func loadFilesWithNumstat() ([]fileChange, error) {
	files, err := gitStatus()
	if err != nil {
		return nil, err
	}
	added, deleted, binary, err := gitNumstatTotals()
	if err != nil {
		return nil, err
	}
	for i := range files {
		p := files[i].Path
		files[i].Added = added[p]
		files[i].Deleted = deleted[p]
		files[i].Binary = binary[p]
	}
	return files, nil
}

func gitStatus() ([]fileChange, error) {
	out, err := run("git", "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("not a git repo or git error: %w", err)
	}
	lines := strings.Split(out, "\n")
	var files []fileChange
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if len(ln) < 3 {
			continue
		}
		x := rune(ln[0])
		y := rune(ln[1])
		p := strings.TrimSpace(ln[3:])
		if i := strings.LastIndex(p, " -> "); i >= 0 {
			p = strings.TrimSpace(p[i+4:])
		}
		files = append(files, fileChange{Path: p, Index: x, Worktree: y, RawLine: ln})
	}
	return files, nil
}

// gitNumstatTotals returns per-path totals for added/deleted lines,
// summing both unstaged (worktree) and staged (index) diffs.
// Also flags binaries (numstat prints "-" for either column).
func gitNumstatTotals() (map[string]int, map[string]int, map[string]bool, error) {
	add := map[string]int{}
	del := map[string]int{}
	bin := map[string]bool{}

	// Unstaged (index..worktree)
	if err := accumulateNumstat(add, del, bin, "diff", "--numstat"); err != nil {
		return nil, nil, nil, err
	}
	// Staged (HEAD..index)
	if err := accumulateNumstat(add, del, bin, "diff", "--cached", "--numstat"); err != nil {
		return nil, nil, nil, err
	}
	return add, del, bin, nil
}

func accumulateNumstat(add, del map[string]int, bin map[string]bool, args ...string) error {
	out, err := run("git", args...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for _, ln := range lines {
		// Format: <added>\t<deleted>\t<path>   (for renames path may be "old\tnew")
		fields := strings.Split(ln, "\t")
		if len(fields) < 3 {
			continue
		}
		addStr := fields[0]
		delStr := fields[1]
		path := fields[len(fields)-1] // take the rightmost (new) path, handles renames

		// Binary files show "-" in either column
		isBin := addStr == "-" || delStr == "-"
		if isBin {
			bin[path] = true
			// We still try to add counts if one side is numeric, but usually "-" on both.
		}

		if a, err := strconv.Atoi(addStr); err == nil {
			add[path] += a
		}
		if d, err := strconv.Atoi(delStr); err == nil {
			del[path] += d
		}
	}
	return nil
}

func gitAdd(paths ...string) error {
	args := append([]string{"add", "--"}, paths...)
	_, err := run("git", args...)
	return err
}

func gitUnstage(paths ...string) error {
	// Reset file(s) from index back to HEAD
	args := append([]string{"reset", "--"}, paths...)
	_, err := run("git", args...)
	return err
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = nil
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %v\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// -------- list item helpers --------

func toBubbleteaItems(files []fileChange) []list.Item {
	items := make([]list.Item, 0, len(files))
	for _, f := range files {
		items = append(items, f)
	}
	return items
}

func itemsPaths(items []list.Item) []string {
	var out []string
	for _, it := range items {
		if f, ok := it.(fileChange); ok {
			out = append(out, f.Path)
		}
	}
	return out
}
