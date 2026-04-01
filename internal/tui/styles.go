package tui

import "github.com/charmbracelet/lipgloss"

var (
	// Colors
	colorCyan    = lipgloss.Color("6")
	colorGreen   = lipgloss.Color("2")
	colorRed     = lipgloss.Color("1")
	colorYellow  = lipgloss.Color("3")
	colorDim     = lipgloss.Color("8")
	colorSubtle  = lipgloss.Color("241")
	colorBorder  = lipgloss.Color("238")
	colorWhite   = lipgloss.Color("15")
	colorMagenta = lipgloss.Color("5")

	// Outer frame — rounded border
	outerBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder)

	// Title in header bar
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWhite)

	// Header bar (capacity, uptime, sparkline)
	headerValueStyle = lipgloss.NewStyle().
				Bold(true)

	headerLabelStyle = lipgloss.NewStyle().
				Foreground(colorSubtle)

	// Capacity bar characters
	capacityUsedStyle = lipgloss.NewStyle().
				Foreground(colorGreen)

	capacityFreeStyle = lipgloss.NewStyle().
				Foreground(colorDim)

	// Repo table header
	tableHeaderStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorSubtle)

	// Repo name in table
	repoNameStyle = lipgloss.NewStyle().
			Foreground(colorCyan)

	// Active runner section header
	runnerSectionHeader = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorSubtle)

	// Runner name
	runnerNameStyle = lipgloss.NewStyle().
			Foreground(colorMagenta)

	// Runner repo
	runnerRepoStyle = lipgloss.NewStyle().
			Foreground(colorDim)

	// Runner duration colors
	durationGreen = lipgloss.NewStyle().
			Foreground(colorGreen)

	durationYellow = lipgloss.NewStyle().
			Foreground(colorYellow)

	durationRed = lipgloss.NewStyle().
			Foreground(colorRed)

	// Runner state
	stateStarting = lipgloss.NewStyle().
			Foreground(colorYellow)

	stateRunning = lipgloss.NewStyle().
			Foreground(colorGreen)

	stateCompleting = lipgloss.NewStyle().
			Foreground(colorDim)

	// Event log header
	eventHeaderStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorSubtle)

	// Event timestamp
	eventTimeStyle = lipgloss.NewStyle().
			Foreground(colorDim)

	// Event types by category
	eventTypeDefault = lipgloss.NewStyle().
				Foreground(colorDim)

	eventTypeJobStarted = lipgloss.NewStyle().
				Foreground(colorCyan)

	eventTypeJobSucceeded = lipgloss.NewStyle().
				Foreground(colorGreen)

	eventTypeError = lipgloss.NewStyle().
			Foreground(colorRed)

	eventRepoStyle = lipgloss.NewStyle().
			Foreground(colorCyan)

	eventPayloadStyle = lipgloss.NewStyle().
				Foreground(colorSubtle)

	// Help bar
	helpBarStyle = lipgloss.NewStyle().
			Foreground(colorDim)

	helpKeyStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorYellow)

	// Filter indicator in event header
	filterIndicatorStyle = lipgloss.NewStyle().
				Foreground(colorYellow)

	// Filter input prompt
	filterPromptStyle = lipgloss.NewStyle().
				Foreground(colorCyan).
				Bold(true)

	// Filter cursor blink
	filterCursorStyle = lipgloss.NewStyle().
				Foreground(colorWhite).
				Bold(true)

	// Divider line
	dividerStyle = lipgloss.NewStyle().
			Foreground(colorBorder)

	// Sparkline
	sparklineStyle = lipgloss.NewStyle().
			Foreground(colorGreen)

	sparklineLabelStyle = lipgloss.NewStyle().
				Foreground(colorDim)

	// Search prompt (reuses filter prompt cyan+bold)
	searchPromptStyle = lipgloss.NewStyle().
				Foreground(colorCyan).
				Bold(true)

	// Search match highlight
	searchMatchStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorYellow)

	// Selected event indicator
	selectedEventStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("236"))

	// Detail overlay border
	detailBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorCyan).
			Padding(1, 2)

	// Detail field labels
	detailLabelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorCyan)

	// Detail field values
	detailValueStyle = lipgloss.NewStyle().
				Foreground(colorWhite)
)
