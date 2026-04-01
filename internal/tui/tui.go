package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	maxEvents   = 500
	tickRefresh = 2 * time.Second
)

// tickMsg fires periodically to refresh status and poll events.
type tickMsg time.Time

// liveStatusMsg is the async result of fetching live status from the daemon.
type liveStatusMsg struct {
	status *control.LiveStatusResult
	err    error
}

// eventsMsg is the async result of polling for new events.
type eventsMsg struct {
	events []event.Event
	err    error
}

const runnerLingerDuration = 10 * time.Second

// RunnerState tracks the state of an active runner derived from events.
type RunnerState struct {
	Name        string
	Repo        string
	SpawnedAt   time.Time
	CompletedAt time.Time // set when runner finishes; used for linger display
	JobName     string    // set when job.started, cleared when completed
	State       string    // "starting", "running", "completing", "done"
}

// allEventTypes lists every event type for the type-filter toggle UI.
var allEventTypes = []event.EventType{
	event.EventDaemonStarted,
	event.EventDaemonStopping,
	event.EventScaleSetCreated,
	event.EventScaleSetDeleted,
	event.EventRunnerSpawned,
	event.EventRunnerCompleted,
	event.EventRunnerFailed,
	event.EventJobStarted,
	event.EventJobCompleted,
	event.EventScaleDecision,
	event.EventError,
}

// Model is the bubbletea model for the TUI dashboard.
// It attaches to a running daemon via the control socket and polls
// the JSONL event store for live updates.
type Model struct {
	store      *event.FileStore
	remoteAddr string                 // TCP address for remote daemon; empty = local Unix socket
	clientOpts []control.ClientOption // TLS options for remote connections

	events   []event.Event
	lastTime time.Time // timestamp of last event seen, for polling new ones
	width    int
	height   int
	showHelp bool

	// Live status from daemon
	liveStatus *control.LiveStatusResult
	daemonErr  string // non-empty if daemon unreachable

	startTime time.Time
	runners   map[string]*RunnerState

	// Repo filter (issue #11)
	filterRepo  string // active repo filter; empty = show all
	filterMode  bool   // true when repo-filter input is active
	filterInput string // partial input while typing

	// Event type filter (issue #12)
	hiddenEventTypes map[event.EventType]bool // true = hidden
	typeFilterMode   bool                     // true when type-filter overlay is shown
	typeFilterCursor int                      // cursor position in allEventTypes

	// Search (issue #13)
	searchMode  bool   // true when '/' search input is active
	searchQuery string // confirmed search query
	searchInput string // partial input while typing

	// Event selection and detail (issue #14)
	selectedEvent     int  // index into visible events (-1 = none selected)
	expandedEvent     bool // true when detail overlay is shown
	eventScrollOffset int  // manual scroll position (-1 = auto-scroll to bottom)
}

// New creates a new TUI model that attaches to a running daemon.
// If remoteAddr is non-empty, the TUI connects via TCP instead of the
// local Unix socket.
func New(store *event.FileStore, remoteAddr string, clientOpts ...control.ClientOption) Model {
	m := Model{
		store:             store,
		remoteAddr:        remoteAddr,
		clientOpts:        clientOpts,
		events:            make([]event.Event, 0, maxEvents),
		width:             80,
		height:            24,
		startTime:         time.Now(),
		runners:           make(map[string]*RunnerState),
		hiddenEventTypes:  make(map[event.EventType]bool),
		selectedEvent:     -1,
		eventScrollOffset: -1,
	}

	// Load events since daemon started (look for most recent daemon.started event)
	if store != nil {
		filter := event.StoreFilter{
			Since: time.Now().Add(-24 * time.Hour), // look back up to 24h
		}
		if history, err := store.Query(filter); err == nil {
			// Find the most recent daemon.started event
			startIdx := 0
			for i := len(history) - 1; i >= 0; i-- {
				if history[i].Type == event.EventDaemonStarted {
					startIdx = i
					break
				}
			}
			// Load events from daemon start onwards
			for _, e := range history[startIdx:] {
				if len(m.events) >= maxEvents {
					m.events = m.events[1:]
				}
				m.events = append(m.events, e)
				m.updateRunnerState(e)
				if e.Time.After(m.lastTime) {
					m.lastTime = e.Time
				}
			}
		}
	}

	return m
}

// Init starts the periodic refresh ticker and kicks off the first async fetch.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.fetchLiveStatusCmd,
		m.fetchEventsCmd,
		m.tickCmd(),
	)
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Handle type filter mode input first
		if m.typeFilterMode {
			switch msg.String() {
			case "esc":
				m.typeFilterMode = false
			case "up", "k":
				if m.typeFilterCursor > 0 {
					m.typeFilterCursor--
				}
			case "down", "j":
				if m.typeFilterCursor < len(allEventTypes)-1 {
					m.typeFilterCursor++
				}
			case " ", "enter":
				et := allEventTypes[m.typeFilterCursor]
				m.hiddenEventTypes[et] = !m.hiddenEventTypes[et]
			}
			return m, nil
		}

		// Handle search input mode
		if m.searchMode {
			switch msg.String() {
			case "esc":
				m.searchMode = false
				m.searchInput = ""
			case "enter":
				m.searchQuery = m.searchInput
				m.searchMode = false
				m.searchInput = ""
				// Jump to first match if there is one
				if m.searchQuery != "" {
					visible := m.visibleEvents()
					for i, e := range visible {
						if eventMatchesSearch(e, m.searchQuery) {
							m.selectedEvent = i
							m.eventScrollOffset = max(i-5, 0)
							break
						}
					}
				}
			case "backspace":
				if len(m.searchInput) > 0 {
					m.searchInput = m.searchInput[:len(m.searchInput)-1]
				}
			default:
				if len(msg.String()) == 1 {
					m.searchInput += msg.String()
				}
			}
			return m, nil
		}

		// Handle repo filter input mode
		if m.filterMode {
			switch msg.String() {
			case "esc":
				m.filterMode = false
				m.filterInput = ""
			case "enter":
				m.filterRepo = m.filterInput
				m.filterMode = false
				m.filterInput = ""
			case "backspace":
				if len(m.filterInput) > 0 {
					m.filterInput = m.filterInput[:len(m.filterInput)-1]
				}
			case "tab":
				// Cycle through repos from liveStatus
				m.filterInput = m.cycleRepoFilter(m.filterInput)
			default:
				if len(msg.String()) == 1 {
					m.filterInput += msg.String()
				}
			}
			return m, nil
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "?":
			m.showHelp = !m.showHelp
			return m, nil
		case "f":
			m.filterMode = true
			m.filterInput = m.filterRepo
			return m, nil
		case "t":
			m.typeFilterMode = !m.typeFilterMode
			return m, nil
		case "/":
			m.searchMode = true
			m.searchInput = ""
			return m, nil
		case "j":
			visible := m.visibleEvents()
			if len(visible) > 0 {
				if m.selectedEvent < 0 {
					m.selectedEvent = 0
					m.eventScrollOffset = 0
				} else if m.selectedEvent < len(visible)-1 {
					m.selectedEvent++
				}
			}
			return m, nil
		case "k":
			if m.selectedEvent > 0 {
				m.selectedEvent--
			}
			return m, nil
		case "g":
			visible := m.visibleEvents()
			if len(visible) > 0 {
				m.selectedEvent = 0
				m.eventScrollOffset = 0
			}
			return m, nil
		case "G":
			m.selectedEvent = -1
			m.eventScrollOffset = -1
			return m, nil
		case "n":
			m.jumpToNextSearchMatch(1)
			return m, nil
		case "N":
			m.jumpToNextSearchMatch(-1)
			return m, nil
		case "enter":
			if m.selectedEvent >= 0 {
				m.expandedEvent = !m.expandedEvent
			}
			return m, nil
		case "esc":
			if m.expandedEvent {
				m.expandedEvent = false
				return m, nil
			}
			if m.searchQuery != "" {
				m.searchQuery = ""
				return m, nil
			}
			if m.selectedEvent >= 0 {
				m.selectedEvent = -1
				m.eventScrollOffset = -1
				return m, nil
			}
			if m.filterRepo != "" {
				m.filterRepo = ""
				return m, nil
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tickMsg:
		return m, tea.Batch(
			m.fetchLiveStatusCmd,
			m.fetchEventsCmd,
			m.tickCmd(),
		)

	case liveStatusMsg:
		if msg.err != nil {
			m.daemonErr = "daemon not running"
			m.liveStatus = nil
		} else {
			m.daemonErr = ""
			m.liveStatus = msg.status
		}
		return m, nil

	case eventsMsg:
		if msg.err != nil || len(msg.events) == 0 {
			return m, nil
		}
		for _, e := range msg.events {
			if e.Time.Before(m.lastTime) {
				continue
			}
			// Dedup events with the same timestamp that we already ingested
			isDup := false
			for i := len(m.events) - 1; i >= 0 && m.events[i].Time.Equal(e.Time); i-- {
				if m.events[i].Type == e.Type && m.events[i].Repo == e.Repo {
					isDup = true
					break
				}
			}
			if isDup {
				continue
			}
			if len(m.events) >= maxEvents {
				m.events = m.events[1:]
			}
			m.events = append(m.events, e)
			m.updateRunnerState(e)
			if e.Time.After(m.lastTime) {
				m.lastTime = e.Time
			}
		}
		return m, nil
	}

	return m, nil
}

// fetchLiveStatusCmd is a tea.Cmd that queries the daemon asynchronously.
func (m Model) fetchLiveStatusCmd() tea.Msg {
	client, err := control.Connect(m.remoteAddr, m.clientOpts...)
	if err != nil {
		return liveStatusMsg{err: fmt.Errorf("daemon not running")}
	}
	defer func() { _ = client.Close() }()

	result, err := client.Call(context.Background(), control.MethodLiveStatus, nil)
	if err != nil {
		return liveStatusMsg{err: err}
	}

	var status control.LiveStatusResult
	if err := json.Unmarshal(result, &status); err != nil {
		return liveStatusMsg{err: err}
	}

	return liveStatusMsg{status: &status}
}

// fetchEventsCmd is a tea.Cmd that polls for new events asynchronously.
func (m Model) fetchEventsCmd() tea.Msg {
	var newEvents []event.Event
	var err error

	if m.remoteAddr != "" {
		newEvents, err = m.pollRemoteEvents()
	} else if m.store != nil {
		newEvents, err = m.pollLocalEvents()
	}

	return eventsMsg{events: newEvents, err: err}
}

// pollLocalEvents reads from the local JSONL store.
func (m Model) pollLocalEvents() ([]event.Event, error) {
	since := m.lastTime
	if since.IsZero() {
		since = time.Now().Add(-1 * time.Hour)
	}
	return m.store.Query(event.StoreFilter{Since: since})
}

// pollRemoteEvents fetches events from the daemon via the control socket.
func (m Model) pollRemoteEvents() ([]event.Event, error) {
	client, err := control.Connect(m.remoteAddr, m.clientOpts...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	since := ""
	if !m.lastTime.IsZero() {
		since = m.lastTime.Format(time.RFC3339Nano)
	} else {
		since = time.Now().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	}

	result, err := client.Call(context.Background(), control.MethodLiveEvents, control.LiveEventsParams{
		Since: since,
	})
	if err != nil {
		return nil, err
	}

	var events []event.Event
	if err := json.Unmarshal(result, &events); err != nil {
		return nil, err
	}
	return events, nil
}

// updateRunnerState updates the runner state map from an event.
func (m *Model) updateRunnerState(e event.Event) {
	switch e.Type {
	case event.EventRunnerSpawned:
		name := payloadField(e.Payload, "name")
		if name != "" {
			m.runners[name] = &RunnerState{
				Name:      name,
				Repo:      e.Repo,
				SpawnedAt: e.Time,
				State:     "starting",
			}
		}
	case event.EventJobStarted:
		runner := payloadField(e.Payload, "runner")
		job := payloadField(e.Payload, "job")
		if rs, ok := m.runners[runner]; ok {
			rs.State = "running"
			rs.JobName = job
		}
	case event.EventJobCompleted:
		runner := payloadField(e.Payload, "runner")
		if rs, ok := m.runners[runner]; ok {
			rs.State = "completing"
			rs.JobName = ""
		}
	case event.EventRunnerCompleted, event.EventRunnerFailed:
		name := payloadField(e.Payload, "name")
		if rs, ok := m.runners[name]; ok {
			rs.State = "done"
			rs.CompletedAt = e.Time
		}
	}

	// Clean up runners that have lingered long enough
	for name, rs := range m.runners {
		if rs.State == "done" && time.Since(rs.CompletedAt) > runnerLingerDuration {
			delete(m.runners, name)
		}
	}
}

// View renders the dashboard.
func (m Model) View() string {
	if m.width == 0 {
		return "Initializing..."
	}

	frameW := m.width - 2
	innerW := max(frameW-2, 20)

	header := m.renderHeader(innerW)
	headerSep := dividerStyle.Render(strings.Repeat("─", innerW))

	leftW := innerW / 2
	rightW := innerW - leftW - 1

	headerLines := lipgloss.Height(header)
	contentH := max(m.height-headerLines-1-1-1-2, 4)

	leftPanel := m.renderLeftPanel(leftW, contentH)
	rightPanel := m.renderRightPanel(rightW, contentH)

	var vDivLines []string
	for range contentH {
		vDivLines = append(vDivLines, dividerStyle.Render("│"))
	}
	vDiv := strings.Join(vDivLines, "\n")

	content := lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, vDiv, rightPanel)
	contentSep := dividerStyle.Render(strings.Repeat("─", innerW))
	helpBar := m.renderHelpBar(innerW)

	var body string
	if m.showHelp {
		helpOverlay := m.renderHelp(innerW)
		body = lipgloss.JoinVertical(lipgloss.Left,
			header, headerSep, content, contentSep, helpOverlay, helpBar)
	} else {
		body = lipgloss.JoinVertical(lipgloss.Left,
			header, headerSep, content, contentSep, helpBar)
	}

	rendered := outerBorder.Width(frameW).Render(body)

	// Overlay the detail view on top if expanded
	if m.expandedEvent && m.selectedEvent >= 0 {
		overlay := m.renderDetailOverlay()
		rendered = m.overlayCenter(rendered, overlay)
	}

	return rendered
}

// renderHeader renders the top header bar.
func (m Model) renderHeader(w int) string {
	title := titleStyle.Render("gso")

	if m.daemonErr != "" {
		errMsg := lipgloss.NewStyle().Foreground(colorRed).Render("disconnected: " + m.daemonErr)
		return title + "  " + errMsg
	}

	var repoCount, totalRunners, maxR, avail int
	if m.liveStatus != nil {
		repoCount = len(m.liveStatus.Repos)
		for _, r := range m.liveStatus.Repos {
			totalRunners += len(r.Runners)
		}
		maxR = m.liveStatus.MaxRunners
		avail = m.liveStatus.Available
	}

	repos := headerLabelStyle.Render("Repos: ") + headerValueStyle.Render(fmt.Sprintf("%d", repoCount))
	capacity := m.renderCapacityBar(totalRunners, maxR)
	uptime := headerLabelStyle.Render("Up: ") + headerValueStyle.Render(formatDuration(time.Since(m.startTime)))
	sparkline := m.renderSparkline()
	_ = avail

	right := repos + "  " + capacity + "  " + uptime + "  " + sparkline

	titleW := lipgloss.Width(title)
	rightW := lipgloss.Width(right)
	gap := w - titleW - rightW
	if gap < 2 {
		return lipgloss.JoinVertical(lipgloss.Left, title, right)
	}
	return title + strings.Repeat(" ", gap) + right
}

// renderCapacityBar renders a visual capacity bar.
func (m Model) renderCapacityBar(used, total int) string {
	barWidth := 8
	if total == 0 {
		total = 1
	}
	filled := used * barWidth / total
	filled = min(filled, barWidth)
	empty := barWidth - filled

	bar := capacityUsedStyle.Render(strings.Repeat("▮", filled)) +
		capacityFreeStyle.Render(strings.Repeat("▯", empty))

	label := headerValueStyle.Render(fmt.Sprintf("%d/%d", used, total))
	return bar + " " + label
}

// renderSparkline renders a sparkline of job throughput.
func (m Model) renderSparkline() string {
	blocks := []rune("▁▂▃▄▅▆▇█")
	buckets := 12
	now := time.Now()

	counts := make([]int, buckets)
	totalCompleted := 0
	for _, e := range m.events {
		if e.Type != event.EventJobCompleted {
			continue
		}
		age := now.Sub(e.Time)
		if age < 0 || age >= 60*time.Minute {
			continue
		}
		idx := int(age.Minutes()) / 5
		if idx >= buckets {
			continue
		}
		counts[buckets-1-idx]++
		totalCompleted++
	}

	maxVal := 0
	for _, c := range counts {
		if c > maxVal {
			maxVal = c
		}
	}

	var spark strings.Builder
	for _, c := range counts {
		if maxVal == 0 {
			spark.WriteRune(blocks[0])
		} else {
			idx := c * (len(blocks) - 1) / maxVal
			spark.WriteRune(blocks[idx])
		}
	}

	return sparklineStyle.Render(spark.String()) + " " +
		sparklineLabelStyle.Render(fmt.Sprintf("%d/hr", totalCompleted))
}

// renderLeftPanel renders the repo table + active runners.
func (m Model) renderLeftPanel(w, h int) string {
	repoTable := m.renderRepoTable(w)
	runnerHeader := runnerSectionHeader.Render("─ Active Runners " + strings.Repeat("─", max(w-18, 0)))
	runnersContent := m.renderActiveRunners(w)

	left := lipgloss.JoinVertical(lipgloss.Left,
		repoTable,
		runnerHeader,
		runnersContent,
	)

	leftH := lipgloss.Height(left)
	if leftH < h {
		left = left + strings.Repeat("\n", h-leftH)
	} else if leftH > h {
		lines := strings.Split(left, "\n")
		if len(lines) > h {
			lines = lines[:h]
		}
		left = strings.Join(lines, "\n")
	}

	return lipgloss.NewStyle().Width(w).Render(left)
}

// renderRepoTable renders the repository status table.
func (m Model) renderRepoTable(w int) string {
	colRunners := 8
	colJobs := 5
	colDone := 5
	colQueued := 2
	// 4 single-space gaps between 5 columns
	colRepo := w - colRunners - colJobs - colDone - colQueued - 4
	colRepo = max(colRepo, 10)

	header := tableHeaderStyle.Render(
		padRight("REPO", colRepo) + " " +
			padLeft("RUNNERS", colRunners) + " " +
			padLeft("JOBS", colJobs) + " " +
			padLeft("DONE", colDone) + " " +
			padLeft("Q", colQueued))

	var rows []string
	rows = append(rows, header)

	if m.liveStatus != nil {
		repos := make([]control.RepoLiveStatus, len(m.liveStatus.Repos))
		copy(repos, m.liveStatus.Repos)
		sort.Slice(repos, func(i, j int) bool { return repos[i].Repo < repos[j].Repo })

		for _, r := range repos {
			runnerCount := m.countActiveRunners(r.Repo)
			jobCount := m.countActiveJobs(r.Repo)
			doneCount := m.countCompletedJobs(r.Repo)
			queuedCount := m.countQueuedJobs(r.Repo)

			displayName := smartTruncateRepo(r.Repo, colRepo)
			// Pad AFTER styling to account for ANSI escape code length
			styledName := repoNameStyle.Render(displayName)
			paddedName := styledName + strings.Repeat(" ", colRepo-len(displayName))

			row := paddedName + " " +
				padLeft(fmt.Sprintf("%d", runnerCount), colRunners) + " " +
				padLeft(fmt.Sprintf("%d", jobCount), colJobs) + " " +
				padLeft(fmt.Sprintf("%d", doneCount), colDone) + " " +
				padLeft(fmt.Sprintf("%d", queuedCount), colQueued)
			rows = append(rows, row)
		}
	}

	if m.liveStatus == nil || len(m.liveStatus.Repos) == 0 {
		rows = append(rows, " "+lipgloss.NewStyle().Foreground(colorDim).Render("(no repos active)"))
	}

	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// renderActiveRunners renders the list of active runners.
func (m Model) renderActiveRunners(w int) string {
	if len(m.runners) == 0 {
		return " " + lipgloss.NewStyle().Foreground(colorDim).Render("(no active runners)")
	}

	names := make([]string, 0, len(m.runners))
	for name := range m.runners {
		names = append(names, name)
	}
	sort.Strings(names)

	var lines []string
	for _, name := range names {
		rs := m.runners[name]
		displayName := truncate(name, w/2)
		repoShort := naming.RepoShortName(rs.Repo)

		nameLine := " " + runnerNameStyle.Render(displayName) + "  " + runnerRepoStyle.Render(repoShort)

		var dur time.Duration
		if rs.State == "done" && !rs.CompletedAt.IsZero() {
			dur = rs.CompletedAt.Sub(rs.SpawnedAt)
		} else {
			dur = time.Since(rs.SpawnedAt)
		}
		durStyled := styleDuration(dur, formatRunnerDuration(dur))

		var stateStr string
		switch rs.State {
		case "starting":
			stateStr = stateStarting.Render("starting..")
		case "running":
			jobDisplay := truncate(rs.JobName, w/3)
			stateStr = stateRunning.Render("running: " + jobDisplay)
		case "completing":
			stateStr = stateCompleting.Render("completing..")
		case "done":
			stateStr = stateDone.Render("done")
		default:
			stateStr = rs.State
		}

		var detailLine string
		if rs.State == "done" {
			// Dim the entire detail line for done runners
			durDimmed := stateDone.Render(formatRunnerDuration(dur))
			detailLine = "   " + durDimmed + "  " + stateStr
		} else {
			detailLine = "   " + durStyled + "  " + stateStr
		}
		lines = append(lines, nameLine, detailLine)
	}

	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

// renderRightPanel renders the event log panel.
func (m Model) renderRightPanel(w, h int) string {
	headerText := "Events"
	if m.filterRepo != "" {
		headerText += " " + filterIndicatorStyle.Render("[repo:"+m.filterRepo+"]")
	}
	if m.searchQuery != "" {
		headerText += " " + filterIndicatorStyle.Render("[search:\""+m.searchQuery+"\"]")
	}
	hiddenCount := 0
	for _, hidden := range m.hiddenEventTypes {
		if hidden {
			hiddenCount++
		}
	}
	if hiddenCount > 0 {
		headerText += " " + filterIndicatorStyle.Render(fmt.Sprintf("[%d type(s) hidden]", hiddenCount))
	}
	header := " " + eventHeaderStyle.Render(headerText)

	// If type filter overlay is active, render it instead of events
	if m.typeFilterMode {
		return m.renderTypeFilterOverlay(w, h, header)
	}

	// Reserve lines for prompts
	promptLines := 0
	if m.filterMode {
		promptLines++
	}
	if m.searchMode {
		promptLines++
	}

	maxLines := h - 1 - promptLines
	maxLines = max(maxLines, 1)

	visible := m.visibleEvents()

	// Clamp selectedEvent
	if m.selectedEvent >= len(visible) {
		m.selectedEvent = len(visible) - 1
	}

	// Calculate view window
	startIdx := 0
	if m.eventScrollOffset >= 0 && m.selectedEvent >= 0 {
		// Manual scroll: keep selected event visible
		startIdx = m.eventScrollOffset
		if m.selectedEvent < startIdx {
			startIdx = m.selectedEvent
		}
		if m.selectedEvent >= startIdx+maxLines {
			startIdx = m.selectedEvent - maxLines + 1
		}
		startIdx = max(startIdx, 0)
	} else {
		// Auto-scroll to bottom
		if len(visible) > maxLines {
			startIdx = len(visible) - maxLines
		}
	}
	// Update scroll offset for consistency
	if m.selectedEvent >= 0 {
		m.eventScrollOffset = startIdx
	}

	endIdx := min(startIdx+maxLines, len(visible))

	var lines []string
	lines = append(lines, header)

	if len(visible) == 0 {
		lines = append(lines, " "+lipgloss.NewStyle().Foreground(colorDim).Render("(waiting for events...)"))
	} else {
		for i := startIdx; i < endIdx; i++ {
			line := m.renderEventLine(visible[i], w)
			isSelected := i == m.selectedEvent
			isMatch := m.searchQuery != "" && eventMatchesSearch(visible[i], m.searchQuery)

			if isSelected {
				// Strip ANSI and re-render with selection background
				line = selectedEventStyle.Width(w).Render(lipgloss.PlaceHorizontal(w, lipgloss.Left, line))
			}
			if isMatch && !isSelected {
				// Add a subtle indicator for search matches
				line = " " + searchMatchStyle.Render(">") + line[2:]
			}

			lines = append(lines, line)
		}
	}

	// Show filter input prompt
	if m.filterMode {
		lines = append(lines, " "+filterPromptStyle.Render("Filter repo: ")+m.filterInput+filterCursorStyle.Render("_"))
	}
	// Show search input prompt
	if m.searchMode {
		lines = append(lines, " "+searchPromptStyle.Render("/ ")+m.searchInput+filterCursorStyle.Render("_"))
	}

	content := lipgloss.JoinVertical(lipgloss.Left, lines...)
	contentH := lipgloss.Height(content)
	if contentH < h {
		content = content + strings.Repeat("\n", h-contentH)
	} else if contentH > h {
		ls := strings.Split(content, "\n")
		if len(ls) > h {
			ls = ls[:h]
		}
		content = strings.Join(ls, "\n")
	}

	return lipgloss.NewStyle().Width(w).Render(content)
}

// renderEventLine renders a single event line, truncated to fit width w.
func (m Model) renderEventLine(e event.Event, w int) string {
	ts := e.Time.Format("15:04:05")

	repo := ""
	if e.Repo != "" {
		repo = naming.RepoShortName(e.Repo)
	}
	if len(repo) > 12 {
		repo = repo[:12]
	}

	payload := summarizePayload(e)
	payloadMax := w - 43
	payloadMax = max(payloadMax, 0)
	payload = truncate(payload, payloadMax)

	return fmt.Sprintf(" %s  %-16s  %-12s  %s",
		eventTimeStyle.Render(ts),
		styleEventType(e),
		eventRepoStyle.Render(repo),
		eventPayloadStyle.Render(payload))
}

// styleEventType returns the styled event type string.
func styleEventType(e event.Event) string {
	t := string(e.Type)
	switch {
	case e.Type == event.EventJobCompleted && isSucceeded(e):
		return eventTypeJobSucceeded.Render(t)
	case e.Type == event.EventRunnerFailed || e.Type == event.EventError:
		return eventTypeError.Render(t)
	case e.Type == event.EventJobStarted:
		return eventTypeJobStarted.Render(t)
	default:
		return eventTypeDefault.Render(t)
	}
}

func isSucceeded(e event.Event) bool {
	result := payloadField(e.Payload, "result")
	return strings.Contains(strings.ToLower(result), "succeed") ||
		strings.Contains(strings.ToLower(result), "success")
}

// renderHelpBar renders the bottom help bar.
func (m Model) renderHelpBar(_ int) string {
	return helpBarStyle.Render(
		" " + helpKeyStyle.Render("q") + ": quit  " +
			helpKeyStyle.Render("f") + ": filter repo  " +
			helpKeyStyle.Render("t") + ": filter types  " +
			helpKeyStyle.Render("/") + ": search  " +
			helpKeyStyle.Render("j/k") + ": select  " +
			helpKeyStyle.Render("enter") + ": detail  " +
			helpKeyStyle.Render("?") + ": help")
}

// renderHelp renders the expanded help overlay.
func (m Model) renderHelp(_ int) string {
	lines := []string{
		"",
		" " + helpKeyStyle.Render("q") + "       quit the TUI (daemon keeps running)",
		" " + helpKeyStyle.Render("?") + "       toggle this help panel",
		" " + helpKeyStyle.Render("f") + "       filter events by repo (type name, tab to cycle, enter to apply)",
		" " + helpKeyStyle.Render("t") + "       toggle event type visibility (j/k to move, space to toggle)",
		" " + helpKeyStyle.Render("/") + "       search events (type query, enter to confirm)",
		" " + helpKeyStyle.Render("j/k") + "     select event down/up",
		" " + helpKeyStyle.Render("g/G") + "     jump to top/bottom of event list",
		" " + helpKeyStyle.Render("n/N") + "     jump to next/previous search match",
		" " + helpKeyStyle.Render("enter") + "   expand selected event detail",
		" " + helpKeyStyle.Render("esc") + "     close detail / clear search / clear selection / clear filter",
		" " + helpKeyStyle.Render("ctrl+c") + "  force quit",
		"",
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

// renderTypeFilterOverlay renders the event type toggle overlay.
func (m Model) renderTypeFilterOverlay(w, h int, header string) string {
	var lines []string
	lines = append(lines, header)
	lines = append(lines, " "+filterPromptStyle.Render("Toggle event types (space=toggle, esc=close):"))
	lines = append(lines, "")

	for i, et := range allEventTypes {
		check := "  [x] "
		if m.hiddenEventTypes[et] {
			check = "  [ ] "
		}
		cursor := "  "
		if i == m.typeFilterCursor {
			cursor = "> "
		}
		style := eventTypeDefault
		if i == m.typeFilterCursor {
			style = lipgloss.NewStyle().Foreground(colorWhite).Bold(true)
		}
		lines = append(lines, cursor+check+style.Render(string(et)))
	}

	content := lipgloss.JoinVertical(lipgloss.Left, lines...)
	contentH := lipgloss.Height(content)
	if contentH < h {
		content = content + strings.Repeat("\n", h-contentH)
	} else if contentH > h {
		ls := strings.Split(content, "\n")
		if len(ls) > h {
			ls = ls[:h]
		}
		content = strings.Join(ls, "\n")
	}

	return lipgloss.NewStyle().Width(w).Render(content)
}

// cycleRepoFilter cycles through available repos matching the current input.
func (m Model) cycleRepoFilter(current string) string {
	if m.liveStatus == nil || len(m.liveStatus.Repos) == 0 {
		return current
	}

	var repos []string
	for _, r := range m.liveStatus.Repos {
		repos = append(repos, r.Repo)
	}
	sort.Strings(repos)

	// Filter to repos matching current prefix
	var matching []string
	for _, r := range repos {
		if current == "" || strings.Contains(r, current) {
			matching = append(matching, r)
		}
	}
	if len(matching) == 0 {
		return current
	}

	// Find current in matching and advance
	for i, r := range matching {
		if r == current {
			return matching[(i+1)%len(matching)]
		}
	}
	return matching[0]
}

// countCompletedJobs counts job.completed events for a given repo.
func (m Model) countCompletedJobs(repo string) int {
	count := 0
	for _, e := range m.events {
		if e.Type == event.EventJobCompleted && e.Repo == repo {
			count++
		}
	}
	return count
}

// visibleEvents returns events filtered by repo and type filters.
func (m Model) visibleEvents() []event.Event {
	var visible []event.Event
	for _, e := range m.events {
		if m.filterRepo != "" && !strings.Contains(e.Repo, m.filterRepo) {
			continue
		}
		if m.hiddenEventTypes[e.Type] {
			continue
		}
		visible = append(visible, e)
	}
	return visible
}

// eventMatchesSearch checks if an event matches the search query (case-insensitive).
func eventMatchesSearch(e event.Event, query string) bool {
	if query == "" {
		return false
	}
	q := strings.ToLower(query)
	if strings.Contains(strings.ToLower(string(e.Type)), q) {
		return true
	}
	if strings.Contains(strings.ToLower(e.Repo), q) {
		return true
	}
	if len(e.Payload) > 0 && strings.Contains(strings.ToLower(string(e.Payload)), q) {
		return true
	}
	return false
}

// jumpToNextSearchMatch jumps to the next (direction=1) or previous (direction=-1) search match.
func (m *Model) jumpToNextSearchMatch(direction int) {
	if m.searchQuery == "" {
		return
	}
	visible := m.visibleEvents()
	if len(visible) == 0 {
		return
	}

	start := m.selectedEvent
	if start < 0 {
		if direction > 0 {
			start = -1
		} else {
			start = len(visible)
		}
	}

	for i := 1; i <= len(visible); i++ {
		idx := (start + direction*i) % len(visible)
		if idx < 0 {
			idx += len(visible)
		}
		if eventMatchesSearch(visible[idx], m.searchQuery) {
			m.selectedEvent = idx
			m.eventScrollOffset = max(idx-5, 0)
			return
		}
	}
}

// renderDetailOverlay renders the event detail modal.
func (m Model) renderDetailOverlay() string {
	visible := m.visibleEvents()
	if m.selectedEvent < 0 || m.selectedEvent >= len(visible) {
		return ""
	}
	e := visible[m.selectedEvent]

	maxW := min(m.width-10, 80)
	maxW = max(maxW, 30)

	var lines []string
	lines = append(lines, detailLabelStyle.Render("Time:  ")+detailValueStyle.Render(e.Time.Format("2006-01-02 15:04:05.000")))
	lines = append(lines, detailLabelStyle.Render("Type:  ")+detailValueStyle.Render(string(e.Type)))
	if e.Repo != "" {
		lines = append(lines, detailLabelStyle.Render("Repo:  ")+detailValueStyle.Render(e.Repo))
	}

	if len(e.Payload) > 0 {
		lines = append(lines, "")
		lines = append(lines, detailLabelStyle.Render("Payload:"))
		var prettyJSON json.RawMessage
		if err := json.Unmarshal(e.Payload, &prettyJSON); err == nil {
			formatted, err := json.MarshalIndent(prettyJSON, "", "  ")
			if err == nil {
				payloadLines := strings.Split(string(formatted), "\n")
				maxPayloadLines := min(m.height-12, 20)
				maxPayloadLines = max(maxPayloadLines, 5)
				for i, pl := range payloadLines {
					if i >= maxPayloadLines {
						lines = append(lines, detailValueStyle.Render("  ..."))
						break
					}
					lines = append(lines, detailValueStyle.Render("  "+pl))
				}
			}
		}
	}

	lines = append(lines, "")
	lines = append(lines, helpBarStyle.Render("esc: close  enter: close"))

	content := lipgloss.JoinVertical(lipgloss.Left, lines...)
	return detailBorder.Width(maxW).Render(content)
}

// overlayCenter places an overlay string centered on the base string.
func (m Model) overlayCenter(base, overlay string) string {
	baseLines := strings.Split(base, "\n")
	overlayLines := strings.Split(overlay, "\n")

	baseH := len(baseLines)
	overlayH := len(overlayLines)
	overlayW := lipgloss.Width(overlay)

	startRow := max((baseH-overlayH)/2, 0)
	startCol := max((m.width-overlayW)/2, 0)

	for i, oLine := range overlayLines {
		row := startRow + i
		if row >= baseH {
			break
		}
		baseLine := baseLines[row]
		// Pad base line if needed
		baseRuneWidth := lipgloss.Width(baseLine)
		if baseRuneWidth < startCol+lipgloss.Width(oLine) {
			baseLine = baseLine + strings.Repeat(" ", startCol+lipgloss.Width(oLine)-baseRuneWidth)
		}

		// Simple overlay: replace characters at position
		// We work with the raw string for simplicity
		prefix := ""
		if startCol > 0 && baseRuneWidth >= startCol {
			prefix = truncateToWidth(baseLine, startCol)
		} else {
			prefix = strings.Repeat(" ", startCol)
		}
		suffix := ""
		afterOverlay := startCol + lipgloss.Width(oLine)
		if afterOverlay < baseRuneWidth {
			suffix = substringFromWidth(baseLine, afterOverlay)
		}
		baseLines[row] = prefix + oLine + suffix
	}

	return strings.Join(baseLines, "\n")
}

// truncateToWidth returns the prefix of s that fits in maxWidth visual columns.
func truncateToWidth(s string, maxWidth int) string {
	w := 0
	for i, r := range s {
		rw := 1
		_ = r
		w += rw
		if w > maxWidth {
			return s[:i]
		}
	}
	return s
}

// substringFromWidth returns the suffix of s starting from the given visual column.
func substringFromWidth(s string, fromWidth int) string {
	w := 0
	for i, r := range s {
		rw := 1
		_ = r
		w += rw
		if w > fromWidth {
			return s[i:]
		}
	}
	return ""
}

// ---------- helpers ----------

func (m Model) countActiveRunners(repo string) int {
	count := 0
	for _, rs := range m.runners {
		if rs.Repo == repo && rs.State != "done" {
			count++
		}
	}
	return count
}

func (m Model) countActiveJobs(repo string) int {
	active := make(map[string]bool)
	for _, e := range m.events {
		if e.Repo != repo {
			continue
		}
		switch e.Type {
		case event.EventJobStarted:
			runner := payloadField(e.Payload, "runner")
			if runner != "" {
				active[runner] = true
			}
		case event.EventJobCompleted:
			runner := payloadField(e.Payload, "runner")
			delete(active, runner)
		case event.EventRunnerCompleted, event.EventRunnerFailed:
			name := payloadField(e.Payload, "name")
			delete(active, name)
		}
	}
	return len(active)
}

func (m Model) countQueuedJobs(repo string) int {
	for i := len(m.events) - 1; i >= 0; i-- {
		e := m.events[i]
		if e.Type == event.EventScaleDecision && e.Repo == repo {
			desired := payloadField(e.Payload, "desired")
			if desired != "" {
				var d int
				if _, err := fmt.Sscanf(desired, "%d", &d); err == nil {
					active := m.countActiveJobs(repo)
					return max(d-active, 0)
				}
			}
		}
	}
	return 0
}

func (m Model) tickCmd() tea.Cmd {
	return tea.Tick(tickRefresh, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func summarizePayload(e event.Event) string {
	if len(e.Payload) == 0 {
		return ""
	}
	switch e.Type {
	case event.EventRunnerSpawned, event.EventRunnerCompleted:
		return truncate(payloadField(e.Payload, "name"), 30)
	case event.EventRunnerFailed:
		name := payloadField(e.Payload, "name")
		errMsg := payloadField(e.Payload, "error")
		return truncate(fmt.Sprintf("%s: %s", name, errMsg), 40)
	case event.EventJobStarted:
		return truncate(payloadField(e.Payload, "job"), 30)
	case event.EventJobCompleted:
		job := payloadField(e.Payload, "job")
		result := payloadField(e.Payload, "result")
		return truncate(fmt.Sprintf("%s %s", job, result), 40)
	case event.EventScaleSetCreated:
		return truncate(payloadField(e.Payload, "name"), 30)
	default:
		return truncate(string(e.Payload), 40)
	}
}

func payloadField(payload json.RawMessage, key string) string {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s[:width]
	}
	return s + strings.Repeat(" ", width-len(s))
}

func padLeft(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}

func smartTruncateRepo(repo string, maxLen int) string {
	if len(repo) <= maxLen {
		return repo
	}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return truncate(repo, maxLen)
	}
	owner, name := parts[0], parts[1]

	// Try "o.../name"
	medium := owner[:1] + ".../" + name
	if len(medium) <= maxLen {
		return medium
	}

	// Just use the repo name
	return truncate(name, maxLen)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		if maxLen < 0 {
			return ""
		}
		return s[:maxLen]
	}
	return s[:maxLen-2] + ".."
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

func formatRunnerDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

func styleDuration(d time.Duration, s string) string {
	switch {
	case d < 5*time.Minute:
		return durationGreen.Render(s)
	case d < 15*time.Minute:
		return durationYellow.Render(s)
	default:
		return durationRed.Render(s)
	}
}
